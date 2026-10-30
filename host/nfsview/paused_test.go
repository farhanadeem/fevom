package nfsview

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

type xdr []byte

func (x xdr) u32(v ...uint32) xdr {
	for _, n := range v {
		x = binary.BigEndian.AppendUint32(x, n)
	}
	return x
}

func (x xdr) opaque(b []byte) xdr {
	x = x.u32(uint32(len(b)))
	x = append(x, b...)
	return append(x, make([]byte, (4-len(b)%4)%4)...)
}

func macCall(xid, proc uint32, tag string, ops ...uint32) []byte {
	cred := xdr(nil).u32(0).opaque([]byte("mac.local")).u32(501, 20, 3, 20, 12, 61)
	x := xdr(nil).u32(xid, rpcCall, rpcVersion, nfsProgram, nfsVersion4, proc).u32(1).opaque(cred).u32(authNone).opaque(nil)
	if proc == procCompound {
		x = x.opaque([]byte(tag)).u32(0, uint32(len(ops)))
		for _, op := range ops {
			x = x.u32(op)
			if op == opRenew {
				x = x.u32(0x1234, 0x5678)
			} else {
				x = x.opaque([]byte("a file handle"))
			}
		}
	}
	return x
}

func record(p []byte, frag int) []byte {
	var out []byte
	for {
		n := min(len(p), frag)
		mark := uint32(n)
		if n == len(p) {
			mark |= 0x80000000
		}
		out = binary.BigEndian.AppendUint32(out, mark)
		out = append(out, p[:n]...)
		p = p[n:]
		if len(p) == 0 {
			return out
		}
	}
}

func TestTheHeadOfACallIsReadAsItComes(t *testing.T) {
	cases := []struct {
		name string
		call []byte
		want headResult
		op   uint32
	}{
		{"getattr", macCall(7, procCompound, "getattr     ", 22, 9), headOurs, 22},
		{"renew", macCall(8, procCompound, "renew       ", opRenew), headOurs, opRenew},
		{"null", macCall(9, procNull, ""), headOurs, 0},
		{"another program", xdr(nil).u32(10, rpcCall, rpcVersion, 100005, 3, 1, 0, 0, 0, 0), headNotOurs, 0},
		{"NFSv3", xdr(nil).u32(11, rpcCall, rpcVersion, nfsProgram, 3, 1, 0, 0, 0, 0), headNotOurs, 0},
		{"another procedure", xdr(nil).u32(12, rpcCall, rpcVersion, nfsProgram, 4, 2, 0, 0, 0, 0), headNotOurs, 0},
		{"a reply", xdr(nil).u32(13, rpcReply, 0, 0, 0, 0), headNotOurs, 0},
		{"minor version 1", xdr(nil).u32(14, rpcCall, rpcVersion, nfsProgram, 4, 1, 0, 0, 0, 0).opaque(nil).u32(1, 1, 53), headNotOurs, 0},
		{"no operations", xdr(nil).u32(15, rpcCall, rpcVersion, nfsProgram, 4, 1, 0, 0, 0, 0).opaque(nil).u32(0, 0), headNotOurs, 0},
		{"a credential past the bound", xdr(nil).u32(16, rpcCall, rpcVersion, nfsProgram, 4, 1, 1, headMax+4), headNotOurs, 0},
	}
	for _, tc := range cases {
		c, r := parseHead(tc.call, true)
		if r != tc.want || (r == headOurs && (c.op != tc.op || c.xid != binary.BigEndian.Uint32(tc.call))) {
			t.Errorf("%s whole: %v %+v, want %v op %d", tc.name, r, c, tc.want, tc.op)
		}
		for n := 0; n < len(tc.call); n++ {
			_, r := parseHead(tc.call[:n], false)
			if r != headMore {
				if r != tc.want {
					t.Errorf("%s at %d bytes: %v, want %v", tc.name, n, r, tc.want)
				}
				break
			}
			if _, r := parseHead(tc.call[:n], true); r != headNotOurs {
				t.Errorf("%s ended at %d bytes: %v, want not ours", tc.name, n, r)
			}
		}
	}
	long := xdr(nil).u32(17, rpcCall, rpcVersion, nfsProgram, 4, 1, 1, headMax-4)
	long = append(long, make([]byte, headMax)...)
	if _, r := parseHead(long[:headMax], false); r != headNotOurs {
		t.Errorf("a head of headMax bytes: %v", r)
	}
}

