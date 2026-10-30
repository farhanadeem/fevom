package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTheFramingOfRequestsAndResponsesIsFollowed(t *testing.T) {
	var got []string
	f := framer{}
	header := func(m message, h []byte) (bool, bool) {
		got = append(got, m.method+" "+m.target)
		raw := m.upgrade || hijacks(m.method, m.target)
		return !raw, raw
	}
	done := func() { got = append(got, "end") }
	stream := "GET /_ping HTTP/1.1\r\nHost: docker\r\n\r\n" +
		"POST /v1.47/images/load HTTP/1.1\r\nContent-Length: 11\r\n\r\nhello world" +
		"POST /v1.47/build HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n5;x=y\r\nhello\r\n6\r\n world\r\n0\r\nTrailer: t\r\n\r\n" +
		"POST /v1.47/containers/abc/attach?stream=1 HTTP/1.1\r\nUpgrade: tcp\r\nConnection: Upgrade\r\n\r\nraw bytes GET / HTTP/1.1\r\n\r\n"
	for i := 0; i < len(stream); i++ {
		f.feed([]byte{stream[i]}, header, done)
	}
	want := "GET /_ping, end, POST /v1.47/images/load, end, POST /v1.47/build, end, POST /v1.47/containers/abc/attach?stream=1"
	if s := strings.Join(got, ", "); s != want {
		t.Fatalf("requests:\n%s\nwant\n%s", s, want)
	}
	if f.phase != inRaw {
		t.Fatalf("an upgraded connection is still followed (%d)", f.phase)
	}

	got = nil
	r := framer{response: true}
	resp := func(m message, h []byte) (bool, bool) {
		got = append(got, fmt.Sprint(m.status))
		return m.status != 204 && m.status != 100, m.status == 101
	}
	r.feed([]byte("HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nabcHTTP/1.1 204 No Content\r\n\r\n"+
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\r\n\r\nHTTP/1.1 200 OK\r\nContent-Type: x\r\n\r\nto the end"), resp, done)
	if s := strings.Join(got, ", "); s != "100, end, 200, end, 204, end, 200, end, 200" || r.phase != inClose {
		t.Fatalf("responses: %s (%d)", s, r.phase)
	}
	big := framer{}
	big.feed([]byte("GET / HTTP/1.1\r\nX: "+strings.Repeat("a", maxHeader+1)), header, done)
	if big.phase != inRaw {
		t.Fatal("an endless header is still followed")
	}
}

func TestEachRequestIsClassedByWhatItReachesInTheEngine(t *testing.T) {
	for _, c := range []struct {
		method, target, kind string
		cls                  class
	}{
		{"POST", "/v1.47/images/create?fromImage=alpine&tag=3", "a pull", work},
		{"POST", "/images/create?fromSrc=-&repo=x", "an image import", work},
		{"POST", "/v1.47/images/load?quiet=1", "an image load", work},
		{"GET", "/v1.47/images/get?names=alpine", "an image save", work},
		{"GET", "/v1.47/images/alpine:3/get", "an image save", work},
		{"GET", "/v1.47/images/ghcr.io/me/app:1/get", "an image save", work},
		{"POST", "/v1.47/images/localhost:5000/web/push?tag=dev", "a push", work},
		{"GET", "/v1.47/containers/abc/export", "a container export", work},
		{"POST", "/v1.47/commit?container=abc&repo=x", "a commit", work},
		{"POST", "/v1.47/build?t=x", "a build", work},
		{"POST", "/session", "a build", upgraded},
		{"POST", "/grpc", "a build", upgraded},
		{"PUT", "/v1.47/containers/abc/archive?path=/", "a container copy", work},
		{"GET", "/v1.47/containers/abc/archive?path=/", "a container copy", work},
		{"HEAD", "/v1.47/containers/abc/archive?path=/", "a container copy", work},
		{"POST", "/v1.47/containers/create", "a Docker request", work},
		{"POST", "/v1.47/containers/abc/start", "a Docker request", work},
		{"DELETE", "/v1.47/containers/abc?force=1", "a Docker request", work},
		{"DELETE", "/v1.47/images/alpine", "a Docker request", work},
		{"POST", "/v1.47/containers/abc/exec", "a Docker request", work},
		{"POST", "/v1.47/exec/e/start", "a Docker request", work},
		{"POST", "/v1.47/containers/abc/wait?condition=removed", "a --rm container", removal},
		{"POST", "/v1.47/containers/abc/wait?condition=next-exit", "", stream},
		{"POST", "/v1.47/containers/abc/wait", "", stream},
		{"POST", "/v1.47/containers/abc/attach?stream=1", "", stream},
		{"GET", "/v1.47/containers/abc/attach/ws", "", stream},
		{"GET", "/v1.47/events?filters=x", "", stream},
		{"GET", "/v1.47/containers/abc/logs?follow=1&stdout=1", "", stream},
		{"GET", "/v1.47/containers/abc/logs?stdout=1", "a Docker request", read},
		{"GET", "/v1.47/containers/abc/stats", "", stream},
		{"GET", "/v1.47/containers/abc/stats?stream=false", "a Docker request", read},
		{"GET", "/v1.47/images/json", "a Docker request", read},
		{"GET", "/v1.47/images/alpine/json", "a Docker request", read},
		{"HEAD", "/_ping", "a Docker request", read},
	} {
		if k, cls := classify(c.method, c.target); k != c.kind || cls != c.cls {
			t.Errorf("%s %s: %q %d, want %q %d", c.method, c.target, k, cls, c.kind, c.cls)
		}
	}
}

type fenceEngine struct {
	sock    string
	release chan struct{}
	mu      sync.Mutex
	seen    []string
	arrived chan string
}

func newFenceEngine(t *testing.T) *fenceEngine {
	e := &fenceEngine{sock: filepath.Join(shortDir(t), "e.sock"), release: make(chan struct{}), arrived: make(chan string, 64)}
	l, err := net.Listen("unix", e.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.seen = append(e.seen, r.Method+" "+r.URL.Path)
		e.mu.Unlock()
		e.arrived <- r.Method + " " + r.URL.Path
		switch {
		case strings.HasSuffix(r.URL.Path, "/images/get"):
			w.Header().Set("Content-Type", "application/x-tar")
			w.WriteHeader(200)
			for {
				w.Write([]byte(strings.Repeat("t", 512)))
				w.(http.Flusher).Flush()
				select {
				case <-e.release:
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		case r.URL.Path == "/v1.47/images/create":
			io.WriteString(w, `{"status":"done"}`)
		default:
			io.WriteString(w, "[]")
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return e
}

func (e *fenceEngine) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", e.sock)
}

func (e *fenceEngine) saw(what string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.seen {
		if s == what {
			return true
		}
	}
	return false
}

func fenceClient(p *Proxy, keepAlive bool) *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: !keepAlive,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", p.Path())
		}}, Timeout: 20 * time.Second}
}

func TestTheFenceWaitsForImageTransfersAndHoldsWhatComesDuringTheRestart(t *testing.T) {
	e := newFenceEngine(t)
	p, err := Listen(filepath.Join(shortDir(t), "docker.sock"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.Release(e.dial)

	pooled := fenceClient(p, true)
	if r, err := pooled.Get("http://docker/v1.47/images/json"); err != nil {
		t.Fatal(err)
	} else {
		io.ReadAll(r.Body)
		r.Body.Close()
	}
	saveDone := make(chan error, 1)
	go func() {
		r, err := fenceClient(p, false).Get("http://docker/v1.47/images/get?names=web")
		if err == nil {
			_, err = io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
		saveDone <- err
	}()
	if a := <-e.arrived; a != "GET /v1.47/images/json" {
		t.Fatalf("first %s", a)
	}
	if a := <-e.arrived; a != "GET /v1.47/images/get" {
		t.Fatalf("second %s", a)
	}

	var wmu sync.Mutex
	var told [][]string
	fenced := make(chan Fenced, 1)
	var release func()
	go func() {
		f, rel, err := p.Fence(context.Background(), 10*time.Second, func(left []string) {
			wmu.Lock()
			told = append(told, left)
			wmu.Unlock()
		})
		if err != nil {
			t.Error(err)
		}
		release = rel
		fenced <- f
	}()
	time.Sleep(100 * time.Millisecond)
	pullDone := make(chan error, 1)
	go func() {
		r, err := fenceClient(p, false).Post("http://docker/v1.47/images/create?fromImage=web", "", nil)
		if err == nil {
			io.ReadAll(r.Body)
			r.Body.Close()
		}
		pullDone <- err
	}()
	if r, err := pooled.Get("http://docker/v1.47/containers/json"); err != nil {
		t.Fatalf("a question during the wait: %v", err)
	} else {
		io.ReadAll(r.Body)
		r.Body.Close()
	}
	time.Sleep(100 * time.Millisecond)
	if e.saw("POST /v1.47/images/create") {
		t.Fatal("a pull reached the engine while the fence waited")
	}
	select {
	case f := <-fenced:
		t.Fatalf("the fence ended with the save in flight: %+v", f)
	default:
	}
	close(e.release)
	if err := <-saveDone; err != nil {
		t.Fatalf("the save: %v", err)
	}
	var f Fenced
	select {
	case f = <-fenced:
	case <-time.After(5 * time.Second):
		t.Fatal("the fence did not end after the save")
	}
	if len(f.Left) != 0 || f.Waited < 200*time.Millisecond {
		t.Fatalf("fenced %+v", f)
	}
	wmu.Lock()
	if len(told) == 0 || strings.Join(told[0], ",") != "an image save" {
		t.Fatalf("told %v", told)
	}
	wmu.Unlock()
	quick := make(chan error, 2)
	go func() {
		r, err := fenceClient(p, false).Get("http://docker/v1.47/info")
		if err == nil {
			r.Body.Close()
		}
		quick <- err
	}()
	go func() {
		r, err := pooled.Get("http://docker/v1.47/version")
		if err == nil {
			r.Body.Close()
		}
		quick <- err
	}()
	time.Sleep(150 * time.Millisecond)
	if e.saw("GET /v1.47/info") || e.saw("GET /v1.47/version") || e.saw("POST /v1.47/images/create") {
		t.Fatalf("reached the engine during the restart: %v", e.seen)
	}
	release()
	for i := 0; i < 2; i++ {
		if err := <-quick; err != nil {
			t.Fatalf("after the release: %v", err)
		}
	}
	if err := <-pullDone; err != nil || !e.saw("POST /v1.47/images/create") {
		t.Fatalf("the held pull: %v %v", err, e.seen)
	}
}

func TestTheFenceGoesOnAfterItsBoundAndSaysWhatIsLeft(t *testing.T) {
	e := newFenceEngine(t)
	defer close(e.release)
	p, err := Listen(filepath.Join(shortDir(t), "docker.sock"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.Release(e.dial)
	c, err := net.Dial("unix", p.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "GET /v1.47/images/get?names=x HTTP/1.1\r\nHost: docker\r\n\r\n")
	<-e.arrived
	a, err := net.Dial("unix", p.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	io.WriteString(a, "POST /v1.47/containers/abc/attach HTTP/1.1\r\nHost: docker\r\nUpgrade: tcp\r\nConnection: Upgrade\r\n\r\n")
	<-e.arrived
	go io.Copy(io.Discard, bufio.NewReader(c))
	began := time.Now()
	f, release, err := p.Fence(context.Background(), 300*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if time.Since(began) < 300*time.Millisecond || strings.Join(f.Left, ",") != "an image save" {
		t.Fatalf("fenced %+v after %v", f, time.Since(began))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, rel, err := p.Fence(ctx, time.Second, nil); err == nil {
		rel()
		t.Fatal("a cancelled fence did not say so")
	}
	if _, rel, err := p.Fence(context.Background(), time.Millisecond, nil); err != nil {
		t.Fatalf("a fence after a cancelled one: %v", err)
	} else {
		rel()
	}
}

func TestAConnectionThatEndsTakesItsTransferOutOfFlight(t *testing.T) {
	e := newFenceEngine(t)
	defer close(e.release)
	p, err := Listen(filepath.Join(shortDir(t), "docker.sock"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.Release(e.dial)
	c, err := net.Dial("unix", p.Path())
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, "GET /v1.47/images/get?names=x HTTP/1.1\r\nHost: docker\r\n\r\n")
	<-e.arrived
	go func() {
		time.Sleep(200 * time.Millisecond)
		c.Close()
	}()
	f, release, err := p.Fence(context.Background(), 5*time.Second, nil)
	release()
	if err != nil || len(f.Left) != 0 || f.Waited > 3*time.Second {
		t.Fatalf("fenced %+v %v", f, err)
	}
}
