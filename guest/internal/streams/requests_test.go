package streams

import (
	"bufio"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	api "fev/proto/agent"
	"fev/proto/relay"
)

type service struct {
	r     *Requests
	path  string
	asked chan api.StreamRequest
	relay *Relay
	mu    sync.Mutex
	host  bool
}

func newService(t *testing.T, wait time.Duration) *service {
	t.Helper()
	s := &service{path: filepath.Join(shortDir(t), "agent.sock"), asked: make(chan api.StreamRequest, 512), host: true}
	s.r = &Requests{Logf: t.Logf, Wait: wait, Ask: func(req api.StreamRequest) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.host {
			s.asked <- req
		}
		return s.host
	}}
	s.relay = &Relay{Stream: s.r.Attach}
	l, err := net.Listen("unix", s.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go s.r.Serve(l, api.StreamSSHAgent)
	return s
}

func (s *service) dial(t *testing.T) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", s.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func closed(c net.Conn, within time.Duration) bool {
	c.SetReadDeadline(time.Now().Add(within))
	_, err := c.Read(make([]byte, 1))
	return err == io.EOF
}

func TestARequestedStreamIsAttachedThroughTheRelay(t *testing.T) {
	s := newService(t, 0)
	guest := s.dial(t)
	req := <-s.asked
	if req.Service != api.StreamSSHAgent || len(req.Token) != relay.TokenLen || strings.ToLower(req.Token) != req.Token {
		t.Fatalf("request %+v", req)
	}
	host := serve(t, s.relay)
	if err := relay.Open(host, relay.Request{Kind: relay.Stream, Target: req.Token}); err != nil {
		t.Fatal(err)
	}
	io.WriteString(guest, "sign this\n")
	line, _ := bufio.NewReader(host).ReadString('\n')
	if line != "sign this\n" {
		t.Fatalf("the host read %q", line)
	}
	io.WriteString(host, "signed\n")
	host.CloseWrite()
	got, _ := io.ReadAll(guest)
	if string(got) != "signed\n" {
		t.Fatalf("the guest read %q", got)
	}
	again := serve(t, s.relay)
	if err := relay.Open(again, relay.Request{Kind: relay.Stream, Target: req.Token}); err == nil {
		t.Fatal("a token was accepted twice")
	}
}

func TestARefusedRequestClosesTheGuestsConnection(t *testing.T) {
	s := newService(t, 0)
	guest := s.dial(t)
	req := <-s.asked
	s.r.Refuse(req.Token, "SSH_AUTH_SOCK is not set")
	if !closed(guest, 2*time.Second) {
		t.Fatal("the guest's connection stayed open after a refusal")
	}
	s.r.Refuse(req.Token, "")
	host := serve(t, s.relay)
	if err := relay.Open(host, relay.Request{Kind: relay.Stream, Target: req.Token}); err == nil {
		t.Fatal("a refused request could still be attached")
	}
}

func TestAnUnansweredRequestIsDropped(t *testing.T) {
	s := newService(t, 30*time.Millisecond)
	guest := s.dial(t)
	<-s.asked
	if !closed(guest, 2*time.Second) {
		t.Fatal("an unanswered request was kept")
	}
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if len(s.r.waiting) != 0 || s.r.total != 0 {
		t.Fatalf("%d waiting, %d in all after the deadline", len(s.r.waiting), s.r.total)
	}
}

func TestWithNoHostConnectionTheSocketClosesAtOnce(t *testing.T) {
	s := newService(t, 0)
	s.mu.Lock()
	s.host = false
	s.mu.Unlock()
	if !closed(s.dial(t), 2*time.Second) {
		t.Fatal("a connection was kept with nobody to ask")
	}
}

func TestStreamsAreBounded(t *testing.T) {
	s := newService(t, 0)
	for i := 0; i < api.MaxStreamRequests; i++ {
		s.dial(t)
		<-s.asked
	}
	if !closed(s.dial(t), 2*time.Second) {
		t.Fatalf("request %d was taken", api.MaxStreamRequests+1)
	}
	select {
	case req := <-s.asked:
		t.Fatalf("the host was asked for a request past the bound: %+v", req)
	default:
	}
	attached := 0
	attach := func(req api.StreamRequest) net.Conn {
		host, guest := pair(t)
		go s.relay.ServeConn(guest)
		if err := relay.Open(host, relay.Request{Kind: relay.Stream, Target: req.Token}); err != nil {
			t.Fatal(err)
		}
		attached++
		return host
	}
	s.r.mu.Lock()
	var tokens []string
	for token := range s.r.waiting {
		tokens = append(tokens, token)
	}
	s.r.mu.Unlock()
	for _, token := range tokens {
		attach(api.StreamRequest{Token: token})
	}
	var lastGuest, lastHost net.Conn
	for attached < api.MaxStreams {
		lastGuest = s.dial(t)
		lastHost = attach(<-s.asked)
	}
	if !closed(s.dial(t), 2*time.Second) {
		t.Fatalf("stream %d was taken", api.MaxStreams+1)
	}
	lastGuest.Close()
	lastHost.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.r.mu.Lock()
		total := s.r.total
		s.r.mu.Unlock()
		if total < api.MaxStreams {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a closed stream kept its place")
		}
		time.Sleep(time.Millisecond)
	}
	s.dial(t)
	<-s.asked
}

func TestAConnectLoopDoesNotFillTheLog(t *testing.T) {
	var mu sync.Mutex
	lines := 0
	s := newService(t, 0)
	s.r.Logf = func(string, ...any) {
		mu.Lock()
		lines++
		mu.Unlock()
	}
	s.mu.Lock()
	s.host = false
	s.mu.Unlock()
	const n = 2000
	for i := 0; i < n; i++ {
		c, err := net.Dial("unix", s.path)
		if err != nil {
			t.Fatal(err)
		}
		if !closed(c, 2*time.Second) {
			t.Fatal("a connection was kept with nobody to ask")
		}
		c.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if lines == 0 || lines > 16 {
		t.Fatalf("%d log lines for %d connections that came to nothing; at least one and at most 16", lines, n)
	}
}

func TestAConnectLoopAmongOpenedStreamsDoesNotFillTheLog(t *testing.T) {
	var mu sync.Mutex
	lines := 0
	s := newService(t, 0)
	s.r.Logf = func(string, ...any) {
		mu.Lock()
		lines++
		mu.Unlock()
	}
	for round := 0; round < 200; round++ {
		guest := s.dial(t)
		req := <-s.asked
		host, relayEnd := pair(t)
		go s.relay.ServeConn(relayEnd)
		if err := relay.Open(host, relay.Request{Kind: relay.Stream, Target: req.Token}); err != nil {
			t.Fatal(err)
		}
		guest.Close()
		host.Close()
		s.mu.Lock()
		s.host = false
		s.mu.Unlock()
		for i := 0; i < 5; i++ {
			c, err := net.Dial("unix", s.path)
			if err != nil {
				t.Fatal(err)
			}
			if !closed(c, 2*time.Second) {
				t.Fatal("a connection was kept with nobody to ask")
			}
			c.Close()
		}
		s.mu.Lock()
		s.host = true
		s.mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	if lines == 0 || lines > 16 {
		t.Fatalf("%d log lines for 1000 connections that came to nothing among 200 streams; at least one and at most 16", lines)
	}
}