type reply struct {
	xid, msg, accept     uint32
	verfFlavor, verfLen  uint32
	status               uint32
	tag                  string
	n, op, opStatus      uint32
	body                 int
	compound, wellFormed bool
}

func decodeReply(t *testing.T, b []byte, compound bool) reply {
	t.Helper()
	var r reply
	if len(b) < 4 {
		t.Fatalf("no mark: %x", b)
	}
	mark := binary.BigEndian.Uint32(b)
	if mark&0x80000000 == 0 || int(mark&0x7fffffff) != len(b)-4 {
		t.Fatalf("mark %08x for %d bytes", mark, len(b)-4)
	}
	p := b[4:]
	r.body = len(p)
	off := 0
	u32 := func() uint32 {
		if off+4 > len(p) {
			t.Fatalf("short reply at %d: %x", off, p)
		}
		v := binary.BigEndian.Uint32(p[off:])
		off += 4
		return v
	}
	r.xid, r.msg = u32(), u32()
	r.accept = u32()
	r.verfFlavor, r.verfLen = u32(), u32()
	off += int(r.verfLen+3) &^ 3
	if st := u32(); st != acceptSuccess {
		t.Fatalf("accept_stat %d", st)
	}
	if compound {
		r.compound = true
		r.status = u32()
		n := u32()
		if off+int(n) > len(p) {
			t.Fatalf("tag of %d bytes past the end", n)
		}
		r.tag = string(p[off : off+int(n)])
		off += int(n+3) &^ 3
		r.n = u32()
		r.op, r.opStatus = u32(), u32()
	}
	r.wellFormed = off == len(p)
	return r
}

func TestTheAnswersBytesAreWhatTheMacsClientReads(t *testing.T) {
	for _, tc := range []struct {
		call     []byte
		compound bool
		status   uint32
		op       uint32
		tag      string
	}{
		{macCall(0x363d5e68, procCompound, "getattr     ", 22, 9), true, nfs4ErrDelay, 22, "getattr     "},
		{macCall(0x363d5e6e, procCompound, "renew       ", opRenew), true, nfs4OK, opRenew, "renew       "},
		{macCall(0x363d5e6f, procCompound, "rd", 22, 26), true, nfs4ErrDelay, 22, "rd"},
		{macCall(0x363d5e70, procCompound, "", 24, 10), true, nfs4ErrDelay, 24, ""},
		{macCall(0x363d5e71, procCompound, "renew+", opRenew, 9), true, nfs4ErrDelay, opRenew, "renew+"},
		{macCall(0x363d5e11, procNull, ""), false, 0, 0, ""},
	} {
		c, r := parseHead(tc.call, true)
		if r != headOurs {
			t.Fatalf("%x: %v", tc.call, r)
		}
		got := decodeReply(t, pausedAnswer(c), tc.compound)
		want := reply{xid: binary.BigEndian.Uint32(tc.call), msg: rpcReply, accept: msgAccepted, verfFlavor: authNone, body: got.body, wellFormed: true}
		if tc.compound {
			want.compound, want.status, want.tag, want.n, want.op, want.opStatus = true, tc.status, tc.tag, 1, tc.op, tc.status
		}
		if got != want {
			t.Errorf("tag %q: %+v\nwant %+v", tc.tag, got, want)
		}
	}
	c, _ := parseHead(macCall(1, procNull, ""), true)
	if n := len(pausedAnswer(c)); n != 4+24 {
		t.Errorf("NULL's answer: %d bytes", n)
	}
}

type fed struct {
	fwd     []byte
	answers []uint32
	called  int
}

func feedAll(stream []byte, steps []int, pausedAt func(i int) bool) fed {
	var u upstream
	var f fed
	i := 0
	k := 0
	for p := stream; len(p) > 0; k++ {
		n := min(steps[k%len(steps)], len(p))
		fwd, answers := u.feed(p[:n], func() bool { b := pausedAt(i); i++; return b }, func() { f.called++ })
		for _, b := range fwd {
			f.fwd = append(f.fwd, b...)
		}
		for _, c := range answers {
			f.answers = append(f.answers, c.xid)
		}
		p = p[n:]
	}
	return f
}

