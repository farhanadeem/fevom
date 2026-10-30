//go:build darwin

package macnet

import (
	"net/netip"
	"testing"

	"golang.org/x/net/route"
)

func TestTheMacsOwnTablesAreRead(t *testing.T) {
	s, err := readMac()
	if err != nil {
		t.Fatal(err)
	}
	if !s.HasAddress(netip.MustParseAddr("127.0.0.1")) {
		t.Fatalf("no 127.0.0.1 among %v", s.Addresses)
	}
	if len(s.Routes) == 0 {
		t.Fatal("no routes")
	}
	for _, r := range s.Routes {
		if !r.Prefix.IsValid() || r.Prefix.Masked() != r.Prefix {
			t.Fatalf("route %+v", r)
		}
	}
	t.Logf("%d routes, %d networks, %d addresses, global IPv6 %v", len(s.Routes), len(s.Networks), len(s.Addresses), s.GlobalIPv6)
}

func TestRoutingMessagesBecomeRoutes(t *testing.T) {
	v4 := func(a, b, c, d byte) *route.Inet4Addr { return &route.Inet4Addr{IP: [4]byte{a, b, c, d}} }
	for _, tc := range []struct {
		name  string
		flags int
		addrs []route.Addr
		want  string
		ok    bool
	}{
		{"a network", 0x901, []route.Addr{v4(172, 16, 30, 0), &route.LinkAddr{Index: 14}, v4(255, 255, 255, 0)}, "172.16.30.0/24", true},
		{"a default", 0x40010803, []route.Addr{v4(0, 0, 0, 0), v4(172, 16, 30, 1), v4(0, 0, 0, 0)}, "0.0.0.0/0", true},
		{"a host route", 0x200005 &^ rtfLocal, []route.Addr{v4(100, 97, 252, 112), v4(100, 97, 252, 112)}, "100.97.252.112/32", true},
		{"a /10", 0x901, []route.Addr{v4(100, 64, 0, 0), nil, v4(255, 192, 0, 0)}, "100.64.0.0/10", true},
		{"a cloned host", 0x45020007, []route.Addr{v4(17, 8, 138, 25), v4(172, 16, 30, 1)}, "", false},
		{"an ARP entry", 0x1020405, []route.Addr{v4(172, 16, 30, 154), &route.LinkAddr{Index: 14}}, "", false},
		{"a local address", 0x1220405, []route.Addr{v4(172, 16, 30, 211), &route.LinkAddr{Index: 14}}, "", false},
		{"a broadcast", 0x1420405, []route.Addr{v4(172, 16, 30, 255), &route.LinkAddr{Index: 14}}, "", false},
		{"no destination", 0x1, []route.Addr{nil}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := parseRoute(tc.flags, tc.addrs, "en0")
			if ok != tc.ok || (ok && r.Prefix.String() != tc.want) {
				t.Fatalf("got %v %v, want %s %v", r.Prefix, ok, tc.want, tc.ok)
			}
		})
	}
	if t.Failed() {
		return
	}
	for _, tc := range []struct {
		name              string
		flags             int
		addrs             []route.Addr
		iface             string
		reject, blackhole bool
		gateway           string
	}{
		{"fev's reject route", 0x809, []route.Addr{v4(198, 19, 0, 0), &route.LinkAddr{Index: 1}, v4(255, 255, 0, 0)}, "lo0", true, false, "invalid IP"},
		{"a VPN's reject route", 0x80b, []route.Addr{v4(198, 19, 0, 0), v4(127, 0, 0, 1), v4(255, 255, 0, 0)}, "lo0", true, false, "127.0.0.1"},
		{"a blackhole route", 0x1803, []route.Addr{v4(198, 19, 0, 0), v4(127, 0, 0, 1), v4(255, 255, 0, 0)}, "lo0", false, true, "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := parseRoute(tc.flags, tc.addrs, tc.iface)
			if !ok || r.Prefix.String() != "198.19.0.0/16" || r.Reject != tc.reject || r.Blackhole != tc.blackhole || r.Gateway.String() != tc.gateway {
				t.Fatalf("got %+v %v", r, ok)
			}
		})
	}
	r, _ := parseRoute(0x41000901, []route.Addr{v4(0, 0, 0, 0), &route.LinkAddr{}, v4(0, 0, 0, 0)}, "utun4")
	if !r.Scoped {
		t.Fatal("RTF_IFSCOPE not read as scoped")
	}
	if r, _ := parseRoute(0x1, []route.Addr{v4(10, 0, 0, 0), nil, v4(255, 0, 255, 0)}, ""); r.Prefix.Bits() != 32 {
		t.Fatalf("non-contiguous mask: %v", r.Prefix)
	}
}
