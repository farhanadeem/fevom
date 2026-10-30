package nfsview

import (
	"io"
	"net"
	"runtime/pprof"
	"strconv"
	"testing"
	"time"
)

func TestManyConnectionsDoNotTakeAThreadEach(t *testing.T) {
	const n = 300
	o := &pairOpener{serve: func(c *net.UnixConn) {
		defer c.Close()
		io.Copy(c, c)
	}}
	s, err := ListenLoopback(o, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	threads := pprof.Lookup("threadcreate")
	before := threads.Count()
	addr := "127.0.0.1:" + strconv.Itoa(s.Endpoint().Port)
	conns := make([]net.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := range n {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		msg := []byte("call " + strconv.Itoa(i))
		c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := c.Write(msg); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(c, got); err != nil || string(got) != string(msg) {
			t.Fatalf("connection %d: read %q, %v", i, got, err)
		}
	}
	if made := threads.Count() - before; made > 2*BlockingConns+64 {
		t.Errorf("%d connections made %d threads; want at most %d", n, made, 2*BlockingConns+64)
	}
	s.mu.Lock()
	blocking := 0
	for c := range s.conns {
		if _, ok := c.(*fdConn); ok {
			blocking++
		}
	}
	s.mu.Unlock()
	if blocking != 2*BlockingConns {
		t.Errorf("%d halves read with blocking calls, want %d (both halves of the first %d connections)", blocking, 2*BlockingConns, BlockingConns)
	}
	for _, c := range conns {
		c.Close()
	}
	conns = conns[:0]
	deadline := time.Now().Add(5 * time.Second)
	for (s.blocking.Load() != 0 || s.Conns() != 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := s.blocking.Load(); n != 0 {
		t.Errorf("%d places held after every connection ended", n)
	}
}
