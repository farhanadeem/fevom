package nfsview

import (
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecordsEndWhereTheirLastFragmentDoes(t *testing.T) {
	frag := func(last bool, body string) []byte {
		v := uint32(len(body))
		if last {
			v |= 0x80000000
		}
		b := binary.BigEndian.AppendUint32(nil, v)
		return append(b, body...)
	}
	var stream []byte
	stream = append(stream, frag(true, "one")...)
	stream = append(stream, frag(false, "tw")...)
	stream = append(stream, frag(true, "o")...)
	stream = append(stream, frag(true, "")...)
	stream = append(stream, frag(true, "four")...)
	for _, step := range []int{len(stream), 1, 3} {
		r := &records{}
		n := 0
		for p := stream; len(p) > 0; {
			k := min(step, len(p))
			r.feed(p[:k], func() { n++ })
			p = p[k:]
		}
		if n != 4 {
			t.Errorf("by %d bytes: %d records, want 4", step, n)
		}
	}
}

func stallServer(t *testing.T, answer *atomic.Bool) (*Server, *atomic.Int32, chan struct{}) {
	t.Helper()
	old := StallAfter
	StallAfter = 100 * time.Millisecond
	t.Cleanup(func() { StallAfter = old })
	o := &pairOpener{serve: func(c *net.UnixConn) {
		defer c.Close()
		for {
			var mark [4]byte
			if _, err := io.ReadFull(c, mark[:]); err != nil {
				return
			}
			body := make([]byte, binary.BigEndian.Uint32(mark[:])&0x7fffffff)
			if _, err := io.ReadFull(c, body); err != nil {
				return
			}
			if answer.Load() {
				c.Write(append(mark[:], body...))
			}
		}
	}}
	s, err := Listen(filepath.Join(shortDir(t), "view.sock"), o, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	told := &atomic.Int32{}
	again := make(chan struct{}, 16)
	return s, told, again
}

func call(t *testing.T, c net.Conn) {
	t.Helper()
	if _, err := c.Write(binary.BigEndian.AppendUint32(nil, 0x80000004)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
}

func TestASilentGuestIsToldOnceARequestHasWaited(t *testing.T) {
	answer := &atomic.Bool{}
	answer.Store(true)
	s, told, _ := stallServer(t, answer)
	keep := &atomic.Bool{}
	keep.Store(true)
	s.OnStall(func() bool { told.Add(1); return keep.Load() })
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 5; i++ {
		call(t, c)
		reply := make([]byte, 8)
		if _, err := io.ReadFull(c, reply); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(400 * time.Millisecond)
	if n := told.Load(); n != 0 {
		t.Fatalf("told %d times of a guest that answers", n)
	}
	answer.Store(false)
	call(t, c)
	time.Sleep(50 * time.Millisecond)
	if n := told.Load(); n != 0 {
		t.Fatalf("told before StallAfter: %d", n)
	}
	time.Sleep(120 * time.Millisecond)
	if n := told.Load(); n != 1 {
		t.Fatalf("told %d times after one StallAfter", n)
	}
	time.Sleep(100 * time.Millisecond)
	if n := told.Load(); n < 2 {
		t.Fatalf("not told again: %d", n)
	}
	keep.Store(false)
	time.Sleep(150 * time.Millisecond)
	settled := told.Load()
	time.Sleep(300 * time.Millisecond)
	if n := told.Load(); n != settled {
		t.Fatalf("told %d more times after answering false", n-settled)
	}
	s.Rearm()
	time.Sleep(150 * time.Millisecond)
	if n := told.Load(); n != settled+1 {
		t.Fatalf("Rearm: told %d, want %d", n, settled+1)
	}
}

func TestOneReplyOfTwoLeavesTheOtherWatched(t *testing.T) {
	answer := &atomic.Bool{}
	answer.Store(true)
	s, told, _ := stallServer(t, answer)
	s.OnStall(func() bool { told.Add(1); return false })
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	call(t, c)
	reply := make([]byte, 8)
	if _, err := io.ReadFull(c, reply); err != nil {
		t.Fatal(err)
	}
	answer.Store(false)
	call(t, c)
	call(t, c)
	time.Sleep(250 * time.Millisecond)
	if n := told.Load(); n != 1 {
		t.Fatalf("told %d times with two requests out", n)
	}
	c.Close()
	s.Rearm()
	time.Sleep(250 * time.Millisecond)
	if n := told.Load(); n != 1 {
		t.Fatalf("told of a connection that ended: %d", n)
	}
}
