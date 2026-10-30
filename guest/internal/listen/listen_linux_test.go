//go:build linux

package listen

import (
	"errors"
	"net/netip"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type rig struct {
	t      *testing.T
	w      *Watcher
	ns     uint64
	events chan Event
	next   int
}

func attach(t *testing.T) *rig {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: the programs are attached at /sys/fs/cgroup (tests/run unit guest runs this in a privileged container)")
	}
	w, err := Attach("/sys/fs/cgroup")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	r := &rig{t: t, w: w, events: make(chan Event, 256), next: 41000}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	r.ns, err = unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
	unix.Close(fd)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(func(ev Event) {
			if ev.NetNS == r.ns {
				r.events <- ev
			}
		})
	}()
	t.Cleanup(func() {
		if n := w.Lost(); n != 0 {
			t.Errorf("%d records lost", n)
		}
		w.Close()
		<-done
	})
	return r
}

func (r *rig) socket(family, typ int) int {
	r.t.Helper()
	fd, err := unix.Socket(family, typ|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { unix.Close(fd) })
	return fd
}

func sa4(addr string, port int) *unix.SockaddrInet4 {
	return &unix.SockaddrInet4{Port: port, Addr: netip.MustParseAddr(addr).As4()}
}

func sa6(addr string, port int) *unix.SockaddrInet6 {
	return &unix.SockaddrInet6{Port: port, Addr: netip.MustParseAddr(addr).As16()}
}

func localPort(t *testing.T, fd int) uint16 {
	t.Helper()
	sa, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	switch a := sa.(type) {
	case *unix.SockaddrInet4:
		return uint16(a.Port)
	case *unix.SockaddrInet6:
		return uint16(a.Port)
	}
	t.Fatalf("getsockname: %T", sa)
	return 0
}

func (r *rig) expect(what string, added bool, proto string, port uint16, cookie uint64) {
	r.t.Helper()
	select {
	case ev := <-r.events:
		if ev.Added != added || ev.Proto != proto || ev.Port != port || (cookie != 0 && ev.Cookie != cookie) {
			r.t.Fatalf("%s: got %+v, want added=%v %s port %d", what, ev, added, proto, port)
		}
	case <-time.After(3 * time.Second):
		r.t.Fatalf("%s: no event in 3 s (want added=%v %s port %d)", what, added, proto, port)
	}
}

func (r *rig) nothing(what string) {
	r.t.Helper()
	r.next++
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := unix.Bind(fd, sa4("127.0.0.1", r.next)); err != nil {
		unix.Close(fd)
		r.t.Fatal(err)
	}
	c, _ := SocketCookie(fd)
	r.expect(what+" (then a sentinel bound by number)", true, UDP, uint16(r.next), c)
	unix.Close(fd)
	r.expect(what+" (the sentinel's release)", false, UDP, uint16(r.next), c)
}

