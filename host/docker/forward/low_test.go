package forward

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"testing"

	api "fev/proto/daemon"
)

type standInHelper struct {
	mu     sync.Mutex
	asked  []string
	refuse error
}

func (h *standInHelper) BindLow(proto string, family int, port uint16) (*os.File, error) {
	h.mu.Lock()
	h.asked = append(h.asked, fmt.Sprintf("%s%d:%d", proto, family, port))
	refuse := h.refuse
	h.mu.Unlock()
	if refuse != nil {
		return nil, refuse
	}
	host := "127.0.0.1"
	if family == 6 {
		host = "::1"
	}
	addr := net.JoinHostPort(host, "0")
	if proto == TCP {
		l, err := net.Listen(TCP+fmt.Sprint(family), addr)
		if err != nil {
			return nil, err
		}
		defer l.Close()
		return l.(*net.TCPListener).File()
	}
	pc, err := net.ListenPacket(UDP+fmt.Sprint(family), addr)
	if err != nil {
		return nil, err
	}
	defer pc.Close()
	return pc.(*net.UDPConn).File()
}

func (h *standInHelper) calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.asked...)
}

func TestALowPortForThisMacAloneIsBoundOnLoopbackThroughTheHelper(t *testing.T) {
	w := newWorld(t)
	h := &standInHelper{}
	w.table.SetLowBinder(h)
	port := lowPort(t)
	id := w.run("web", "", port, "tcp")
	w.table.Set(api.ForwardDocker, id, []Want{{TCP, port, guest("127.0.0.1", port), false}, {UDP, port, guest("127.0.0.1", port), false}})
	var tcpAddrs, udpAddrs []string
	for _, f := range w.table.List() {
		if f.State != api.ForwardActive {
			t.Fatalf("%+v", f)
		}
		for _, a := range f.Mac {
			ap := netip.MustParseAddrPort(a)
			if !ap.Addr().IsLoopback() {
				t.Errorf("%s/%d listens on %s, not loopback", f.Proto, port, a)
			}
		}
		if f.Proto == TCP {
			tcpAddrs = f.Mac
		} else {
			udpAddrs = f.Mac
		}
	}
	if len(tcpAddrs) != 2 || len(udpAddrs) != 2 {
		t.Fatalf("tcp %v udp %v: want one socket a family", tcpAddrs, udpAddrs)
	}
	want := []string{fmt.Sprintf("tcp4:%d", port), fmt.Sprintf("tcp6:%d", port), fmt.Sprintf("udp4:%d", port), fmt.Sprintf("udp6:%d", port)}
	if got := h.calls(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the helper was asked %v, want %v", got, want)
	}
	for _, addr := range tcpAddrs {
		if body, err := fetch(addr); err != nil || body != "hello from web:80/tcp" {
			t.Errorf("%s: %q, %v", addr, body, err)
		}
	}
	if l, err := listen(TCP, fmt.Sprintf("0.0.0.0:%d", port), false); err != nil {
		t.Errorf("the wildcard address of %d is held: %v", port, err)
	} else {
		l.(*net.TCPListener).Close()
	}

	n := len(h.calls())
	high := freePort(t)
	id2 := w.run("wide", "", high, "tcp")
	w.table.Set(api.ForwardDocker, id2, []Want{{TCP, high, guest("127.0.0.1", high), false}})
	if len(h.calls()) != n {
		t.Errorf("a port from 1024 up asked the helper: %v", h.calls()[n:])
	}
}

func TestALowPortTheHelperRefusesIsBoundAsBefore(t *testing.T) {
	w := newWorld(t)
	w.table.SetLowBinder(&standInHelper{refuse: errors.New("refused")})
	port := lowPort(t)
	id := w.run("web", "", port, "tcp")
	w.table.Set(api.ForwardDocker, id, []Want{{TCP, port, guest("127.0.0.1", port), false}})
	f := w.only()
	if f.State != api.ForwardActive || fmt.Sprint(f.Mac) != fmt.Sprintf("[0.0.0.0:%d [::]:%d]", port, port) {
		t.Fatalf("%+v", f)
	}
	if body, err := fetch(fmt.Sprintf("127.0.0.1:%d", port)); err != nil || body != "hello from web:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
}

func TestAHalfBoundLowPortFallsBackWhole(t *testing.T) {
	w := newWorld(t)
	h := &sixRefused{}
	w.table.SetLowBinder(h)
	port := lowPort(t)
	id := w.run("web", "", port, "tcp")
	w.table.Set(api.ForwardDocker, id, []Want{{TCP, port, guest("127.0.0.1", port), false}})
	f := w.only()
	if fmt.Sprint(f.Mac) != fmt.Sprintf("[0.0.0.0:%d [::]:%d]", port, port) {
		t.Fatalf("%+v", f)
	}
	if h.four == nil {
		t.Fatal("the IPv4 socket was never made")
	}
	addr := h.four.Addr().String()
	h.four.Close()
	l, err := net.Listen("tcp4", addr)
	if err != nil {
		t.Fatalf("the IPv4 socket of the half-bound port was kept: %v", err)
	}
	l.Close()
}

type sixRefused struct {
	standInHelper
	four *net.TCPListener
}

func (h *sixRefused) BindLow(proto string, family int, port uint16) (*os.File, error) {
	if family == 6 {
		return nil, errors.New("::1 refused")
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	h.four = l.(*net.TCPListener)
	return h.four.File()
}
