package vsock

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"fev/host/docker/fakeguest"
	"fev/proto/relay"
)

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bk.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestADialledStreamHasTheSendBuffer(t *testing.T) {
	path := filepath.Join(shortDir(t), "s")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := Dial(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw, _ := c.SyscallConn()
	var size int
	raw.Control(func(fd uintptr) { size, err = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF) })
	if err != nil || size != SendBuffer {
		t.Fatalf("SO_SNDBUF is %d, %v; want %d", size, err, SendBuffer)
	}
}

func TestABurstLargerThanTheListenQueueAllConnect(t *testing.T) {
	path := filepath.Join(shortDir(t), "s")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	const n = 600
	accepted := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		for range n {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
		close(accepted)
	}()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := Dial(context.Background(), path)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			<-accepted
		}()
	}
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("%d of %d connections failed; the first: %v", len(errs), n, <-errs)
	}
}

func TestASocketNobodyListensOnIsRefusedAfterTheWait(t *testing.T) {
	path := filepath.Join(shortDir(t), "s")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	started := time.Now()
	_, err = Dial(context.Background(), path)
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("got %v, want connection refused", err)
	}
	if took := time.Since(started); took < refusedFor || took > refusedFor+time.Second {
		t.Fatalf("gave up after %v, want about %v", took, refusedFor)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started = time.Now()
	if _, err := Dial(ctx, path); err == nil || time.Since(started) > time.Second {
		t.Fatalf("got %v after %v", err, time.Since(started))
	}
	started = time.Now()
	if _, err := Dial(context.Background(), path+".absent"); err == nil || time.Since(started) > time.Second {
		t.Fatalf("got %v after %v", err, time.Since(started))
	}
}

func TestOpenSpeaksTheRelayProtocol(t *testing.T) {
	path := filepath.Join(shortDir(t), "vsock_1026.sock")
	engine := fakeguest.NewEngine()
	defer engine.Close()
	if _, err := engine.Run(fakeguest.Spec{Name: "web", Ports: []fakeguest.Port{{HostPort: 8080, Proto: "tcp", ContainerPort: 80}}}); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go (&fakeguest.Relay{Engine: engine}).Serve(l)

	r := Relay{Path: path}
	conn, err := r.Open(context.Background(), relay.Request{Kind: relay.TCP, Target: "127.0.0.1:8080"})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	_, err = r.Open(context.Background(), relay.Request{Kind: relay.TCP, Target: "127.0.0.1:8081"})
	var refusal *relay.Error
	if !errors.As(err, &refusal) || refusal.Code != relay.CodeRefused {
		t.Fatalf("got %v, want a refusal", err)
	}
	if _, err := r.Open(context.Background(), relay.Request{Kind: relay.TCP, Target: "localhost:80"}); err == nil {
		t.Fatal("a host name was accepted as a target")
	}
}

func TestOpenGivesUpOnAGuestThatDoesNotAnswer(t *testing.T) {
	path := filepath.Join(shortDir(t), "s")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := (Relay{Path: path}).Open(ctx, relay.Request{Kind: relay.UDP, Target: "127.0.0.1:53"}); err == nil || time.Since(started) > time.Second {
		t.Fatalf("got %v after %v", err, time.Since(started))
	}
}
