package session

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"
	"fev/proto/relay"

	"fev/guest/internal/reaper"
)

func manager(t *testing.T) *Manager {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh")
	}
	r := reaper.New()
	sig := make(chan os.Signal, 16)
	signal.Notify(sig, unix.SIGCHLD)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-sig:
				r.Reap()
			case <-done:
				return
			}
		}
	}()
	t.Cleanup(func() { signal.Stop(sig); close(done) })
	return NewManager(r, map[string]Context{api.ExecInit: Here{}}, t.Logf)
}

func attachHost(t *testing.T, m *Manager, token string) (net.Conn, error) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := func(fd int) net.Conn {
		f := os.NewFile(uintptr(fd), "socket")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	host, guest := conn(fds[0]), conn(fds[1])
	t.Cleanup(func() { host.Close() })
	go m.Attach(token, guest)
	host.SetDeadline(time.Now().Add(20 * time.Second))
	return host, relay.ReadReply(host)
}

func exitOf(t *testing.T, ch <-chan api.ExecExit) api.ExecExit {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(20 * time.Second):
		t.Fatal("the session's exit was never reported")
		return api.ExecExit{}
	}
}

func expect(t *testing.T, r *bufio.Reader, text string) string {
	t.Helper()
	var seen strings.Builder
	for !strings.Contains(seen.String(), text) {
		b, err := r.ReadByte()
		if err != nil {
			t.Fatalf("waiting for %q: %v; saw %q", text, err, seen.String())
		}
		seen.WriteByte(b)
	}
	return seen.String()
}

func TestTerminalSessionEchoesResizesAndReportsASignal(t *testing.T) {
	m := manager(t)
	exits := make(chan api.ExecExit, 1)
	script := `test -t 0 && echo tty=yes; stty size; read line; echo "got=$line"; stty size; trap 'echo winch' WINCH; echo ready; exec sleep 100`
	started, err := m.Start(api.ExecStartParams{
		Context: api.ExecInit, Argv: []string{"sh", "-c", script}, Env: []string{"PATH=/usr/bin:/bin"},
		TTY: true, Rows: 24, Cols: 80,
	}, func(e api.ExecExit) { exits <- e })
	if err != nil {
		t.Fatal(err)
	}
	if len(started.Token) != relay.TokenLen {
		t.Fatalf("token %q", started.Token)
	}
	host, err := attachHost(t, m, started.Token)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(host)
	expect(t, r, "tty=yes")
	expect(t, r, "24 80")
	if err := m.Resize(started.ID, 40, 120); err != nil {
		t.Fatal(err)
	}
	io.WriteString(host, "hello\n")
	if out := expect(t, r, "got=hello"); !strings.Contains(out, "hello\r\n") {
		t.Fatalf("input was not echoed: %q", out)
	}
	expect(t, r, "40 120")
	expect(t, r, "ready")
	if err := m.Signal(started.ID, int(unix.SIGINT)); err != nil {
		t.Fatal(err)
	}
	if e := exitOf(t, exits); e.ID != started.ID || e.ExitCode != -1 || e.Signal != int(unix.SIGINT) {
		t.Fatalf("exit %+v", e)
	}
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("stream did not end cleanly: %v", err)
	}
	if m.Count() != 0 {
		t.Fatalf("%d sessions left", m.Count())
	}
}

func TestFramedSessionKeepsOutputsApart(t *testing.T) {
	m := manager(t)
	exits := make(chan api.ExecExit, 1)
	started, err := m.Start(api.ExecStartParams{
		Context: api.ExecInit, Argv: []string{"/bin/sh", "-c", "echo out; echo err >&2; cat; pwd; echo $GREETING; exit 3"},
		Env: []string{"GREETING=hi", "PATH=/usr/bin:/bin"}, Dir: "/tmp",
	}, func(e api.ExecExit) { exits <- e })
	if err != nil {
		t.Fatal(err)
	}
	host, err := attachHost(t, m, started.Token)
	if err != nil {
		t.Fatal(err)
	}
	relay.WriteFrame(host, relay.Stdin, []byte("typed\n"))
	relay.WriteFrame(host, relay.Stdin, nil)
	got := map[byte]string{}
	buf := make([]byte, relay.MaxFrame)
	for {
		stream, n, err := relay.ReadFrame(host, buf)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			break
		}
		got[stream] += string(buf[:n])
	}
	if got[relay.Stdout] != "out\ntyped\n/tmp\nhi\n" || got[relay.Stderr] != "err\n" {
		t.Fatalf("stdout %q, stderr %q", got[relay.Stdout], got[relay.Stderr])
	}
	if e := exitOf(t, exits); e.ExitCode != 3 || e.Signal != 0 || e.Error != "" {
		t.Fatalf("exit %+v", e)
	}
}

