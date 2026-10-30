package netstack

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/header"
)

type vmmEnd struct {
	t         testing.TB
	conn      *net.UnixConn
	region    *region
	toStack   producer
	fromStack consumer
	sends     *largest

	wake    chan struct{}
	reading sync.Once
}

func socketPair(t testing.TB) (*net.UnixConn, *vmmEnd) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conns := make([]*net.UnixConn, 2)
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "link")
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c.(*net.UnixConn)
	}

	const capacity = 1 << 20
	f, err := os.CreateTemp(t.TempDir(), "ring")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	os.Remove(f.Name())
	if err := f.Truncate(int64(ringHeaderLen + 2*capacity)); err != nil {
		t.Fatal(err)
	}
	r, err := mapRegion(int(f.Fd()), capacity)
	if err != nil {
		t.Fatal(err)
	}
	hello := append([]byte(ringMagic), make([]byte, 8)...)
	binary.LittleEndian.PutUint32(hello[8:], capacity)
	if _, _, err := conns[1].WriteMsgUnix(hello, syscall.UnixRights(int(f.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	v := &vmmEnd{
		t: t, conn: conns[1], region: r,
		toStack: r.producer(guestToNetstack), fromStack: r.consumer(netstackToGuest),
		wake: make(chan struct{}, 1),
	}
	t.Cleanup(func() { v.Close() })
	return conns[0], v
}

func (v *vmmEnd) Close() { v.conn.Close() }

func (v *vmmEnd) endpoint() *linkEndpoint {
	ep := newLinkEndpoint(v.conn, guestMAC)
	ep.region = v.region
	ep.received = v.fromStack
	ep.sent = v.toStack
	if v.sends != nil {
		v.sends.watch(ep)
	}
	return ep
}

func (v *vmmEnd) wait(deadline time.Time) bool {
	v.reading.Do(func() {
		go func() {
			var buf [64]byte
			for {
				if _, err := v.conn.Read(buf[:]); err != nil {
					return
				}
				select {
				case v.wake <- struct{}{}:
				default:
				}
			}
		}()
	})
	select {
	case <-v.wake:
		return true
	case <-time.After(time.Until(deadline)):
		return false
	}
}

func (v *vmmEnd) send(msg []byte) {
	v.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ok, err := v.toStack.hasRoom(len(msg))
		if err != nil {
			v.t.Fatal(err)
		}
		if ok {
			break
		}
		if full, _ := v.toStack.waitForRoom(len(msg)); full && !v.wait(deadline) {
			v.t.Fatal("the stack never made room in the ring")
		}
	}
	v.toStack.begin(len(msg))
	v.toStack.write(0, msg)
	if v.toStack.publish(len(msg)) {
		v.conn.Write(wakeByte)
	}
}

func (v *vmmEnd) sendFrame(frame []byte) {
	v.t.Helper()
	v.send(append(make([]byte, netHdrLen), frame...))
}

func (v *vmmEnd) recv(deadline time.Time) ([]byte, bool) {
	v.t.Helper()
	for {
		n, ok, err := v.fromStack.peek()
		if err != nil {
			v.t.Fatal(err)
		}
		if ok {
			msg := make([]byte, n)
			v.fromStack.read(0, msg)
			if v.fromStack.advance(n) {
				v.conn.Write(wakeByte)
			}
			return msg, true
		}
		if v.fromStack.sleepIfEmpty() && !v.wait(deadline) {
			return nil, false
		}
	}
}

func newWatched(conn net.Conn, opts Options, sends *largest) (*Stack, error) {
	var onSend func(int)
	if sends != nil {
		probe := &linkEndpoint{}
		sends.watch(probe)
		onSend = probe.onSend
	}
	return newStack(conn, opts, onSend)
}

func ringPair(t *testing.T, capacity int) (producer, consumer) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "ring")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(int64(ringHeaderLen + 2*capacity)); err != nil {
		t.Fatal(err)
	}
	r, err := mapRegion(int(f.Fd()), capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.unmap)
	return r.producer(0), r.consumer(0)
}

func put(p *producer, msg []byte) (fits, wake bool) {
	if ok, err := p.hasRoom(len(msg)); err != nil || !ok {
		return false, false
	}
	p.begin(len(msg))
	p.write(0, msg)
	return true, p.publish(len(msg))
}

func TestRingMessagesComeOutAsTheyWentInAcrossTheWrap(t *testing.T) {
	p, c := ringPair(t, 1<<16)
	for round := range 2000 {
		msg := make([]byte, (round*37)%5000)
		for i := range msg {
			msg[i] = byte(i + round)
		}
		if fits, wake := put(&p, msg); !fits || wake {
			t.Fatalf("round %d: fits=%v wake=%v", round, fits, wake)
		}
		n, ok, err := c.peek()
		if err != nil || !ok || n != len(msg) {
			t.Fatalf("round %d: peek = %d, %v, %v", round, n, ok, err)
		}
		got := make([]byte, n)
		c.read(0, got)
		if string(got) != string(msg) {
			t.Fatalf("round %d: message changed in the ring", round)
		}
		if c.advance(n) {
			t.Fatal("nobody was waiting for room")
		}
	}
}

