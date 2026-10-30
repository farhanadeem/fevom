package netstack

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

type Resolver interface {
	Resolve(ctx context.Context, name string, qtype uint16) ([]dns.RR, error)
	SearchDomains() []string
}

const (
	dnsPort    = 53
	dnsTimeout = 4 * time.Second
	dnsUDPSize = 512
)

type dnsServer struct {
	udp, tcp *dns.Server
}

func newDNSServer(s *Stack) (*dnsServer, error) {
	addr := tcpip.FullAddress{NIC: nicID, Addr: toAddr(s.cfg.Gateway), Port: dnsPort}
	pc, err := gonet.DialUDP(s.stack, &addr, nil, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("netstack: DNS over UDP: %w", err)
	}
	l, err := gonet.ListenTCP(s.stack, addr, ipv4.ProtocolNumber)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("netstack: DNS over TCP: %w", err)
	}
	resolver := hostNames{Resolver: ipv6Gate{Resolver: s.host, usable: s.ipv6Usable}, addr: s.cfg.Host}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		ctx, cancel := context.WithTimeout(s.ctx, dnsTimeout)
		defer cancel()
		resp, ok := answerZone(s.zone, req)
		if !ok {
			resp = answerDNS(ctx, resolver, req)
			if len(req.Question) == 1 && resp.Rcode == dns.RcodeSuccess {
				s.names.record(answeredName(req.Question[0], resp), resp.Answer)
			}
		}
		if _, isUDP := w.RemoteAddr().(*net.UDPAddr); isUDP || w.RemoteAddr().Network() == "udp" {
			size := dnsUDPSize
			if opt := req.IsEdns0(); opt != nil && int(opt.UDPSize()) > size {
				size = int(opt.UDPSize())
			}
			resp.Truncate(size)
		}
		w.WriteMsg(resp)
	})
	d := &dnsServer{
		udp: &dns.Server{PacketConn: quietPacketConn{pc}, Handler: handler},
		tcp: &dns.Server{Listener: l, Handler: handler},
	}
	go d.udp.ActivateAndServe()
	go d.tcp.ActivateAndServe()
	return d, nil
}

type Zone interface {
	Answer(req *dns.Msg) (*dns.Msg, bool)
}

const LocalZone = "fev.local."

const localNegativeTTL = 1

func answerZone(z Zone, req *dns.Msg) (*dns.Msg, bool) {
	if z != nil {
		if resp, ok := z.Answer(req); ok && resp != nil {
			return resp, true
		}
	}
	if len(req.Question) != 1 || req.Opcode != dns.OpcodeQuery || !underLocalZone(req.Question[0].Name) {
		return nil, false
	}
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Authoritative = true
	resp.RecursionAvailable = true
	resp.Rcode = dns.RcodeNameError
	resp.Ns = []dns.RR{&dns.SOA{
		Hdr:     dns.RR_Header{Name: LocalZone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: localNegativeTTL},
		Ns:      LocalZone,
		Mbox:    "hostmaster." + LocalZone,
		Serial:  1,
		Refresh: 3600,
		Retry:   600,
		Expire:  86400,
		Minttl:  localNegativeTTL,
	}}
	return resp, true
}

func underLocalZone(name string) bool {
	name = strings.ToLower(dns.Fqdn(name))
	return name == LocalZone || strings.HasSuffix(name, "."+LocalZone)
}

type quietPacketConn struct{ net.PacketConn }

func (c quietPacketConn) SetReadDeadline(t time.Time) error {
	if !t.IsZero() && t.Before(time.Now()) {
		return c.PacketConn.SetReadDeadline(t)
	}
	return nil
}

func (d *dnsServer) close() {
	d.udp.Shutdown()
	d.tcp.Shutdown()
}

var HostNames = []string{"host.docker.internal.", "host.fev.internal."}

const hostNameTTL = 60

type hostNames struct {
	Resolver
	addr netip.Addr
}