func TestTheClientsCallsAreFramedHoweverTheyArrive(t *testing.T) {
	calls := [][]byte{
		record(macCall(1, procCompound, "getattr     ", 22, 9), 1<<20),
		record(macCall(2, procCompound, "renew       ", opRenew), 20),
		record(macCall(3, procNull, ""), 1<<20),
		record(xdr(nil).u32(4, rpcCall, rpcVersion, 100005, 3, 1, 0, 0, 0, 0), 1<<20),
		append(binary.BigEndian.AppendUint32(nil, uint32(len(macCall(5, procCompound, "x", 22, 25)))), append(macCall(5, procCompound, "x", 22, 25), 0x80, 0, 0, 0)...),
		record(append(macCall(6, procCompound, "read        ", 22, 25), make([]byte, 300)...), 1<<20),
	}
	var stream []byte
	for _, c := range calls {
		stream = append(stream, c...)
	}
	stepsets := [][]int{{len(stream)}, {1}, {3}, {7}, {5, 200, 1, 64}, {4}, {40}}
	for _, pattern := range []struct {
		name   string
		paused func(i int) bool
	}{
		{"running", func(int) bool { return false }},
		{"paused", func(int) bool { return true }},
		{"paused from the third", func(i int) bool { return i >= 2 }},
		{"paused for the second and fifth", func(i int) bool { return i == 1 || i == 4 }},
	} {
		for _, steps := range stepsets {
			f := feedAll(stream, steps, pattern.paused)
			var wantFwd []byte
			var wantAns []uint32
			wantCalled := 0
			for i, c := range calls {
				if pattern.paused(i) && i != 3 {
					wantAns = append(wantAns, uint32(i+1))
					continue
				}
				wantFwd = append(wantFwd, c...)
				wantCalled++
			}
			name := fmt.Sprintf("%s by %v", pattern.name, steps)
			if !bytes.Equal(f.fwd, wantFwd) {
				t.Errorf("%s: forwarded %d bytes, want %d", name, len(f.fwd), len(wantFwd))
			}
			if !slices.Equal(f.answers, wantAns) {
				t.Errorf("%s: answered %v, want %v", name, f.answers, wantAns)
			}
			if f.called != wantCalled {
				t.Errorf("%s: %d calls counted out, want %d", name, f.called, wantCalled)
			}
		}
	}
}

func TestACallBegunBeforeThePauseGoesOnToTheGuest(t *testing.T) {
	first := record(append(macCall(1, procCompound, "write       ", 22, 38), make([]byte, 5000)...), 1<<20)
	second := record(macCall(2, procCompound, "getattr     ", 22, 9), 1<<20)
	var u upstream
	paused := false
	var fwd []byte
	var answers []uint32
	feed := func(p []byte) {
		f, a := u.feed(p, func() bool { return paused }, func() {})
		for _, b := range f {
			fwd = append(fwd, b...)
		}
		for _, c := range a {
			answers = append(answers, c.xid)
		}
	}
	feed(first[:100])
	paused = true
	feed(first[100:])
	feed(second)
	if !bytes.Equal(fwd, first) || !slices.Equal(answers, []uint32{2}) {
		t.Fatalf("forwarded %d bytes of %d, answered %v", len(fwd), len(first), answers)
	}
}

type guestEnd struct {
	mu    sync.Mutex
	got   []byte
	conns []*net.UnixConn
	opens chan struct{}
}

func (g *guestEnd) serve(c *net.UnixConn) {
	g.mu.Lock()
	g.conns = append(g.conns, c)
	g.mu.Unlock()
	g.opens <- struct{}{}
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		g.mu.Lock()
		g.got = append(g.got, buf[:n]...)
		g.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (g *guestEnd) received() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.got)
}

func (g *guestEnd) conn(t *testing.T) *net.UnixConn {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.conns) == 0 {
		t.Fatal("no stream opened")
	}
	return g.conns[len(g.conns)-1]
}

func pausedServer(t *testing.T) (*Server, *guestEnd) {
	t.Helper()
	g := &guestEnd{opens: make(chan struct{}, 8)}
	s, err := Listen(filepath.Join(shortDir(t), "view.sock"), &pairOpener{serve: g.serve}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, g
}

func readRecord(t *testing.T, c net.Conn) []byte {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(time.Second))
	defer c.SetReadDeadline(time.Time{})
	var mark [4]byte
	if _, err := io.ReadFull(c, mark[:]); err != nil {
		t.Fatalf("no reply: %v", err)
	}
	body := make([]byte, binary.BigEndian.Uint32(mark[:])&0x7fffffff)
	if _, err := io.ReadFull(c, body); err != nil {
		t.Fatal(err)
	}
	return append(mark[:], body...)
}

