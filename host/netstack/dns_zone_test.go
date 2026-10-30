package netstack

import (
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/miekg/dns"
)

type fakeZone struct {
	mu    sync.Mutex
	asked []string
}

func (z *fakeZone) Answer(req *dns.Msg) (*dns.Msg, bool) {
	q := req.Question[0]
	name := strings.ToLower(q.Name)
	z.mu.Lock()
	z.asked = append(z.asked, name)
	z.mu.Unlock()
	if !strings.HasSuffix(name, ".fev.local.") && name != "app.test." {
		return nil, false
	}
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Authoritative = true
	if name != "web.fev.local." && name != "app.test." {
		resp.Rcode = dns.RcodeNameError
		return resp, true
	}
	if q.Qtype == dns.TypeA {
		resp.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 10}, A: net.IPv4(198, 19, 4, 2)}}
	}
	return resp, true
}

func zoneWorld(t *testing.T, z Zone) *world {
	t.Helper()
	hostEnd, guestEnd := socketPair(t)
	h := newFakeHost()
	ns, err := New(hostEnd, Options{Host: h, Config: DefaultConfig, Zone: z})
	if err != nil {
		t.Fatal(err)
	}
	g := newGuestStack(t, guestEnd, false)
	t.Cleanup(func() {
		ns.Close()
		g.Close()
	})
	return &world{host: h, ns: ns, guest: g}
}

func (h *fakeHost) wasAsked(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.asked {
		if strings.EqualFold(a, name) {
			return true
		}
	}
	return false
}

func TestTheZoneAnswersFevomsNamesBeforeTheMac(t *testing.T) {
	z := &fakeZone{}
	w := zoneWorld(t, z)
	w.host.add(t, "example.com. 60 IN A 192.0.2.80")
	for _, network := range []string{"udp", "tcp"} {
		r := w.query(t, network, "Web.Fev.Local.", dns.TypeA)
		if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "198.19.4.2" || !r.Authoritative {
			t.Errorf("%s: web.fev.local: %s %q", network, dns.RcodeToString[r.Rcode], answers(r))
		}
	}
	if r := w.query(t, "udp", "app.test.", dns.TypeA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
		t.Errorf("app.test: %s %q", dns.RcodeToString[r.Rcode], answers(r))
	}
	if r := w.query(t, "udp", "nope.fev.local.", dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Errorf("nope.fev.local: %s, want NXDOMAIN", dns.RcodeToString[r.Rcode])
	}
	if r := w.query(t, "udp", "example.com.", dns.TypeA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
		t.Errorf("example.com: %s %q", dns.RcodeToString[r.Rcode], answers(r))
	}
	for _, name := range []string{"web.fev.local.", "app.test.", "nope.fev.local."} {
		if w.host.wasAsked(name) {
			t.Errorf("%s was put to the Mac's resolver", name)
		}
	}
	if !w.host.wasAsked("example.com.") {
		t.Error("example.com was not put to the Mac's resolver")
	}
}

func TestFevomLocalIsNeverAskedOfTheMac(t *testing.T) {
	w := zoneWorld(t, nil)
	for _, tc := range []struct {
		name  string
		qtype uint16
	}{
		{"web.fev.local.", dns.TypeA},
		{"WEB.fev.LOCAL.", dns.TypeAAAA},
		{"fev.local.", dns.TypeA},
		{"a.b.fev.local.", dns.TypeHTTPS},
	} {
		r := w.query(t, "udp", tc.name, tc.qtype)
		if r.Rcode != dns.RcodeNameError || !r.Authoritative || len(r.Ns) != 1 {
			t.Errorf("%s %s: %s aa=%v ns=%v", tc.name, dns.TypeToString[tc.qtype], dns.RcodeToString[r.Rcode], r.Authoritative, r.Ns)
			continue
		}
		soa, ok := r.Ns[0].(*dns.SOA)
		if !ok || soa.Hdr.Ttl != 1 || soa.Minttl != 1 || soa.Hdr.Name != "fev.local." {
			t.Errorf("%s: the SOA is %v", tc.name, r.Ns[0])
		}
		if w.host.wasAsked(tc.name) {
			t.Errorf("%s was put to the Mac's resolver", tc.name)
		}
	}
	w.host.add(t, "notfev.local. 60 IN A 192.0.2.9")
	if r := w.query(t, "udp", "notfev.local.", dns.TypeA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
		t.Errorf("notfev.local: %s %q", dns.RcodeToString[r.Rcode], answers(r))
	}
}
