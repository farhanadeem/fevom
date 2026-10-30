package machnet

import (
	"net/netip"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	LocalZone    = "fev.local"
	InternalZone = "fev.internal"
	DockerName   = "docker." + InternalZone
)

const (
	nameTTL     = 10
	negativeTTL = 1
)

type resolver struct {
	lookup       func(name string) (netip.Addr, bool)
	lookup6      func(name string) netip.Addr
	gateway      netip.Addr
	forwardLocal bool
}

func (r resolver) answer(query []byte) []byte {
	a, _ := r.decide(query)
	return a
}

func (r resolver) decide(query []byte) (out, fallback []byte) {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil || h.Response {
		return nil, nil
	}
	q, err := p.Question()
	if err != nil {
		return nil, nil
	}
	name := strings.ToLower(strings.TrimSuffix(q.Name.String(), "."))
	ours := func(zone string) bool { return name == zone || strings.HasSuffix(name, "."+zone) }
	var addr, addr6 netip.Addr
	found, machine := false, false
	switch {
	case name == DockerName:
		addr, found = r.gateway, true
	case ours(LocalZone):
		if label, ok := strings.CutSuffix(name, "."+LocalZone); ok {
			if addr, found = r.lookup(label); found && r.lookup6 != nil {
				addr6 = r.lookup6(label)
			}
			machine = found
		}
		if !found && r.forwardLocal {
			return nil, nil
		}
	default:
		return nil, nil
	}
	var a []byte
	switch {
	case h.OpCode != 0 || q.Class != dnsmessage.ClassINET:
		a = reply(h, q, dnsmessage.RCodeNotImplemented, netip.Addr{})
	case !found:
		a = reply(h, q, dnsmessage.RCodeNameError, netip.Addr{})
	case q.Type == dnsmessage.TypeA:
		a = reply(h, q, dnsmessage.RCodeSuccess, addr)
	case q.Type == dnsmessage.TypeAAAA && addr6.IsValid():
		a = reply(h, q, dnsmessage.RCodeSuccess, addr6)
	default:
		a = reply(h, q, dnsmessage.RCodeSuccess, netip.Addr{})
	}
	if machine && r.forwardLocal {
		return nil, a
	}
	return a, nil
}

func useFallback(answer []byte) bool {
	return len(answer) < 4 || dnsmessage.RCode(answer[3]&0x0f) == dnsmessage.RCodeNameError
}

func reply(h dnsmessage.Header, q dnsmessage.Question, rcode dnsmessage.RCode, addr netip.Addr) []byte {
	b := dnsmessage.NewBuilder(make([]byte, 0, 512), dnsmessage.Header{
		ID: h.ID, Response: true, Authoritative: true, RecursionDesired: h.RecursionDesired,
		RecursionAvailable: true, RCode: rcode,
	})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil
	}
	if err := b.Question(q); err != nil {
		return nil
	}
	if addr.IsValid() {
		if err := b.StartAnswers(); err != nil {
			return nil
		}
		if addr.Is4() {
			rh := dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: nameTTL}
			if err := b.AResource(rh, dnsmessage.AResource{A: addr.As4()}); err != nil {
				return nil
			}
		} else {
			rh := dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET, TTL: nameTTL}
			if err := b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: addr.As16()}); err != nil {
				return nil
			}
		}
	} else if rcode == dnsmessage.RCodeSuccess || rcode == dnsmessage.RCodeNameError {
		if err := b.StartAuthorities(); err != nil {
			return nil
		}
		zone := LocalZone
		if n := strings.ToLower(q.Name.String()); n == InternalZone+"." || strings.HasSuffix(n, "."+InternalZone+".") {
			zone = InternalZone
		}
		apex := dnsmessage.MustNewName(zone + ".")
		rh := dnsmessage.ResourceHeader{Name: apex, Type: dnsmessage.TypeSOA, Class: dnsmessage.ClassINET, TTL: negativeTTL}
		soa := dnsmessage.SOAResource{
			NS: dnsmessage.MustNewName("ns." + zone + "."), MBox: dnsmessage.MustNewName("hostmaster." + zone + "."),
			Serial: 1, Refresh: 3600, Retry: 600, Expire: 86400, MinTTL: negativeTTL,
		}
		if err := b.SOAResource(rh, soa); err != nil {
			return nil
		}
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}