func (h hostNames) ungated() Resolver {
	if g, ok := h.Resolver.(ipv6Gate); ok {
		h.Resolver = g.Resolver
	}
	return h
}

func (h hostNames) withheld(qtype uint16) bool {
	g, ok := h.Resolver.(ipv6Gate)
	return ok && g.withheld(qtype)
}

func (h hostNames) Resolve(ctx context.Context, name string, qtype uint16) ([]dns.RR, error) {
	if !h.addr.IsValid() || !isHostName(name) {
		return h.Resolver.Resolve(ctx, name, qtype)
	}
	if qtype != dns.TypeA {
		return nil, nil
	}
	return []dns.RR{&dns.A{
		Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: hostNameTTL},
		A:   net.IP(h.addr.AsSlice()),
	}}, nil
}

func isHostName(name string) bool {
	for _, known := range HostNames {
		if strings.EqualFold(name, known) {
			return true
		}
	}
	return false
}

func answerDNS(ctx context.Context, r Resolver, req *dns.Msg) *dns.Msg {
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.RecursionAvailable = true
	if req.Opcode != dns.OpcodeQuery {
		resp.Rcode = dns.RcodeNotImplemented
		return resp
	}
	if len(req.Question) != 1 {
		resp.Rcode = dns.RcodeFormatError
		return resp
	}
	q := req.Question[0]
	switch {
	case q.Qclass != dns.ClassINET:
		resp.Rcode = dns.RcodeNotImplemented
		return resp
	case q.Qtype == dns.TypeANY, q.Qtype == dns.TypeAXFR, q.Qtype == dns.TypeIXFR, q.Qtype == dns.TypeOPT:
		resp.Rcode = dns.RcodeNotImplemented
		return resp
	}

	rrs, err := r.Resolve(ctx, q.Name, q.Qtype)
	if err != nil {
		resp.Rcode = dns.RcodeServerFailure
		return resp
	}
	if len(rrs) == 0 && singleLabel(q.Name) {
		for _, domain := range r.SearchDomains() {
			full := dns.Fqdn(strings.TrimSuffix(q.Name, ".") + "." + domain)
			found, err := r.Resolve(ctx, full, q.Qtype)
			if err != nil || len(found) == 0 {
				continue
			}
			alias := &dns.CNAME{
				Hdr:    dns.RR_Header{Name: q.Name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 0},
				Target: full,
			}
			rrs = append([]dns.RR{alias}, found...)
			break
		}
	}
	if len(rrs) == 0 && (q.Qtype == dns.TypeA || q.Qtype == dns.TypeAAAA) && unknownName(ctx, r, q.Name, q.Qtype) {
		resp.Rcode = dns.RcodeNameError
	}
	resp.Answer = rrs
	return resp
}

func unknownName(ctx context.Context, r Resolver, name string, qtype uint16) bool {
	ask := []uint16{dns.TypeAAAA}
	if qtype == dns.TypeAAAA {
		ask = []uint16{dns.TypeA}
	}
	if g, ok := r.(gated); ok {
		if g.withheld(qtype) {
			ask = append(ask, qtype)
		}
		r = g.ungated()
	}
	for _, t := range ask {
		found, err := r.Resolve(ctx, name, t)
		if err != nil || len(found) > 0 {
			return false
		}
	}
	return true
}

type gated interface {
	withheld(qtype uint16) bool
	ungated() Resolver
}

func singleLabel(name string) bool {
	name = strings.TrimSuffix(name, ".")
	return name != "" && !strings.Contains(name, ".")
}

type ipv6Gate struct {
	Resolver
	usable func() bool
}

func (g ipv6Gate) withheld(qtype uint16) bool {
	return qtype == dns.TypeAAAA && g.usable != nil && !g.usable()
}

func (g ipv6Gate) ungated() Resolver { return g.Resolver }

func (g ipv6Gate) Resolve(ctx context.Context, name string, qtype uint16) ([]dns.RR, error) {
	if g.withheld(qtype) {
		return nil, nil
	}
	return g.Resolver.Resolve(ctx, name, qtype)
}
