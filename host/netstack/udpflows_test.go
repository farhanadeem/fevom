package netstack

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

type movingMac struct {
	*fakeHost
	mu    sync.Mutex
	local netip.Addr
}

func (m *movingMac) DialUDP(ctx context.Context, addr netip.AddrPort) (UDPConn, error) {
	c, err := m.fakeHost.DialUDP(ctx, addr)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return localUDPConn{UDPConn: c, local: &net.UDPAddr{IP: m.local.AsSlice(), Port: int(c.LocalPort())}}, nil
}

type localUDPConn struct {
	UDPConn
	local *net.UDPAddr
}

func (c localUDPConn) LocalAddr() net.Addr { return c.local }

type macAddrs []netip.Addr

func (m macAddrs) HasAddress(a netip.Addr) bool {
	for _, x := range m {
		if x == a {
			return true
		}
	}
	return false
}

func TestAUDPFlowFromAnAddressTheMacNoLongerHasIsEndedAndTheNextDatagramOpensAnother(t *testing.T) {
	hostEnd, guestEnd := socketPair(t)
	wifi, hotspot := netip.MustParseAddr("172.16.30.211"), netip.MustParseAddr("172.20.10.4")
	mac := &movingMac{fakeHost: newFakeHost(), local: wifi}
	ns, err := New(hostEnd, Options{Host: mac})
	if err != nil {
		t.Fatal(err)
	}
	g := newGuestStack(t, guestEnd, false)
	defer func() {
		ns.Close()
		g.Close()
	}()
	conn, err := gonet.DialUDP(g, nil, &tcpip.FullAddress{NIC: 1, Addr: toAddr(netip.MustParseAddr("1.1.1.1")), Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	roundTrip := func(msg string) {
		t.Helper()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil || string(buf[:n]) != msg {
			t.Fatalf("echo of %q: %q, %v", msg, buf[:n], err)
		}
	}
	dials := func() int {
		mac.fakeHost.mu.Lock()
		defer mac.fakeHost.mu.Unlock()
		return len(mac.fakeHost.udpDials)
	}
	roundTrip("one")
	if n := ns.MacAddressesChanged(macAddrs{wifi, netip.MustParseAddr("127.0.0.1")}); n != 0 || dials() != 1 {
		t.Fatalf("with the address still the Mac's: %d ended, %d dials", n, dials())
	}
	roundTrip("two")

	mac.mu.Lock()
	mac.local = hotspot
	mac.mu.Unlock()
	if n := ns.MacAddressesChanged(macAddrs{hotspot, netip.MustParseAddr("127.0.0.1")}); n != 1 {
		t.Fatalf("%d flows ended, want 1", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ns.udpFlows.mu.Lock()
		left := len(ns.udpFlows.flows)
		ns.udpFlows.mu.Unlock()
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the flow did not end")
		}
		time.Sleep(time.Millisecond)
	}
	roundTrip("three")
	if dials() != 2 {
		t.Fatalf("%d dials, want a second for the datagram after the change", dials())
	}
	ns.udpFlows.mu.Lock()
	defer ns.udpFlows.mu.Unlock()
	for f := range ns.udpFlows.flows {
		if f.local != hotspot {
			t.Fatalf("the new flow's local address is %v", f.local)
		}
	}
}
