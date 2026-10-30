package vmnet

import (
	"net"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
)

type Lookup func(name string) (addr netip.Addr, found bool)

type Answerer struct {
	Lookup Lookup
	TTL    uint32
}

var (
	Group     = netip.MustParseAddr("224.0.0.251")
	GroupPort = netip.AddrPortFrom(Group, 5353)
)

const cacheFlush = 0x8000

func (a *Answerer) Respond(msg []byte, from netip.AddrPort) ([]byte, netip.AddrPort, bool) {
	var q dns.Msg
	if err := q.Unpack(msg); err != nil || q.Response || q.Opcode != dns.OpcodeQuery || len(q.Question) == 0 {
		return nil, netip.AddrPort{}, false
	}
	ttl := a.TTL
	if ttl == 0 {
		ttl = 10
	}
	legacy := from.Port() != 5353
	unicast := legacy
	allQU := true
	var answers, extra []dns.RR
	for _, qq := range q.Question {
		class := qq.Qclass &^ cacheFlush
		if qq.Qclass&cacheFlush == 0 {
			allQU = false
		}
		if class != dns.ClassINET && class != dns.ClassANY {
			continue
		}
		name := strings.ToLower(strings.TrimSuffix(qq.Name, "."))
		addr, found := a.Lookup(name)
		if !found {
			continue
		}
		rrClass := uint16(dns.ClassINET)
		if !legacy {
			rrClass |= cacheFlush
		}
		hdr := func(t uint16) dns.RR_Header {
			return dns.RR_Header{Name: qq.Name, Rrtype: t, Class: rrClass, Ttl: ttl}
		}
		nsec := &dns.NSEC{Hdr: hdr(dns.TypeNSEC), NextDomain: qq.Name}
		var rec *dns.A
		if addr.Is4() {
			nsec.TypeBitMap = []uint16{dns.TypeA}
			rec = &dns.A{Hdr: hdr(dns.TypeA), A: net.IP(addr.AsSlice())}
		}
		switch qq.Qtype {
		case dns.TypeA, dns.TypeANY:
			switch {
			case rec == nil:
				answers = append(answers, nsec)
				continue
			case !known(&q, rec, ttl):
				answers = append(answers, rec)
			}
			if qq.Qtype == dns.TypeANY {
				answers = append(answers, nsec)
			} else if !legacy {
				extra = append(extra, nsec)
			}
		default:
			answers = append(answers, nsec)
		}
	}
	if len(answers) == 0 {
		return nil, netip.AddrPort{}, false
	}
	r := new(dns.Msg)
	r.Response, r.Authoritative = true, true
	r.Answer, r.Extra = answers, extra
	to := GroupPort
	if legacy {
		r.Id = q.Id
		r.Question = q.Question
	}
	if unicast || allQU {
		to = from
	}
	r.Compress = true
	out, err := r.Pack()
	if err != nil {
		return nil, netip.AddrPort{}, false
	}
	return out, to, true
}

func known(q *dns.Msg, rec *dns.A, ttl uint32) bool {
	for _, k := range q.Answer {
		if a, ok := k.(*dns.A); ok && strings.EqualFold(a.Hdr.Name, rec.Hdr.Name) && a.A.Equal(rec.A) && a.Hdr.Ttl >= ttl/2 {
			return true
		}
	}
	return false
}

func upFrame(b []byte) (netip.AddrPort, []byte, bool) {
	if len(b) < 6 {
		return netip.AddrPort{}, nil, false
	}
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[:4])), uint16(b[4])<<8|uint16(b[5])), b[6:], true
}

func downFrame(to netip.AddrPort, msg []byte) []byte {
	a := to.Addr().As4()
	b := make([]byte, 0, 6+len(msg))
	b = append(b, a[:]...)
	b = append(b, byte(to.Port()>>8), byte(to.Port()))
	return append(b, msg...)
}