func TestATokenWorksOnceAndAnUnknownOneIsDenied(t *testing.T) {
	m := manager(t)
	exits := make(chan api.ExecExit, 1)
	started, err := m.Start(api.ExecStartParams{Context: api.ExecInit, Argv: []string{"/bin/sh", "-c", "exit 0"}}, func(e api.ExecExit) { exits <- e })
	if err != nil {
		t.Fatal(err)
	}
	var denied *relay.Error
	if _, err := attachHost(t, m, strings.Repeat("0", relay.TokenLen)); !errors.As(err, &denied) || denied.Code != relay.CodeDenied {
		t.Fatalf("unknown token: %v", err)
	}
	if _, err := attachHost(t, m, started.Token); err != nil {
		t.Fatal(err)
	}
	exitOf(t, exits)
	if _, err := attachHost(t, m, started.Token); !errors.As(err, &denied) || denied.Code != relay.CodeDenied {
		t.Fatalf("second use of a token: %v", err)
	}
}

func TestAnUnattachedSessionExpiresAndAnUnknownContextIsRefused(t *testing.T) {
	m := manager(t)
	var fire func()
	m.after = func(d time.Duration, f func()) func() {
		if d != AttachTimeout {
			t.Errorf("expiry armed for %v", d)
		}
		fire = f
		return func() {}
	}
	exits := make(chan api.ExecExit, 1)
	started, err := m.Start(api.ExecStartParams{Context: api.ExecInit, Argv: []string{"/bin/true"}}, func(e api.ExecExit) { exits <- e })
	if err != nil {
		t.Fatal(err)
	}
	fire()
	if e := exitOf(t, exits); e.ID != started.ID || e.ExitCode != -1 || e.Error == "" {
		t.Fatalf("expiry reported as %+v", e)
	}
	var denied *relay.Error
	if _, err := attachHost(t, m, started.Token); !errors.As(err, &denied) {
		t.Fatalf("an expired token still worked: %v", err)
	}
	if m.Count() != 0 {
		t.Fatal("the expired session is still held")
	}
	if _, err := m.Start(api.ExecStartParams{Context: api.ExecEngine, Argv: []string{"/bin/true"}}, func(api.ExecExit) {}); err == nil {
		t.Fatal("a context that does not exist was accepted")
	}
}

func TestFailureToStartCloseAndShutdown(t *testing.T) {
	m := manager(t)
	exits := make(chan api.ExecExit, 4)
	started, _ := m.Start(api.ExecStartParams{Context: api.ExecInit, Argv: []string{"no-such-program"}}, func(e api.ExecExit) { exits <- e })
	var failed *relay.Error
	if _, err := attachHost(t, m, started.Token); !errors.As(err, &failed) || failed.Code != relay.CodeFailed {
		t.Fatalf("got %v", err)
	}
	if e := exitOf(t, exits); e.ExitCode != -1 || !strings.Contains(e.Error, "not found") {
		t.Fatalf("exit %+v", e)
	}

	started, _ = m.Start(api.ExecStartParams{Context: api.ExecInit, Argv: []string{"/bin/sh", "-c", "echo up; sleep 100"}, TTY: true}, func(e api.ExecExit) { exits <- e })
	host, err := attachHost(t, m, started.Token)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(host)
	expect(t, r, "up")
	if err := m.Close(started.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("stream after close: %v", err)
	}
	if err := m.Signal(started.ID, 15); err == nil {
		t.Fatal("a closed session still took a signal")
	}

	started, _ = m.Start(api.ExecStartParams{Context: api.ExecInit, Argv: []string{"/bin/sh", "-c", "trap '' TERM HUP; echo up; sleep 100"}}, func(e api.ExecExit) { exits <- e })
	host, err = attachHost(t, m, started.Token)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, relay.MaxFrame)
	if _, _, err := relay.ReadFrame(host, buf); err != nil {
		t.Fatal(err)
	}
	if left := m.Shutdown(10 * time.Second); left != 0 {
		t.Fatalf("%d sessions outlived the shutdown", left)
	}
	if _, err := m.Start(api.ExecStartParams{Context: api.ExecInit, Argv: []string{"/bin/true"}}, func(api.ExecExit) {}); err == nil {
		t.Fatal("a session was accepted during shutdown")
	}
	if e := exitOf(t, exits); e.ID != started.ID || e.Signal != int(unix.SIGKILL) {
		t.Fatalf("the session the shutdown ended reported %+v", e)
	}
	select {
	case e := <-exits:
		t.Fatalf("a closed session reported %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
}
