package macnet

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"fev/proto/netplan"
)

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func addr(s string) netip.Addr  { return netip.MustParseAddr(s) }

func office() Snapshot {
	return Snapshot{
		Routes: []Route{
			{Prefix: pfx("0.0.0.0/0"), Gateway: addr("172.16.30.1"), Interface: "en0"},
			{Prefix: pfx("0.0.0.0/0"), Interface: "utun4", Scoped: true},
			{Prefix: pfx("100.64.0.0/10"), Interface: "utun4"},
			{Prefix: pfx("100.100.100.100/32"), Interface: "utun4"},
			{Prefix: pfx("169.254.0.0/16"), Interface: "en0"},
			{Prefix: pfx("172.16.30.0/24"), Interface: "en0"},
			{Prefix: pfx("172.16.30.1/32"), Interface: "en0"},
			{Prefix: pfx("198.19.0.0/24"), Interface: "utun9"},
			{Prefix: pfx("198.19.64.0/18"), Interface: "utun9"},
			{Prefix: pfx("::/0"), Gateway: addr("fe80::1"), Interface: "en0"},
		},
		Networks: []Network{
			{Prefix: pfx("172.16.30.0/24"), Interface: "en0"},
			{Prefix: pfx("100.97.252.112/32"), Interface: "utun4"},
			{Prefix: pfx("198.19.3.1/32"), Interface: "utun9"},
		},
		Addresses: []Address{
			{Addr: addr("127.0.0.1"), Interface: "lo0"},
			{Addr: addr("172.16.30.211"), Interface: "en0"},
			{Addr: addr("100.97.252.112"), Interface: "utun4"},
			{Addr: addr("198.19.3.1"), Interface: "utun9"},
			{Addr: addr("fe80::1c2d:3e4f:5a6b:7c8d"), Interface: "en0"},
		},
	}
}

var routed = netplan.Default().Routed()

func with(s Snapshot, extra ...Route) Snapshot {
	s.Routes = append(slices.Clone(s.Routes), extra...)
	return s
}

func TestNothingOfTheMacsMeetsTheDefaultPlanHere(t *testing.T) {
	r := Check(routed, "utun9", office())
	if !slices.Equal(r.Keep, routed) || len(r.Conflicts) != 0 {
		t.Fatalf("kept %v, conflicts %v; want every prefix and none", r.Keep, r.Conflicts)
	}
}

func TestAnEqualRouteWithdrawsThePrefixAndAMoreSpecificOneToo(t *testing.T) {
	for _, tc := range []struct {
		name, route, gone string
	}{
		{"equal", "198.19.4.0/22", "198.19.4.0/22"},
		{"more specific", "198.19.70.0/24", "198.19.64.0/18"},
		{"a host route", "198.19.0.9/32", "198.19.0.0/24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := with(office(), Route{Prefix: pfx(tc.route), Gateway: addr("10.8.0.1"), Interface: "utun5"})
			r := Check(routed, "utun9", s)
			if got := r.Withdrawn(); !slices.Equal(got, []netip.Prefix{pfx(tc.gone)}) {
				t.Fatalf("withdrawn %v, want %s", got, tc.gone)
			}
			if slices.Contains(r.Keep, pfx(tc.gone)) || len(r.Keep) != len(routed)-1 {
				t.Fatalf("kept %v", r.Keep)
			}
			w := r.Conflicts[0].Warning()
			if !strings.Contains(w, tc.route) || !strings.Contains(w, tc.gone) || !strings.Contains(w, "utun5") || !strings.Contains(w, "off") {
				t.Fatalf("warning %q names neither both prefixes nor the interface", w)
			}
		})
	}
}

