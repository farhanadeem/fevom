package netstack

import (
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func TestANameTheMacHasNoAddressForIsNoSuchName(t *testing.T) {
	w := newWorld(t)
	w.host.add(t,
		"v4only.example. 60 IN A 192.0.2.1",
		"v6only.example. 60 IN AAAA 2001:db8::1",
		"mxonly.example. 60 IN MX 10 mail.example.",
		"alias.example. 60 IN CNAME nowhere.example.",
	)
	for _, tc := range []struct {
		name  string
		qtype uint16
		rcode int
	}{
		{"missing.example.", dns.TypeA, dns.RcodeNameError},
		{"missing.example.", dns.TypeAAAA, dns.RcodeNameError},
		{"v4only.example.", dns.TypeAAAA, dns.RcodeSuccess},
		{"v6only.example.", dns.TypeA, dns.RcodeSuccess},
		{"v4only.example.", dns.TypeMX, dns.RcodeSuccess},
		{"missing.example.", dns.TypeMX, dns.RcodeSuccess},
		{"mxonly.example.", dns.TypeA, dns.RcodeNameError},
		{"alias.example.", dns.TypeA, dns.RcodeSuccess},
	} {
		r := w.query(t, "udp", tc.name, tc.qtype)
		if r.Rcode != tc.rcode {
			t.Errorf("%s %s: rcode %s, want %s (%q)", tc.name, dns.TypeToString[tc.qtype], dns.RcodeToString[r.Rcode], dns.RcodeToString[tc.rcode], answers(r))
		}
	}
	w.host.search = []string{"lab.example"}
	if r := w.query(t, "udp", "printer.", dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Errorf("a bare name found nowhere: %s", dns.RcodeToString[r.Rcode])
	}
	if r := w.query(t, "udp", "host.docker.internal.", dns.TypeAAAA); r.Rcode != dns.RcodeSuccess {
		t.Errorf("host.docker.internal AAAA: %s", dns.RcodeToString[r.Rcode])
	}
}

func TestAWithheldAAAADoesNotMakeANameUnknown(t *testing.T) {
	cfg := DefaultConfig
	hostEnd, guestEnd := socketPair(t)
	h := newFakeHost()
	ns, err := New(hostEnd, Options{Host: h, Config: cfg, IPv6Usable: func() bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	g := newGuestStack(t, guestEnd, false)
	defer func() {
		ns.Close()
		g.Close()
	}()
	w := &world{host: h, ns: ns, guest: g}
	h.add(t, "v6only.example. 60 IN AAAA 2001:db8::1")
	for _, qt := range []uint16{dns.TypeA, dns.TypeAAAA} {
		r := w.query(t, "udp", "v6only.example.", qt)
		if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
			t.Errorf("%s: %s %q", dns.TypeToString[qt], dns.RcodeToString[r.Rcode], answers(r))
		}
	}
	if r := w.query(t, "udp", "missing.example.", dns.TypeAAAA); r.Rcode != dns.RcodeNameError {
		t.Errorf("missing AAAA: %s", dns.RcodeToString[r.Rcode])
	}
	if !strings.Contains(strings.Join(h.asked, " "), "v6only.example.") {
		t.Fatal("never asked")
	}
}
