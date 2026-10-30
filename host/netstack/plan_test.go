package netstack

import (
	"net/netip"
	"testing"

	"github.com/miekg/dns"

	"fev/proto/netplan"
)

func TestTheDefaultsAreThePlans(t *testing.T) {
	p := netplan.Default()
	if DefaultConfig.Subnet != p.Uplink() || DefaultConfig.Gateway != p.UplinkGateway() || DefaultConfig.Host != p.Host() {
		t.Fatalf("DefaultConfig %+v is not the plan's uplink %s, gateway %s, host %s", DefaultConfig, p.Uplink(), p.UplinkGateway(), p.Host())
	}
}

func TestAnAddressOfTheOldUplinkIsReached(t *testing.T) {
	w := newWorld(t)
	w.host.add(t, "fake-ip.example. 60 IN A 198.18.0.5")
	r := w.query(t, "udp", "fake-ip.example.", dns.TypeA)
	if len(r.Answer) != 1 {
		t.Fatalf("answer %v", r.Answer)
	}
	a, _ := netip.AddrFromSlice(r.Answer[0].(*dns.A).A.To4())
	c, err := w.dialTCP(t, netip.AddrPortFrom(a, 443).String())
	if err != nil {
		t.Fatalf("the guest could not reach %s: %v", a, err)
	}
	c.Close()
	w.host.mu.Lock()
	defer w.host.mu.Unlock()
	if len(w.host.tcpDials) != 1 || w.host.tcpDials[0] != netip.MustParseAddrPort("198.18.0.5:443") {
		t.Fatalf("host dialled %v", w.host.tcpDials)
	}
}