func TestALessSpecificRouteLeavesFevomsRouteAndWarns(t *testing.T) {
	s := with(office(), Route{Prefix: pfx("198.18.0.0/15"), Interface: "utun6"})
	r := Check(routed, "utun9", s)
	if !slices.Equal(r.Keep, routed) {
		t.Fatalf("kept %v, want all", r.Keep)
	}
	if len(r.Conflicts) != len(routed) {
		t.Fatalf("conflicts %v: want one warning a prefix", r.Conflicts)
	}
	for _, c := range r.Conflicts {
		if c.Withdrawn || c.With != pfx("198.18.0.0/15") || !strings.Contains(c.Warning(), "covers") {
			t.Fatalf("conflict %+v (%q)", c, c.Warning())
		}
	}
	vpn := with(office(), Route{Prefix: pfx("10.0.0.0/8"), Interface: "utun5"})
	p, _ := netplan.New(pfx("10.198.0.0/16"))
	if r := Check(p.Routed(), "utun9", vpn); !slices.Equal(r.Keep, p.Routed()) || len(r.Withdrawn()) != 0 {
		t.Fatalf("a VPN's 10/8 over 10.198/16: kept %v withdrawn %v", r.Keep, r.Withdrawn())
	}
}

func TestAConnectedNetworkInsideAPrefixWithdrawsItAndOverItWarns(t *testing.T) {
	s := office()
	s.Networks = append(s.Networks, Network{Prefix: pfx("198.19.66.0/24"), Interface: "en7"})
	s.Addresses = append(s.Addresses, Address{Addr: addr("198.19.66.20"), Interface: "en7"})
	s.Routes = append(s.Routes, Route{Prefix: pfx("198.19.66.0/24"), Interface: "en7"})
	r := Check(routed, "utun9", s)
	if got := r.Withdrawn(); !slices.Equal(got, []netip.Prefix{pfx("198.19.64.0/18")}) {
		t.Fatalf("withdrawn %v", got)
	}
	n := 0
	for _, c := range r.Conflicts {
		if c.Prefix == pfx("198.19.64.0/18") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("conflicts for the pool: %v, want the network once and the address", r.Conflicts)
	}

	s = office()
	s.Networks = append(s.Networks, Network{Prefix: pfx("198.19.0.0/16"), Interface: "en7"})
	s.Addresses = append(s.Addresses, Address{Addr: addr("198.19.200.7"), Interface: "en7"})
	r = Check(routed, "utun9", s)
	if len(r.Withdrawn()) != 0 || !slices.Equal(r.Keep, routed) {
		t.Fatalf("a covering LAN withdrew %v", r.Withdrawn())
	}
	s.Addresses = append(s.Addresses, Address{Addr: addr("198.19.5.20"), Interface: "en7"})
	if got := Check(routed, "utun9", s).Withdrawn(); !slices.Equal(got, []netip.Prefix{pfx("198.19.4.0/22")}) {
		t.Fatalf("the Mac's own address in the names' range: withdrawn %v", got)
	}
}

func TestAVPNsScopedRouteWarnsAndWithdrawsNothing(t *testing.T) {
	s := with(office(), Route{Prefix: pfx("198.19.2.0/24"), Interface: "utun5", Scoped: true})
	r := Check(routed, "utun9", s)
	if len(r.Withdrawn()) != 0 || len(r.Conflicts) != 1 || r.Conflicts[0].Kind != KindScoped {
		t.Fatalf("conflicts %v", r.Conflicts)
	}
	if w := r.Conflicts[0].Warning(); !strings.Contains(w, "bound to utun5") {
		t.Fatalf("warning %q", w)
	}
}

func TestFevomsOwnTunnelAndDefaultRoutesNeverConflict(t *testing.T) {
	s := office()
	s.Routes = append(s.Routes, Route{Prefix: pfx("198.19.4.0/22"), Interface: "utun9"}, Route{Prefix: pfx("0.0.0.0/0"), Interface: "en0", Scoped: true})
	if r := Check(routed, "utun9", s); len(r.Conflicts) != 0 {
		t.Fatalf("conflicts %v", r.Conflicts)
	}
	if r := Check(routed, "utun3", s); len(r.Withdrawn()) == 0 {
		t.Fatal("another interface's route inside fev's prefix was not a conflict")
	}
}

