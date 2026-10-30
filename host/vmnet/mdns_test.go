package vmnet

import (
	"net/netip"
	"testing"

	"github.com/miekg/dns"
)

var testNames = map[string]netip.Addr{
	"fev.local":      netip.MustParseAddr("10.183.4.1"),
	"web.fev.local":  netip.MustParseAddr("10.183.4.2"),
	"shop.local":     netip.MustParseAddr("10.183.4.3"),
	"bare.fev.local": {},
}

func testLookup(name string) (netip.Addr, bool) {
	a, ok := testNames[name]
	return a, ok
}

var mac = netip.MustParseAddrPort("10.183.3.1:5353")

func query(t *testing.T, id uint16, qs ...dns.Question) []byte {
	t.Helper()
	m := new(dns.Msg)
	m.Id = id
	m.Question = qs
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func q(name string, t uint16) dns.Question {
	return dns.Question{Name: dns.Fqdn(name), Qtype: t, Qclass: dns.ClassINET}
}

func respond(t *testing.T, msg []byte, from netip.AddrPort) (*dns.Msg, netip.AddrPort, bool) {
	t.Helper()
	a := &Answerer{Lookup: testLookup}
	out, to, ok := a.Respond(msg, from)
	if !ok {
		return nil, to, false
	}
	var r dns.Msg
	if err := r.Unpack(out); err != nil {
		t.Fatal(err)
	}
	return &r, to, true
}

func TestAnAQuestionIsAnsweredToTheGroupWithItsNSEC(t *testing.T) {
	r, to, ok := respond(t, query(t, 0, q("Web.Fev.Local", dns.TypeA)), mac)
	if !ok {
		t.Fatal("no answer")
	}
	if to != GroupPort || r.Id != 0 || len(r.Question) != 0 || !r.Response || !r.Authoritative {
		t.Fatalf("to %v, id %d, questions %d, flags %v %v", to, r.Id, len(r.Question), r.Response, r.Authoritative)
	}
	if len(r.Answer) != 1 {
		t.Fatalf("answers %v", r.Answer)
	}
	a, ok := r.Answer[0].(*dns.A)
	if !ok || a.A.String() != "10.183.4.2" || a.Hdr.Name != "Web.Fev.Local." || a.Hdr.Ttl != 10 || a.Hdr.Class != dns.ClassINET|cacheFlush {
		t.Fatalf("answer %v (class %#x)", r.Answer[0], r.Answer[0].Header().Class)
	}
	if len(r.Extra) != 1 {
		t.Fatalf("extra %v", r.Extra)
	}
	if n, ok := r.Extra[0].(*dns.NSEC); !ok || len(n.TypeBitMap) != 1 || n.TypeBitMap[0] != dns.TypeA {
		t.Fatalf("extra %v", r.Extra[0])
	}
}

func TestAnAAAAQuestionIsAnNSECSayingAAlone(t *testing.T) {
	for _, qt := range []uint16{dns.TypeAAAA, dns.TypeHTTPS, dns.TypeTXT} {
		r, _, ok := respond(t, query(t, 0, q("fev.local", qt)), mac)
		if !ok || len(r.Answer) != 1 {
			t.Fatalf("type %d: %v", qt, r)
		}
		n, ok := r.Answer[0].(*dns.NSEC)
		if !ok || len(n.TypeBitMap) != 1 || n.TypeBitMap[0] != dns.TypeA || n.NextDomain != "fev.local." {
			t.Fatalf("type %d: %v", qt, r.Answer[0])
		}
	}
	r, _, ok := respond(t, query(t, 0, q("bare.fev.local", dns.TypeA)), mac)
	if !ok || len(r.Answer) != 1 || r.Answer[0].Header().Rrtype != dns.TypeNSEC || len(r.Answer[0].(*dns.NSEC).TypeBitMap) != 0 {
		t.Fatalf("bare: %v", r)
	}
	r, _, ok = respond(t, query(t, 0, q("bare.fev.local", dns.TypeAAAA)), mac)
	if !ok || len(r.Answer) != 1 || len(r.Answer[0].(*dns.NSEC).TypeBitMap) != 0 {
		t.Fatalf("bare AAAA: %v", r)
	}
}

func TestANYHasTheAAndTheNSEC(t *testing.T) {
	r, _, ok := respond(t, query(t, 0, q("shop.local", dns.TypeANY)), mac)
	if !ok || len(r.Answer) != 2 || r.Answer[0].Header().Rrtype != dns.TypeA || r.Answer[1].Header().Rrtype != dns.TypeNSEC {
		t.Fatalf("%v", r)
	}
}

func TestNamesFevomDoesNotHoldAreNotAnswered(t *testing.T) {
	for _, n := range []string{"other.fev.local", "printer.local", "adas-mac.local"} {
		if r, _, ok := respond(t, query(t, 0, q(n, dns.TypeA)), mac); ok {
			t.Fatalf("%s answered: %v", n, r)
		}
	}
	r, _, ok := respond(t, query(t, 0, q("printer.local", dns.TypeA), q("web.fev.local", dns.TypeA)), mac)
	if !ok || len(r.Answer) != 1 || r.Answer[0].Header().Name != "web.fev.local." {
		t.Fatalf("%v", r)
	}
}

func TestResponsesAndOtherOpcodesAreNotAnswered(t *testing.T) {
	m := new(dns.Msg)
	m.Response = true
	m.Question = []dns.Question{q("web.fev.local", dns.TypeA)}
	b, _ := m.Pack()
	if _, _, ok := (&Answerer{Lookup: testLookup}).Respond(b, mac); ok {
		t.Fatal("a response was answered")
	}
	m.Response = false
	m.Opcode = dns.OpcodeUpdate
	b, _ = m.Pack()
	if _, _, ok := (&Answerer{Lookup: testLookup}).Respond(b, mac); ok {
		t.Fatal("an update was answered")
	}
	if _, _, ok := (&Answerer{Lookup: testLookup}).Respond([]byte{1, 2, 3}, mac); ok {
		t.Fatal("garbage was answered")
	}
}

func TestALegacyQueryIsAnsweredToItsAskerWithItsIDAndQuestion(t *testing.T) {
	from := netip.MustParseAddrPort("10.183.3.1:61234")
	r, to, ok := respond(t, query(t, 4242, q("web.fev.local", dns.TypeA)), from)
	if !ok || to != from || r.Id != 4242 || len(r.Question) != 1 || len(r.Answer) != 1 || len(r.Extra) != 0 {
		t.Fatalf("to %v: %v", to, r)
	}
	if c := r.Answer[0].Header().Class; c != dns.ClassINET {
		t.Fatalf("a legacy answer has the cache-flush bit: %#x", c)
	}
}

func TestAQUQuestionIsAnsweredToItsAsker(t *testing.T) {
	qq := q("web.fev.local", dns.TypeA)
	qq.Qclass |= cacheFlush
	_, to, ok := respond(t, query(t, 0, qq), mac)
	if !ok || to != mac {
		t.Fatalf("to %v", to)
	}
}

func TestAKnownAnswerIsNotSentAgain(t *testing.T) {
	m := new(dns.Msg)
	m.Question = []dns.Question{q("web.fev.local", dns.TypeA)}
	known, _ := dns.NewRR("web.fev.local. 8 IN A 10.183.4.2")
	m.Answer = []dns.RR{known}
	b, _ := m.Pack()
	r, _, ok := respond(t, b, mac)
	if ok && len(r.Answer) != 0 {
		t.Fatalf("a known answer sent again: %v", r)
	}
	stale, _ := dns.NewRR("web.fev.local. 4 IN A 10.183.4.2")
	m.Answer = []dns.RR{stale}
	b, _ = m.Pack()
	if r, _, ok := respond(t, b, mac); !ok || len(r.Answer) != 1 {
		t.Fatalf("a stale known answer kept back: %v", r)
	}
}

func TestTheShimsFraming(t *testing.T) {
	to := netip.MustParseAddrPort("224.0.0.251:5353")
	b := downFrame(to, []byte("m"))
	from, msg, ok := upFrame(b)
	if !ok || from != to || string(msg) != "m" {
		t.Fatalf("%v %q %v", from, msg, ok)
	}
	if _, _, ok := upFrame([]byte{1, 2}); ok {
		t.Fatal("a short frame read")
	}
}
