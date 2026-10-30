package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type workEngine struct {
	sock    string
	release chan struct{}
	mu      sync.Mutex
	seen    []string
	arrived chan string
}

func newWorkEngine(t *testing.T) *workEngine {
	e := &workEngine{sock: filepath.Join(shortDir(t), "e.sock"), release: make(chan struct{}), arrived: make(chan string, 64)}
	l, err := net.Listen("unix", e.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		what := r.Method + " " + r.URL.Path
		e.mu.Lock()
		e.seen = append(e.seen, what)
		e.mu.Unlock()
		e.arrived <- what
		p := r.URL.Path
		switch {
		case r.Header.Get("Upgrade") != "":
			c, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			rw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: " + r.Header.Get("Upgrade") + "\r\n\r\n")
			rw.Flush()
			io.Copy(io.Discard, rw)
			c.Close()
		case strings.HasSuffix(p, "/commit"), strings.HasSuffix(p, "/build"), strings.HasSuffix(p, "/archive"),
			strings.HasSuffix(p, "/wait"), strings.HasSuffix(p, "/create"), strings.HasSuffix(p, "/events"),
			strings.HasSuffix(p, "/logs"), strings.HasSuffix(p, "/get"):
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-e.release
			io.WriteString(w, "{}")
		default:
			io.WriteString(w, "[]")
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return e
}

func (e *workEngine) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", e.sock)
}

func (e *workEngine) saw(what string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.seen {
		if s == what {
			return true
		}
	}
	return false
}

func workProxy(t *testing.T, e *workEngine) *Proxy {
	p, err := Listen(filepath.Join(shortDir(t), "docker.sock"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	p.Release(e.dial)
	return p
}

func send(t *testing.T, p *Proxy, e *workEngine, req string) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", p.Path())
	if err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, bufio.NewReader(c))
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	select {
	case <-e.arrived:
	case <-time.After(5 * time.Second):
		t.Fatalf("the engine never had %q", strings.SplitN(req, "\r\n", 2)[0])
	}
	return c
}

type fenceResult struct {
	f       Fenced
	release func()
	err     error
}

func fenceAsync(p *Proxy, bound time.Duration) chan fenceResult {
	ch := make(chan fenceResult, 1)
	go func() {
		f, rel, err := p.Fence(context.Background(), bound, nil)
		ch <- fenceResult{f, rel, err}
	}()
	return ch
}

func TestTheFenceWaitsForEveryRequestThatReachesContainerd(t *testing.T) {
	const h = "Host: docker\r\n"
	for _, c := range []struct {
		name, req     string
		wait, byClose bool
	}{
		{"commit", "POST /v1.47/commit?container=c&repo=x HTTP/1.1\r\n" + h + "Content-Length: 0\r\n\r\n", true, false},
		{"legacy build", "POST /v1.47/build?t=x HTTP/1.1\r\n" + h + "Content-Length: 3\r\n\r\nabc", true, false},
		{"BuildKit's controller", "POST /grpc HTTP/1.1\r\n" + h + "Upgrade: h2c\r\nConnection: Upgrade\r\n\r\n", true, true},
		{"BuildKit's session", "POST /session HTTP/1.1\r\n" + h + "Upgrade: h2c\r\nConnection: Upgrade\r\nX-Docker-Expose-Session-Uuid: u\r\n\r\n", true, true},
		{"copy into a container", "PUT /v1.47/containers/c/archive?path=/ HTTP/1.1\r\n" + h + "Content-Length: 3\r\n\r\nabc", true, false},
		{"copy out of a container", "GET /v1.47/containers/c/archive?path=/ HTTP/1.1\r\n" + h + "\r\n", true, false},
		{"container create", "POST /v1.47/containers/create HTTP/1.1\r\n" + h + "Content-Length: 2\r\n\r\n{}", true, false},
		{"a --rm container's end", "POST /v1.47/containers/c/wait?condition=removed HTTP/1.1\r\n" + h + "Content-Length: 0\r\n\r\n", true, false},
		{"plain wait", "POST /v1.47/containers/c/wait?condition=next-exit HTTP/1.1\r\n" + h + "Content-Length: 0\r\n\r\n", false, false},
		{"exec's stream", "POST /v1.47/exec/e/start HTTP/1.1\r\n" + h + "Upgrade: tcp\r\nConnection: Upgrade\r\nContent-Length: 2\r\n\r\n{}", false, true},
		{"attach", "POST /v1.47/containers/c/attach?stream=1 HTTP/1.1\r\n" + h + "Upgrade: tcp\r\nConnection: Upgrade\r\n\r\n", false, true},
		{"events", "GET /v1.47/events HTTP/1.1\r\n" + h + "\r\n", false, false},
		{"logs followed", "GET /v1.47/containers/c/logs?follow=1&stdout=1 HTTP/1.1\r\n" + h + "\r\n", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newWorkEngine(t)
			p := workProxy(t, e)
			conn := send(t, p, e, c.req)
			defer conn.Close()
			done := fenceAsync(p, 10*time.Second)
			if !c.wait {
				select {
				case r := <-done:
					r.release()
					if r.err != nil || len(r.f.Left) != 0 {
						t.Fatalf("fenced %+v %v", r.f, r.err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("the fence waited for a stream that never ends of itself")
				}
				close(e.release)
				return
			}
			select {
			case r := <-done:
				r.release()
				t.Fatalf("the fence ended with %s in flight: %+v", c.name, r.f)
			case <-time.After(300 * time.Millisecond):
			}
			if c.byClose {
				conn.Close()
			} else {
				close(e.release)
			}
			select {
			case r := <-done:
				r.release()
				if r.err != nil || len(r.f.Left) != 0 {
					t.Fatalf("fenced %+v %v", r.f, r.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("the fence did not end after %s did", c.name)
			}
			if c.byClose {
				close(e.release)
			}
		})
	}
}

func TestWhileTheFenceWaitsNewWorkIsHeld(t *testing.T) {
	e := newWorkEngine(t)
	p := workProxy(t, e)
	commit := send(t, p, e, "POST /v1.47/commit?container=c HTTP/1.1\r\nHost: docker\r\nContent-Length: 0\r\n\r\n")
	defer commit.Close()
	done := fenceAsync(p, 10*time.Second)
	time.Sleep(100 * time.Millisecond)
	created := make(chan error, 1)
	go func() {
		r, err := fenceClient(p, false).Post("http://docker/v1.47/containers/create", "application/json", strings.NewReader("{}"))
		if err == nil {
			io.ReadAll(r.Body)
			r.Body.Close()
		}
		created <- err
	}()
	if r, err := fenceClient(p, false).Get("http://docker/v1.47/containers/json"); err != nil {
		t.Fatalf("a question during the wait: %v", err)
	} else {
		r.Body.Close()
	}
	time.Sleep(150 * time.Millisecond)
	if e.saw("POST /v1.47/containers/create") {
		t.Fatal("a container create reached the engine while the fence waited")
	}
	close(e.release)
	var r fenceResult
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the fence did not end after the commit")
	}
	time.Sleep(100 * time.Millisecond)
	if e.saw("POST /v1.47/containers/create") {
		t.Fatal("a container create reached the engine during the restart")
	}
	r.release()
	if err := <-created; err != nil || !e.saw("POST /v1.47/containers/create") {
		t.Fatalf("the held create: %v %v", err, e.seen)
	}
}

func TestARemovalStillRunningIsNotWaitedForPastItsBound(t *testing.T) {
	was := removalBound
	removalBound = 300 * time.Millisecond
	defer func() { removalBound = was }()
	e := newWorkEngine(t)
	defer close(e.release)
	p := workProxy(t, e)
	w := send(t, p, e, "POST /v1.47/containers/c/wait?condition=removed HTTP/1.1\r\nHost: docker\r\nContent-Length: 0\r\n\r\n")
	defer w.Close()
	began := time.Now()
	r := <-fenceAsync(p, 10*time.Second)
	r.release()
	if took := time.Since(began); took < 300*time.Millisecond || took > 3*time.Second || len(r.f.Left) != 0 {
		t.Fatalf("fenced %+v after %v", r.f, took)
	}
}

func TestAFenceRaisedAsAHeaderEndsHoldsOrCountsIt(t *testing.T) {
	e := newWorkEngine(t)
	p := workProxy(t, e)
	var once sync.Once
	raised := make(chan fenceResult, 1)
	headerEnded = func() {
		once.Do(func() {
			f, rel, err := p.Fence(context.Background(), time.Second, nil)
			raised <- fenceResult{f, rel, err}
		})
	}
	defer func() { headerEnded = nil }()
	c, err := net.Dial("unix", p.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go io.Copy(io.Discard, bufio.NewReader(c))
	io.WriteString(c, "POST /v1.47/commit?container=c HTTP/1.1\r\nHost: docker\r\nContent-Length: 0\r\n\r\n")
	var r fenceResult
	select {
	case r = <-raised:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook never ran")
	}
	time.Sleep(200 * time.Millisecond)
	if e.saw("POST /v1.47/commit") {
		r.release()
		close(e.release)
		t.Fatalf("a commit nobody counted reached the engine during the restart (fenced %+v)", r.f)
	}
	r.release()
	select {
	case <-e.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the held commit never reached the engine after the release")
	}
	close(e.release)
}

func TestAKeptConnectionsTransferLeavesFlightAtItsEnd(t *testing.T) {
	e := newWorkEngine(t)
	p := workProxy(t, e)
	c := send(t, p, e, "GET /v1.47/images/get?names=x HTTP/1.1\r\nHost: docker\r\n\r\n")
	defer c.Close()
	done := fenceAsync(p, 10*time.Second)
	time.Sleep(200 * time.Millisecond)
	close(e.release)
	select {
	case r := <-done:
		r.release()
		if len(r.f.Left) != 0 || r.f.Waited > 3*time.Second {
			t.Fatalf("fenced %+v", r.f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fence waited for a kept connection whose save had ended")
	}
}