func TestAPrefixComesBackWhenTheRouteGoes(t *testing.T) {
	s := with(office(), Route{Prefix: pfx("198.19.2.0/24"), Interface: "utun5"})
	if got := Check(routed, "utun9", s).Withdrawn(); len(got) != 1 {
		t.Fatalf("withdrawn %v", got)
	}
	if r := Check(routed, "utun9", office()); !slices.Equal(r.Keep, routed) {
		t.Fatalf("kept %v after the route went", r.Keep)
	}
}

func TestTakenIsWhatTheFirstStartAvoids(t *testing.T) {
	s := with(office(), Route{Prefix: pfx("198.18.0.0/15"), Interface: "utun6"})
	taken := Taken(s, "")
	p, ok := netplan.Choose(netplan.Candidates, taken)
	if !ok || p.Base() != pfx("10.198.0.0/16") {
		t.Fatalf("chose %v, %v: a fake-IP /15 over the default must move the plan", p.Base(), ok)
	}
	o := office()
	o.Routes = slices.DeleteFunc(slices.Clone(o.Routes), func(r Route) bool { return r.Interface == "utun9" })
	if p, ok := netplan.Choose(netplan.Candidates, Taken(o, "utun9")); !ok || p.Base() != netplan.DefaultBase {
		t.Fatalf("chose %v", p.Base())
	}
	for _, q := range Taken(s, "") {
		if q.Bits() == 0 || q.Addr().IsLoopback() {
			t.Fatalf("taken has %v", q)
		}
	}
}

func TestTheMacsAddressesForOtherHosts(t *testing.T) {
	got := office().MacAddresses()
	want := []netip.Addr{addr("100.97.252.112"), addr("172.16.30.211"), addr("198.19.3.1")}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if !office().HasAddress(addr("127.0.0.1")) || office().HasAddress(addr("172.16.30.99")) {
		t.Fatal("HasAddress")
	}
}

func TestOnlyTheHelpersOwnRejectRouteIsLeftOut(t *testing.T) {
	held := []Held{{Prefix: pfx("198.19.0.0/16"), Interface: "lo0"}, {Prefix: pfx("fd5b:6e2a:91c0::/48"), Interface: "lo0"}}
	fevs := Route{Prefix: pfx("198.19.0.0/16"), Interface: "lo0", Reject: true}
	s := with(office(), fevs, Route{Prefix: pfx("fd5b:6e2a:91c0::/48"), Interface: "lo0", Reject: true})
	if r := Check(routed, "utun9", s.WithoutHeld(held)); len(r.Conflicts) != 0 || !slices.Equal(r.Keep, routed) {
		t.Errorf("fev's own reject route: kept %v, conflicts %v", r.Keep, r.Conflicts)
	}
	if got := Taken(s.WithoutHeld(held), "utun9"); slices.Contains(got, pfx("198.19.0.0/16")) {
		t.Errorf("a new plan would avoid fev's own held base: %v", got)
	}
	for _, other := range []Route{
		{Prefix: pfx("198.19.0.0/16"), Gateway: addr("127.0.0.1"), Interface: "lo0", Reject: true},
		{Prefix: pfx("198.19.0.0/16"), Interface: "lo0", Blackhole: true},
		{Prefix: pfx("198.19.0.0/16"), Interface: "utun5", Reject: true},
		{Prefix: pfx("198.19.4.0/22"), Interface: "lo0", Reject: true},
	} {
		s := with(office(), other).WithoutHeld(held)
		r := Check(routed, "utun9", s)
		if len(r.Conflicts) == 0 {
			t.Errorf("%+v: no conflict", other)
		}
		if !slices.Contains(Taken(s, "utun9"), other.Prefix) {
			t.Errorf("%+v: not avoided by a new plan", other)
		}
	}
}
