package streams

import (
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type gush struct{ net.Conn }

func (gush) Read(p []byte) (int, error)      { return len(p), nil }
func (gush) SetReadDeadline(time.Time) error { return nil }
func (gush) Close() error                    { return nil }

type dam struct {
	net.Conn
	held *atomic.Int64
	open chan struct{}
}

func (d dam) Write(p []byte) (int, error) {
	if len(p) > smallCopyBuffer {
		d.held.Add(1)
		<-d.open
		return 0, net.ErrClosed
	}
	return len(p), nil
}

func TestStalledStreamsHoldABoundedNumberOfLargeBuffers(t *testing.T) {
	const streams = 3 * maxLargeBuffers
	var held atomic.Int64
	open := make(chan struct{})
	var wg sync.WaitGroup
	for range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copyStream(dam{held: &held, open: open}, gush{}, nil)
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for held.Load() < maxLargeBuffers && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	got := held.Load()
	close(open)
	wg.Wait()
	if got != maxLargeBuffers {
		t.Fatalf("%d stalled streams hold %d large buffers (%d MiB); the bound is %d", streams, got, got*largeCopyBuffer>>20, maxLargeBuffers)
	}
	if n := largeLent.Load(); n != 0 {
		t.Fatalf("%d large buffers are still counted as lent after every stream ended", n)
	}
	held.Store(0)
	open = make(chan struct{})
	done := make(chan struct{})
	go func() { copyStream(dam{held: &held, open: open}, gush{}, nil); close(done) }()
	for held.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if held.Load() != 1 {
		t.Fatal("a stream that came after the stalled ones ended was lent no large buffer")
	}
	close(open)
	<-done
}

type flood struct {
	left      atomic.Int64
	open, max atomic.Int64
	done      chan struct{}
}

type flooded struct {
	net.Conn
	f      *flood
	closed chan struct{}
	once   sync.Once
}

func (f *flood) Accept() (net.Conn, error) {
	if f.left.Add(-1) < 0 {
		<-f.done
		return nil, net.ErrClosed
	}
	n := f.open.Add(1)
	for {
		m := f.max.Load()
		if n <= m || f.max.CompareAndSwap(m, n) {
			break
		}
	}
	return &flooded{f: f, closed: make(chan struct{})}, nil
}
func (f *flood) Close() error   { return nil }
func (f *flood) Addr() net.Addr { return nil }

func (c *flooded) Write(p []byte) (int, error) {
	time.Sleep(time.Millisecond)
	return len(p), nil
}
func (c *flooded) Read([]byte) (int, error) {
	<-c.closed
	return 0, net.ErrClosed
}
func (c *flooded) SetReadDeadline(time.Time) error  { return nil }
func (c *flooded) SetWriteDeadline(time.Time) error { return nil }
func (c *flooded) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.f.open.Add(-1)
	})
	return nil
}

func TestABurstPastTheLimitHoldsOneDescriptorOver(t *testing.T) {
	f := &flood{done: make(chan struct{})}
	f.left.Store(500)
	r := &Relay{Max: 2, Local: func(netip.Addr) bool { return false }}
	ended := make(chan error, 1)
	go func() { ended <- r.Serve(f) }()
	deadline := time.Now().Add(20 * time.Second)
	for f.left.Load() >= 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.left.Load() >= 0 {
		t.Fatal("the relay stopped accepting")
	}
	if got := f.max.Load(); got > 3 {
		t.Fatalf("a burst of 500 against a relay of 2 streams had %d connections open at one time; 3 is the most the limit allows", got)
	}
	if got := r.active.Load(); got != 2 {
		t.Fatalf("%d streams are counted; the relay has 2", got)
	}
	close(f.done)
	<-ended
}