func TestRingSaysWhenTheOtherSideMustBeWoken(t *testing.T) {
	p, c := ringPair(t, 1<<16)
	if !c.sleepIfEmpty() {
		t.Fatal("an empty ring did not let the consumer sleep")
	}
	if fits, wake := put(&p, []byte("one")); !fits || !wake {
		t.Fatal("the sleeping consumer was not owed a wake-up")
	}
	if _, wake := put(&p, []byte("two")); wake {
		t.Fatal("a second wake-up for a consumer already woken")
	}
	c.advance(3)
	c.advance(3)
	put(&p, []byte("three"))
	if c.sleepIfEmpty() {
		t.Fatal("the consumer slept with a message waiting")
	}
	c.advance(5)

	msg := make([]byte, 4092)
	held := 0
	for {
		if fits, _ := put(&p, msg); !fits {
			break
		}
		held++
	}
	if held != 16 {
		t.Fatalf("a 64 KiB ring held %d messages of 4 KiB", held)
	}
	if full, err := p.waitForRoom(len(msg)); !full || err != nil {
		t.Fatalf("waitForRoom = %v, %v on a full ring", full, err)
	}
	if !c.advance(len(msg)) {
		t.Fatal("the waiting producer was not owed a wake-up")
	}
	if ok, _ := p.hasRoom(len(msg)); !ok {
		t.Fatal("no room after the consumer advanced")
	}
}

func TestRingImpossibleValuesAreErrorsNotBelieved(t *testing.T) {
	p, c := ringPair(t, 1<<16)
	for _, tail := range []uint64{1<<16 + 8, 13} {
		c.tail.Store(tail)
		if _, _, err := c.peek(); err == nil {
			t.Fatalf("a tail of %d was believed", tail)
		}
	}
	c.tail.Store(16)
	for _, n := range []uint32{4000, 0xffffffff} {
		binary.LittleEndian.PutUint32(c.data, n)
		if _, _, err := c.peek(); err == nil {
			t.Fatalf("a length of %d was believed", n)
		}
	}
	p.head.Store(1 << 40)
	if _, err := p.hasRoom(8); err == nil {
		t.Fatal("a head ahead of the tail was believed")
	}
}

func TestAVMMThatCorruptsTheSharedMemoryEndsTheLink(t *testing.T) {
	hostEnd, guestEnd := socketPair(t)
	ns, err := New(hostEnd, Options{Host: newFakeHost()})
	if err != nil {
		t.Fatal(err)
	}
	guestEnd.sendFrame(make([]byte, header.EthernetMinimumSize+28))
	time.Sleep(20 * time.Millisecond)
	guestEnd.toStack.tail.Store(1 << 50)
	guestEnd.conn.Write(wakeByte)
	select {
	case <-ns.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("link still up after an impossible index")
	}
}

func TestAHelloWithoutMemoryIsRefused(t *testing.T) {
	for name, hello := range map[string][]byte{
		"wrong magic": append([]byte("NOTFEV"), make([]byte, 8)...),
		"no memory":   append([]byte(ringMagic), 0, 0, 16, 0, 0, 0, 0, 0),
	} {
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		files := []*os.File{os.NewFile(uintptr(fds[0]), "a"), os.NewFile(uintptr(fds[1]), "b")}
		a, _ := net.FileConn(files[0])
		b, _ := net.FileConn(files[1])
		files[0].Close()
		files[1].Close()
		ns, err := New(a, Options{Host: newFakeHost()})
		if err != nil {
			t.Fatal(err)
		}
		b.Write(hello)
		select {
		case <-ns.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the link stayed up", name)
		}
		b.Close()
	}
}

func TestAFastStreamThatPausesCarriesOn(t *testing.T) {
	w := newWorld(t)
	c, err := w.dialTCP(t, "93.184.215.14:80")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte(i*131 + i>>9)
	}
	got := make([]byte, len(payload))
	for burst := range 3 {
		go c.Write(payload)
		if _, err := io.ReadFull(c, got); err != nil {
			t.Fatalf("burst %d: %v", burst, err)
		}
		if string(got) != string(payload) {
			t.Fatalf("burst %d came back changed", burst)
		}
		time.Sleep(3 * largeBufferIdle)
	}
	c.CloseWrite()
	if rest, err := io.ReadAll(c); err != nil || string(rest) != "<eof seen>" {
		t.Fatalf("after the last burst: %q, %v", rest, err)
	}
}