func nothingFor(t *testing.T, c net.Conn, d time.Duration) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(d))
	defer c.SetReadDeadline(time.Time{})
	b := make([]byte, 1)
	if n, err := c.Read(b); n > 0 || err == nil {
		t.Fatalf("read %d bytes, want nothing", n)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("not so: %s", what)
		}
	}
}

func TestAPausedVMsCallsAreAnsweredAndNoneForwarded(t *testing.T) {
	s, g := pausedServer(t)
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-g.opens
	before := record(macCall(1, procCompound, "read        ", 22, 25), 1<<20)
	c.Write(before)
	eventually(t, "the first call reached the guest", func() bool { return bytes.Equal(g.received(), before) })
	s.SetPaused(true)
	if n := s.Out(); n != 1 {
		t.Fatalf("%d calls out at the pause, want 1", n)
	}
	c.Write(record(macCall(2, procNull, ""), 1<<20))
	if r := decodeReply(t, readRecord(t, c), false); r.xid != 2 || !r.wellFormed {
		t.Fatalf("NULL: %+v", r)
	}
	c.Write(append(record(macCall(3, procCompound, "getattr     ", 22, 9), 1<<20), record(macCall(4, procCompound, "renew       ", opRenew), 7)...))
	if r := decodeReply(t, readRecord(t, c), true); r.xid != 3 || r.status != nfs4ErrDelay || r.op != 22 {
		t.Fatalf("GETATTR: %+v", r)
	}
	if r := decodeReply(t, readRecord(t, c), true); r.xid != 4 || r.status != nfs4OK || r.op != opRenew {
		t.Fatalf("RENEW: %+v", r)
	}
	if got := g.received(); !bytes.Equal(got, before) {
		t.Fatalf("the guest was sent %d bytes while paused", len(got)-len(before))
	}
	if n := s.Answered(); n != 3 {
		t.Fatalf("answered %d", n)
	}
	s.SetPaused(false)
	g.conn(t).Write(record(xdr(nil).u32(1, rpcReply, 0, 0, 0, 0, 0), 1<<20))
	if r := readRecord(t, c); binary.BigEndian.Uint32(r[4:]) != 1 {
		t.Fatalf("the guest's reply: %x", r)
	}
	after := record(macCall(5, procCompound, "getattr     ", 22, 9), 1<<20)
	c.Write(after)
	eventually(t, "the call after the resume reached the guest", func() bool { return bytes.Equal(g.received(), append(slices.Clone(before), after...)) })
	nothingFor(t, c, 50*time.Millisecond)
}

func TestAConnectionMadeWhilePausedGetsItsStreamAtItsFirstForward(t *testing.T) {
	s, g := pausedServer(t)
	s.SetPaused(true)
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(record(macCall(1, procNull, ""), 1<<20))
	readRecord(t, c)
	c.Write(record(macCall(2, procCompound, "setclid     ", 35), 1<<20))
	if r := decodeReply(t, readRecord(t, c), true); r.status != nfs4ErrDelay {
		t.Fatalf("SETCLIENTID: %+v", r)
	}
	select {
	case <-g.opens:
		t.Fatal("a stream was opened to a paused guest")
	case <-time.After(50 * time.Millisecond):
	}
	s.SetPaused(false)
	call := record(macCall(3, procCompound, "setclid     ", 35), 1<<20)
	c.Write(call)
	select {
	case <-g.opens:
	case <-time.After(time.Second):
		t.Fatal("no stream at the first forward")
	}
	eventually(t, "the call reached the guest", func() bool { return bytes.Equal(g.received(), call) })
	g.conn(t).Write(record(xdr(nil).u32(3, rpcReply, 0, 0, 0, 0, 0), 1<<20))
	if r := readRecord(t, c); binary.BigEndian.Uint32(r[4:]) != 3 {
		t.Fatalf("the guest's reply: %x", r)
	}
	c.(*net.UnixConn).CloseWrite()
	eventually(t, "the guest's stream ended", func() bool {
		g.conn(t).SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		_, err := g.conn(t).Read(make([]byte, 1))
		return err == io.EOF || (err != nil && !isTimeout(err))
	})
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

func TestAnAnswerWaitsForAReplyTheGuestLeftHalfSent(t *testing.T) {
	s, g := pausedServer(t)
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-g.opens
	c.Write(record(macCall(1, procCompound, "read        ", 22, 25), 1<<20))
	eventually(t, "the call reached the guest", func() bool { return len(g.received()) > 0 })
	reply := record(append(xdr(nil).u32(1, rpcReply, 0, 0, 0, 0, 0), make([]byte, 1000)...), 1<<20)
	g.conn(t).Write(reply[:300])
	got := make([]byte, 300)
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Time{})
	s.SetPaused(true)
	c.Write(record(macCall(2, procCompound, "getattr     ", 22, 9), 1<<20))
	nothingFor(t, c, 100*time.Millisecond)
	g.conn(t).Write(reply[300:])
	rest := make([]byte, len(reply)-300)
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(c, rest); err != nil || !bytes.Equal(append(got, rest...), reply) {
		t.Fatalf("the guest's reply: %v", err)
	}
	if r := decodeReply(t, readRecord(t, c), true); r.xid != 2 || r.status != nfs4ErrDelay {
		t.Fatalf("the answer after it: %+v", r)
	}
}

