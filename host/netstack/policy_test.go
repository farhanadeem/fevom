package netstack

import (
	"net/netip"
	"testing"
)

func TestForwardable(t *testing.T) {
	c := DefaultConfig
	yes := []string{"1.1.1.1", "93.184.215.14", "10.0.0.5", "192.168.1.1", "172.16.30.1", "100.64.0.1", "198.18.0.5", "198.18.1.1", "198.19.0.1"}
	no := []string{
		"127.0.0.1", "127.8.8.8",
		"0.0.0.0", "255.255.255.255", "224.0.0.251", "239.1.2.3", "169.254.169.254",
		"198.19.1.1", "198.19.1.2", "198.19.1.255",
		"::1", "2606:4700::1111",
		"::ffff:127.0.0.1",
	}
	for _, a := range yes {
		if !c.forwardable(netip.MustParseAddr(a)) {
			t.Errorf("%s should be forwardable", a)
		}
	}
	for _, a := range no {
		if c.forwardable(netip.MustParseAddr(a)) {
			t.Errorf("%s should not be forwardable", a)
		}
	}
	if c.forwardable(netip.Addr{}) {
		t.Error("the zero address should not be forwardable")
	}
}
