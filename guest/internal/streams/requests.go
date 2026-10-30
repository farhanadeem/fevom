package streams

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"time"

	api "fev/proto/agent"
	"fev/proto/relay"
)

type Requests struct {
	Ask  func(api.StreamRequest) bool
	Logf func(format string, args ...any)
	Wait time.Duration

	mu      sync.Mutex
	waiting map[string]*request
	total   int
	nothing uint64
	since   time.Time
}

const logWindow = 10 * time.Minute

type request struct {
	conn    net.Conn
	service string
	head    []byte
	say     func(conn net.Conn, why string)
	timer   *time.Timer
}

const requestWait = 10 * time.Second

func (r *Requests) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Requests) failed(format string, args ...any) {
	r.mu.Lock()
	r.nothing++
	n := r.nothing
	r.mu.Unlock()
	if n&(n-1) != 0 {
		return
	}
	if n > 1 {
		format += fmt.Sprintf(" (%d such connections of late)", n)
	}
	r.logf(format, args...)
}

func (r *Requests) Serve(l net.Listener, service string) error {
	var wait acceptWait
	for {
		conn, err := l.Accept()
		if err != nil {
			if wait.again(err) {
				continue
			}
			return err
		}
		wait.reset()
		r.request(conn, service)
	}
}

func (r *Requests) request(conn net.Conn, service string) {
	r.Open(conn, api.StreamRequest{Service: service}, nil, nil)
}

func (r *Requests) Open(conn net.Conn, req api.StreamRequest, head []byte, say func(conn net.Conn, why string)) {
	service := req.Service
	var raw [relay.TokenLen / 2]byte
	if _, err := rand.Read(raw[:]); err != nil {
		conn.Close()
		return
	}
	token := hex.EncodeToString(raw[:])
	wait := r.Wait
	if wait == 0 {
		wait = requestWait
	}
	r.mu.Lock()
	if r.waiting == nil {
		r.waiting = map[string]*request{}
	}
	if len(r.waiting) >= api.MaxStreamRequests || r.total >= api.MaxStreams {
		waiting, total := len(r.waiting), r.total
		r.mu.Unlock()
		if say != nil {
			say(conn, "too many requests wait for the Mac")
		}
		conn.Close()
		r.failed("streams: refused a connection to the host's %s: %d requests wait and %d streams exist", service, waiting, total)
		return
	}
	w := &request{conn: conn, service: service, head: head, say: say}
	w.timer = time.AfterFunc(wait, func() { r.drop(token, "the host did not answer in "+wait.String()) })
	r.waiting[token] = w
	r.total++
	r.mu.Unlock()
	req.Token = token
	if !r.Ask(req) {
		r.drop(token, "there is no connection to the host")
	}
}

func (r *Requests) drop(token, why string) {
	r.mu.Lock()
	req := r.waiting[token]
	if req != nil {
		delete(r.waiting, token)
		r.total--
	}
	r.mu.Unlock()
	if req == nil {
		return
	}
	req.timer.Stop()
	if req.say != nil {
		req.say(req.conn, why)
	}
	req.conn.Close()
	r.failed("streams: a connection to the host's %s was closed: %s", req.service, why)
}

func (r *Requests) Refuse(token, reason string) {
	if reason == "" {
		reason = "the host refused it"
	}
	r.drop(token, reason)
}

func (r *Requests) Attach(token string, host net.Conn) {
	r.mu.Lock()
	req := r.waiting[token]
	delete(r.waiting, token)
	if now := time.Now(); req != nil && now.Sub(r.since) > logWindow {
		r.nothing, r.since = 0, now
	}
	r.mu.Unlock()
	if req == nil {
		relay.WriteError(host, relay.CodeDenied, "no such stream request")
		return
	}
	req.timer.Stop()
	defer func() {
		r.mu.Lock()
		r.total--
		r.mu.Unlock()
	}()
	if relay.WriteOK(host) != nil {
		req.conn.Close()
		return
	}
	if len(req.head) > 0 {
		if _, err := host.Write(req.head); err != nil {
			req.conn.Close()
			return
		}
	}
	Splice(host, req.conn)
}

func (r *Requests) Close() {
	r.mu.Lock()
	tokens := make([]string, 0, len(r.waiting))
	for token := range r.waiting {
		tokens = append(tokens, token)
	}
	r.mu.Unlock()
	for _, token := range tokens {
		r.drop(token, "the service's socket was closed")
	}
}
