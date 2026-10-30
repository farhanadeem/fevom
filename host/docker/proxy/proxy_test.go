package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"fev/host/docker/fakeguest"
	"fev/host/docker/vsock"
)

type world struct {
	t          *testing.T
	p          *Proxy
	engine     *fakeguest.Engine
	engineSock string
}

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bp.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newWorld(t *testing.T) *world {
	t.Helper()
	dir := shortDir(t)
	w := &world{t: t, engine: fakeguest.NewEngine(), engineSock: filepath.Join(dir, "vsock_2375.sock")}
	l, err := net.Listen("unix", w.engineSock)
	if err != nil {
		t.Fatal(err)
	}
	go w.engine.Serve(l)
	w.p, err = Listen(filepath.Join(dir, "docker.sock"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		w.p.Close()
		w.engine.Close()
	})
	return w
}

func (w *world) dial(ctx context.Context) (net.Conn, error) {
	return vsock.Dial(ctx, w.engineSock)
}

func (w *world) client() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", w.p.Path())
		},
	}, Timeout: 20 * time.Second}
}

func (w *world) conn() *net.UnixConn {
	w.t.Helper()
	c, err := vsock.Dial(context.Background(), w.p.Path())
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(20 * time.Second))
	return c
}

func (w *world) held(n int) {
	w.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if held, _, _ := w.p.Counts(); held == n {
			return
		} else if time.Now().After(deadline) {
			w.t.Fatalf("the proxy holds %d connections, want %d", held, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func get(c *http.Client, path string) (int, string, error) {
	resp, err := c.Get("http://docker" + path)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

func TestRequestsMadeBeforeTheEngineIsReadyWaitAndThenSucceed(t *testing.T) {
	w := newWorld(t)
	type result struct {
		status int
		body   string
		err    error
	}
	results := make(chan result, 5)
	for range 5 {
		go func() {
			status, body, err := get(w.client(), "/_ping")
			results <- result{status, body, err}
		}()
	}
	w.held(5)
	time.Sleep(50 * time.Millisecond)
	if s := w.engine.Stats(); s.Connections != 0 || len(results) != 0 {
		t.Fatalf("while holding: the engine saw %d connections, %d clients were answered", s.Connections, len(results))
	}
	if r := w.p.Release(w.dial); r.Joined != 5 || r.Abandoned != 0 {
		t.Fatalf("released %+v, want 5 joined", r)
	}
	for range 5 {
		if r := <-results; r.err != nil || r.status != 200 || r.body != "OK" {
			t.Fatalf("a held request got %d %q, %v", r.status, r.body, r.err)
		}
	}
	if status, body, err := get(w.client(), "/_ping"); err != nil || status != 200 || body != "OK" {
		t.Fatalf("after release: %d %q, %v", status, body, err)
	}
	if held, _, joined := w.p.Counts(); held != 0 || joined != 6 {
		t.Fatalf("held %d, joined %d", held, joined)
	}
}

func TestOnlyAClosedPeerIsGone(t *testing.T) {
	dir := shortDir(t)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "s"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var ours []*net.UnixConn
	peer := func() *net.UnixConn {
		c, err := net.DialUnix("unix", nil, l.Addr().(*net.UnixAddr))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		s, err := l.AcceptUnix()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		ours = append(ours, s)
		return c
	}
	request := []byte("POST /containers/create HTTP/1.1\r\nHost: docker\r\nContent-Length: 2\r\n\r\n{}")
	open, halfClosed, closed, closedSilent := peer(), peer(), peer(), peer()
	open.Write(request)
	halfClosed.Write(request)
	halfClosed.CloseWrite()
	closed.Write(request)
	closed.Close()
	closedSilent.Close()
	got := gone(ours)
	want := []bool{false, false, true, true}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("gone = %v, want %v (open, finished sending, wrote then closed, closed)", got, want)
	}
	buf := make([]byte, len(request))
	ours[0].SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(ours[0], buf); err != nil || !bytes.Equal(buf, request) {
		t.Fatalf("after the check, read %q, %v", buf, err)
	}
}

func TestAConnectionAbandonedWhileHeldNeverReachesTheEngine(t *testing.T) {
	w := newWorld(t)
	run := `{"name":"stray"}`
	create := fmt.Sprintf("POST /_fake/run HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(run), run)

	cancelled := w.conn()
	io.WriteString(cancelled, create)
	ping := w.conn()
	io.WriteString(ping, "HEAD /_ping HTTP/1.1\r\nHost: docker\r\n\r\n")
	patient := w.conn()
	io.WriteString(patient, "GET /_ping HTTP/1.1\r\nHost: docker\r\nConnection: close\r\n\r\n")
	patient.CloseWrite()
	w.held(3)
	cancelled.Close()
	ping.Close()

	if r := w.p.Release(w.dial); r.Joined != 1 || r.Abandoned != 2 {
		t.Fatalf("released %+v, want 1 joined and 2 abandoned", r)
	}
	resp, err := http.ReadResponse(bufio.NewReader(patient), nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("the client that finished sending: %v, %v", resp, err)
	}
	if body, _ := io.ReadAll(resp.Body); string(body) != "OK" {
		t.Fatalf("body %q", body)
	}
	if status, body, err := get(w.client(), "/containers/json?all=1"); err != nil || status != 200 || strings.TrimSpace(body) != "[]" {
		t.Fatalf("containers after release: %d %q, %v: the cancelled request made one", status, body, err)
	}
	s := w.engine.Stats()
	if got := strings.Join(s.Paths, ", "); got != "GET /_ping, GET /containers/json" || s.Connections != 2 {
		t.Fatalf("the engine saw %d connections and %q", s.Connections, got)
	}
}

func TestAHijackedStreamPassesBothWaysWithHalfClose(t *testing.T) {
	w := newWorld(t)
	id, err := w.engine.Run(fakeguest.Spec{Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	c := w.conn()
	fmt.Fprintf(c, "POST /v1.48/containers/%s/attach?stream=1&stdin=1&stdout=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", id)
	w.held(1)
	w.p.Release(w.dial)

	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Upgrade") != "tcp" {
		t.Fatalf("attach response: %+v, %v", resp, err)
	}
	payload := make([]byte, 6<<20)
	for i := range payload {
		payload[i] = byte(i*7 + i>>11)
	}
	go func() {
		c.Write(payload)
		c.CloseWrite()
	}()
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, payload...), fakeguest.AttachEOF...)
	if !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes back, want %d; the tail is %q", len(got), len(want), got[max(0, len(got)-40):])
	}
}

func TestALargeUploadBegunWhileHeldArrivesWhole(t *testing.T) {
	w := newWorld(t)
	const size = 48 << 20
	sum := sha256.New()
	pr, pw := io.Pipe()
	go func() {
		chunk := make([]byte, 1<<20)
		for i := 0; i < size/len(chunk); i++ {
			for j := range chunk {
				chunk[j] = byte(i + j)
			}
			sum.Write(chunk)
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
		pw.Close()
	}()
	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := w.client().Post("http://docker/build", "application/x-tar", pr)
		if err != nil {
			done <- result{"", err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		done <- result{string(body), err}
	}()
	w.held(1)
	time.Sleep(100 * time.Millisecond)
	if s := w.engine.Stats(); s.Connections != 0 {
		t.Fatal("the engine was reached during the hold")
	}
	w.p.Release(w.dial)
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	var out struct{ Stream string }
	json.Unmarshal([]byte(r.body), &out)
	if want := fmt.Sprintf("sha256:%x %d\n", sum.Sum(nil), size); out.Stream != want {
		t.Fatalf("the engine received %q, want %q", out.Stream, want)
	}
}

func TestWhenThereWillBeNoEngineEveryClientIsToldWhy(t *testing.T) {
	w := newWorld(t)
	type result struct {
		status int
		body   string
		err    error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			status, body, err := get(w.client(), "/v1.48/info")
			results <- result{status, body, err}
		}()
	}
	w.held(2)
	started := time.Now()
	w.p.Refuse("the Docker engine did not start: no space left on device")
	want := `{"message":"fev: the Docker engine did not start: no space left on device"}`
	for range 2 {
		if r := <-results; r.err != nil || r.status != 503 || r.body != want {
			t.Fatalf("a held request got %d %q, %v", r.status, r.body, r.err)
		}
	}
	if status, body, err := get(w.client(), "/_ping"); err != nil || status != 503 || body != want {
		t.Fatalf("a later request got %d %q, %v", status, body, err)
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("the refusals took %v", took)
	}
	if s := w.engine.Stats(); s.Connections != 0 {
		t.Fatal("the engine was reached")
	}
	w.p.Hold()
	go func() {
		status, body, err := get(w.client(), "/_ping")
		results <- result{status, body, err}
	}()
	w.held(1)
	w.p.Release(w.dial)
	if r := <-results; r.err != nil || r.status != 200 {
		t.Fatalf("after the engine came up: %d %q, %v", r.status, r.body, r.err)
	}
}

func TestAnEngineRestartEndsOpenStreamsAndHoldsNewOnes(t *testing.T) {
	w := newWorld(t)
	w.p.Release(w.dial)
	resp, err := w.client().Get("http://docker/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	w.engine.Emit("volume", "create", "data", nil)
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, `"volume"`) {
		t.Fatalf("event %q, %v", line, err)
	}

	w.p.Hold()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Log("the open stream ended cleanly")
	}
	if _, active, _ := w.p.Counts(); active != 0 {
		deadline := time.Now().Add(2 * time.Second)
		for active != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
			_, active, _ = w.p.Counts()
		}
		if active != 0 {
			t.Fatalf("%d connections are still being copied after the hold", active)
		}
	}
	answered := make(chan int, 1)
	go func() {
		status, _, _ := get(w.client(), "/_ping")
		answered <- status
	}()
	w.held(1)
	select {
	case status := <-answered:
		t.Fatalf("a request during the restart was answered with %d", status)
	case <-time.After(50 * time.Millisecond):
	}
	w.p.Release(w.dial)
	if status := <-answered; status != 200 {
		t.Fatalf("after the restart: %d", status)
	}
}

func TestTheSocketIsForOneUserAndReplacesAStaleOne(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "docker.sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	p, err := Listen(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode %v, %v", info.Mode(), err)
	}
	p.Close()
	p.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the socket is still there after Close: %v", err)
	}
}