func TestAUDPSocketIsToldOnlyWhenBoundByNumber(t *testing.T) {
	r := attach(t)

	fd := r.socket(unix.AF_INET, unix.SOCK_DGRAM)
	if err := unix.Bind(fd, sa4("0.0.0.0", 0)); err != nil {
		t.Fatal(err)
	}
	unix.Sendto(fd, []byte("query"), 0, sa4("127.0.0.1", 9))
	unix.Close(fd)
	fd = r.socket(unix.AF_INET6, unix.SOCK_DGRAM)
	if err := unix.Bind(fd, sa6("::", 0)); err != nil {
		t.Fatal(err)
	}
	fd = r.socket(unix.AF_INET, unix.SOCK_DGRAM)
	if err := unix.Bind(fd, sa4("127.0.0.1", 0)); err != nil {
		t.Fatal(err)
	}
	r.nothing("bound to port 0")

	fd = r.socket(unix.AF_INET, unix.SOCK_DGRAM)
	if err := unix.Sendto(fd, []byte("x"), 0, sa4("127.0.0.1", 9)); err != nil {
		t.Fatal(err)
	}
	fd = r.socket(unix.AF_INET6, unix.SOCK_DGRAM)
	if err := unix.Connect(fd, sa6("::1", 9)); err != nil && !errors.Is(err, unix.ENETUNREACH) && !errors.Is(err, unix.EADDRNOTAVAIL) {
		t.Fatal(err)
	}
	fd = r.socket(unix.AF_INET, unix.SOCK_DGRAM)
	if err := unix.Connect(fd, sa4("127.0.0.1", 9)); err != nil {
		t.Fatal(err)
	}
	r.nothing("bound by a sendto or a connect")

	fd = r.socket(unix.AF_INET, unix.SOCK_DGRAM)
	if err := unix.Bind(fd, sa4("0.0.0.0", 40001)); err != nil {
		t.Fatal(err)
	}
	c, _ := SocketCookie(fd)
	r.expect("bound by number", true, UDP, 40001, c)
	if err := unix.Connect(fd, sa4("127.0.0.1", 9)); err != nil {
		t.Fatal(err)
	}
	r.nothing("bound by number, then connected")
	unix.Close(fd)
	r.expect("the connected socket's close", false, UDP, 40001, c)

	fd6 := r.socket(unix.AF_INET6, unix.SOCK_DGRAM)
	if err := unix.Bind(fd6, sa6("::", 40002)); err != nil {
		t.Fatal(err)
	}
	c6, _ := SocketCookie(fd6)
	r.expect("bound by number (IPv6)", true, UDP, 40002, c6)
	if err := unix.Connect(fd6, sa6("::1", 9)); err != nil && !errors.Is(err, unix.ENETUNREACH) && !errors.Is(err, unix.EADDRNOTAVAIL) {
		t.Fatal(err)
	}
	r.nothing("bound by number, then connected (IPv6)")
	unix.Close(fd6)
	r.expect("closed (IPv6)", false, UDP, 40002, c6)

	hold := r.socket(unix.AF_INET, unix.SOCK_DGRAM)
	if err := unix.Bind(hold, sa4("127.0.0.1", 40003)); err != nil {
		t.Fatal(err)
	}
	hc, _ := SocketCookie(hold)
	r.expect("the holder", true, UDP, 40003, hc)
	fd = r.socket(unix.AF_INET, unix.SOCK_DGRAM)
	if err := unix.Bind(fd, sa4("127.0.0.1", 40003)); !errors.Is(err, unix.EADDRINUSE) {
		t.Fatalf("a second bind of 40003: %v", err)
	}
	if err := unix.Bind(fd, sa4("127.0.0.1", 0)); err != nil {
		t.Fatal(err)
	}
	r.nothing("port 0 after a failed bind by number")
	unix.Close(fd)
	r.nothing("that socket's close")

	fd = r.socket(unix.AF_INET, unix.SOCK_DGRAM)
	if err := unix.Bind(fd, sa4("127.0.0.1", 40003)); !errors.Is(err, unix.EADDRINUSE) {
		t.Fatalf("a second bind of 40003: %v", err)
	}
	if err := unix.Bind(fd, sa4("127.0.0.1", 40004)); err != nil {
		t.Fatal(err)
	}
	c, _ = SocketCookie(fd)
	r.expect("bound by number after a failed one", true, UDP, 40004, c)
	if err := unix.Bind(fd, sa4("127.0.0.1", 0)); !errors.Is(err, unix.EINVAL) {
		t.Fatalf("a bound socket bound again: %v", err)
	}
	unix.Close(fd)
	r.expect("its close", false, UDP, 40004, c)
	unix.Close(hold)
	r.expect("the holder's close", false, UDP, 40003, hc)

	tfd := r.socket(unix.AF_INET, unix.SOCK_STREAM)
	if err := unix.Bind(tfd, sa4("127.0.0.1", 0)); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(tfd, 4); err != nil {
		t.Fatal(err)
	}
	tc, _ := SocketCookie(tfd)
	tp := localPort(t, tfd)
	r.expect("a TCP listen on port 0", true, TCP, tp, tc)
	unix.Close(tfd)
	r.expect("its close", false, TCP, tp, tc)
}