func TestNoStallIsToldOfCallsAnsweredWhilePaused(t *testing.T) {
	old := StallAfter
	StallAfter = 50 * time.Millisecond
	defer func() { StallAfter = old }()
	s, g := pausedServer(t)
	told := make(chan struct{}, 16)
	s.OnStall(func() bool { told <- struct{}{}; return false })
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-g.opens
	s.SetPaused(true)
	for i := range 5 {
		c.Write(record(macCall(uint32(i), procCompound, "getattr     ", 22, 9), 1<<20))
		readRecord(t, c)
	}
	select {
	case <-told:
		t.Fatal("told of a stall while every call was answered")
	case <-time.After(4 * StallAfter):
	}
	if s.Out() != 0 {
		t.Fatalf("%d out", s.Out())
	}
}

func TestACallInFlightAtThePauseIsWaitedForFromItsFirstByte(t *testing.T) {
	s, g := pausedServer(t)
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-g.opens
	if n := s.WaitQuiet(time.Now().Add(time.Second)); n != 0 {
		t.Fatalf("%d in flight with nothing sent", n)
	}
	read := record(macCall(1, procCompound, "read        ", 22, 25), 1<<20)
	write := record(append(macCall(2, procCompound, "write       ", 22, 38), make([]byte, 5000)...), 1<<20)
	c.Write(append(slices.Clone(read), write[:100]...))
	eventually(t, "the calls reached the guest", func() bool { return len(g.received()) == len(read)+100 })
	s.SetPaused(true)
	t0 := time.Now()
	if n := s.WaitQuiet(t0.Add(50 * time.Millisecond)); n != 2 {
		t.Fatalf("%d in flight at the deadline, want 2", n)
	}
	if d := time.Since(t0); d < 50*time.Millisecond || d > 500*time.Millisecond {
		t.Fatalf("waited %v for a 50 ms deadline", d)
	}
	c.Write(write[100:])
	c.Write(record(macCall(3, procCompound, "getattr     ", 22, 9), 1<<20))
	if r := decodeReply(t, readRecord(t, c), true); r.xid != 3 || r.status != nfs4ErrDelay {
		t.Fatalf("the new call: %+v", r)
	}
	eventually(t, "the write's rest reached the guest", func() bool { return bytes.Equal(g.received(), append(slices.Clone(read), write...)) })
	done := make(chan int)
	go func() { done <- s.WaitQuiet(time.Now().Add(5 * time.Second)) }()
	g.conn(t).Write(record(xdr(nil).u32(1, rpcReply, 0, 0, 0, 0, 0), 1<<20))
	readRecord(t, c)
	select {
	case n := <-done:
		t.Fatalf("the wait ended with %d in flight, one reply in", n)
	case <-time.After(50 * time.Millisecond):
	}
	g.conn(t).Write(record(xdr(nil).u32(2, rpcReply, 0, 0, 0, 0, 0), 1<<20))
	readRecord(t, c)
	select {
	case n := <-done:
		if n != 0 {
			t.Fatalf("%d in flight after both replies", n)
		}
	case <-time.After(time.Second):
		t.Fatal("the wait did not end at the last reply")
	}
	c.Write(record(macCall(4, procCompound, "read        ", 22, 25), 1<<20))
	readRecord(t, c)
	s.SetPaused(false)
	c.Write(record(macCall(5, procCompound, "read        ", 22, 25), 1<<20))
	eventually(t, "the call after the resume reached the guest", func() bool { return len(g.received()) == len(read)+len(write)+len(read) })
	c.Close()
	g.conn(t).Close()
	if n := s.WaitQuiet(time.Now().Add(time.Second)); n != 0 {
		t.Fatalf("%d in flight after the connection ended", n)
	}
}