func TestMoreConnectionsAreHeldThanTheDefaultFileLimitAllows(t *testing.T) {
	var before unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &before); err != nil {
		t.Fatal(err)
	}
	low := before
	low.Cur = 256
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &low); err != nil {
		t.Fatal(err)
	}
	defer unix.Setrlimit(unix.RLIMIT_NOFILE, &before)
	limit, err := RaiseFileLimit()
	if err != nil {
		t.Fatal(err)
	}
	const n = 400
	if limit < 4*n {
		t.Skipf("the hard limit here is %d descriptors, too few for this test", limit)
	}
	w := newWorld(t)
	var wg sync.WaitGroup
	failed := make(chan string, n)
	for range n {
		c := w.conn()
		wg.Add(1)
		go func() {
			defer wg.Done()
			io.WriteString(c, "GET /_ping HTTP/1.1\r\nHost: docker\r\nConnection: close\r\n\r\n")
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil || resp.StatusCode != 200 {
				failed <- fmt.Sprint(resp, err)
			}
		}()
	}
	w.held(n)
	if r := w.p.Release(w.dial); r.Joined != n {
		t.Fatalf("released %+v, want %d joined", r, n)
	}
	wg.Wait()
	if len(failed) != 0 {
		t.Fatalf("%d of %d held requests failed; the first: %s", len(failed), n, <-failed)
	}
}

func TestUnavailableIsAResponseTheDockerClientCanRead(t *testing.T) {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(Unavailable(`quoted "text"`))), nil)
	if err != nil || resp.StatusCode != 503 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%+v, %v", resp, err)
	}
	var body struct{ Message string }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Message != `fev: quoted "text"` {
		t.Fatalf("%q, %v", body.Message, err)
	}
}
