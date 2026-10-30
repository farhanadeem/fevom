package domains

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const (
	PositiveTTL = 10
	NegativeTTL = 1
)

func (r *Registry) Answer(req *dns.Msg) (*dns.Msg, bool) {
	if req == nil || len(req.Question) != 1 {
		return nil, false
	}
	q := req.Question[0]
	if !r.Owns(q.Name) {
		return nil, false
	}
	return r.answer(req, false), true
}

func (r *Registry) AnswerGuest(req *dns.Msg) (*dns.Msg, bool) {
	if req == nil || len(req.Question) != 1 {
		return nil, false
	}
	if !r.Owns(req.Question[0].Name) {
		return nil, false
	}
	return r.answer(req, true), true
}

func (r *Registry) answer(req *dns.Msg, guest bool) *dns.Msg {
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Authoritative = true
	resp.RecursionAvailable = req.RecursionDesired
	q := req.Question[0]
	if req.Opcode != dns.OpcodeQuery || q.Qclass != dns.ClassINET {
		resp.Rcode = dns.RcodeNotImplemented
		return resp
	}
	name := canonical(q.Name)
	a := r.Lookup(name)
	if !a.Found {
		resp.Rcode = dns.RcodeNameError
		resp.Ns = []dns.RR{r.soa(r.zoneOf(name))}
		return resp
	}
	if q.Qtype == dns.TypeA && a.Address.Is4() {
		resp.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: PositiveTTL},
			A:   net.IP(a.Address.AsSlice()),
		}}
		return resp
	}
	six := a.Mac6
	if guest {
		six = a.Address6
	}
	if q.Qtype == dns.TypeAAAA && six.IsValid() {
		resp.Answer = []dns.RR{&dns.AAAA{
			Hdr:  dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: PositiveTTL},
			AAAA: net.IP(six.AsSlice()),
		}}
		return resp
	}
	resp.Ns = []dns.RR{r.soa(r.zoneOf(name))}
	return resp
}

func (r *Registry) soa(zone string) dns.RR {
	apex := dns.Fqdn(zone)
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: apex, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: NegativeTTL},
		Ns:      dns.Fqdn(Zone),
		Mbox:    "hostmaster." + dns.Fqdn(Zone),
		Serial:  uint32(r.Generation()),
		Refresh: 3600,
		Retry:   600,
		Expire:  86400,
		Minttl:  NegativeTTL,
	}
}

type Server struct {
	udp, tcp *dns.Server
	addr     netip.AddrPort
	once     sync.Once
}

func Serve(addr netip.AddrPort, r *Registry) (*Server, error) {
	pc, err := net.ListenPacket("udp", addr.String())
	if err != nil {
		return nil, err
	}
	bound := pc.LocalAddr().(*net.UDPAddr).AddrPort()
	l, err := net.Listen("tcp", netip.AddrPortFrom(addr.Addr(), bound.Port()).String())
	if err != nil {
		pc.Close()
		return nil, err
	}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		resp, ok := r.Forward(req)
		if !ok {
			resp, ok = r.Answer(req)
		}
		if !ok {
			resp = new(dns.Msg)
			resp.SetReply(req)
			resp.Rcode = dns.RcodeRefused
			if len(req.Question) == 1 && under(canonical(req.Question[0].Name), "local") {
				resp.Authoritative = true
				resp.Rcode = dns.RcodeNameError
				resp.Ns = []dns.RR{r.soa(canonical(req.Question[0].Name))}
			}
		}
		if w.RemoteAddr().Network() == "udp" {
			size := dns.MinMsgSize
			if opt := req.IsEdns0(); opt != nil && int(opt.UDPSize()) > size {
				size = int(opt.UDPSize())
			}
			resp.Truncate(size)
		}
		w.WriteMsg(resp)
	})
	s := &Server{
		udp:  &dns.Server{PacketConn: quietPacketConn{pc}, Handler: handler},
		tcp:  &dns.Server{Listener: l, Handler: handler, IdleTimeout: tcpIdle},
		addr: netip.AddrPortFrom(addr.Addr(), bound.Port()),
	}
	go s.udp.ActivateAndServe()
	go s.tcp.ActivateAndServe()
	return s, nil
}

func tcpIdle() time.Duration { return 8 * time.Second }

func (s *Server) Addr() netip.AddrPort { return s.addr }

func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err = errors.Join(s.udp.ShutdownContext(ctx), s.tcp.ShutdownContext(ctx))
	})
	return err
}

type quietPacketConn struct{ net.PacketConn }

func (c quietPacketConn) SetReadDeadline(t time.Time) error {
	if !t.IsZero() && t.Before(time.Now()) {
		return c.PacketConn.SetReadDeadline(t)
	}
	return nil
}
