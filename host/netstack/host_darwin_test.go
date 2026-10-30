//go:build darwin

package netstack

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func TestTheMacsIPv6SocketsCarryHopLimitsAndEcho(t *testing.T) {
	srv, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	defer srv.Close()
	go func() {
		buf := make([]byte, 64)
		for {
			n, from, err := srv.ReadFromUDP(buf)
			if err != nil {
				return
			}
			srv.WriteToUDP(buf[:n], from)
		}
	}()
	var h MacHost
	c, err := h.DialUDP(context.Background(), srv.LocalAddr().(*net.UDPAddr).AddrPort())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SetTTL(7); err != nil {
		t.Fatalf("hop limit on a udp6 flow: %v", err)
	}
	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write([]byte("six"))
	if n, err := c.Read(make([]byte, 8)); err != nil || n != 3 {
		t.Fatalf("echo: %d %v", n, err)
	}

	p, err := h.ListenICMP6()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Send(netip.IPv6Loopback(), 0x4b42, 3, []byte("ping6"), 5); err != nil {
		t.Fatal(err)
	}
	p.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	for {
		n, from, err := p.Recv(buf)
		if err != nil {
			t.Fatalf("no echo reply: %v", err)
		}
		m := header.ICMPv6(buf[:n])
		if m.Type() == header.ICMPv6EchoReply && m.Ident() == 0x4b42 && from == netip.IPv6Loopback() {
			return
		}
	}
}
