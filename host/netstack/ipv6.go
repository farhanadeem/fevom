package netstack

import (
	"net/netip"
	"sync"
	"time"

	"fev/proto/netplan"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

var macLoopback6 = netip.IPv6Loopback()

func (c Config) WithIPv6(p netplan.Plan6) Config {
	c.Subnet6 = p.Subnet(netplan.Subnet6Uplink)
	c.Gateway6 = p.UplinkGateway6()
	c.Host6 = p.Host6()
	c.Own6 = p.Prefix()
	return c
}

func (c Config) isHost(a netip.Addr) bool {
	return (c.Host.IsValid() && a.Unmap() == c.Host) || (c.Host6.IsValid() && a == c.Host6)
}

func (c Config) forwardable6(dst netip.Addr) bool {
	switch {
	case !c.Gateway6.IsValid(), !dst.IsValid(), !dst.Is6(), dst.Is4In6():
		return false
	case dst.IsLoopback(), dst.IsUnspecified(), dst.IsMulticast(), dst.IsLinkLocalUnicast():
		return false
	case c.Own6.IsValid() && c.Own6.Contains(dst), c.Subnet6.IsValid() && c.Subnet6.Contains(dst):
		return false
	}
	return true
}

func ipv6Options() ipv6.Options {
	return ipv6.Options{
		NDPConfigs: ipv6.NDPConfigurations{
			MaxRtrSolicitations:        0,
			HandleRAs:                  ipv6.HandlingRAsDisabled,
			DiscoverDefaultRouters:     false,
			DiscoverMoreSpecificRoutes: false,
			DiscoverOnLinkPrefixes:     false,
			AutoGenGlobalAddresses:     false,
			AutoGenTempGlobalAddresses: false,
		},
		AutoGenLinkLocal: false,
		MLD:              ipv6.MLDOptions{Enabled: false},
		DADConfigs:       stack.DADConfigurations{DupAddrDetectTransmits: 0},
	}
}

func (ns *Stack) bringUp6() []tcpip.Route {
	if !ns.cfg.Gateway6.IsValid() || !ns.cfg.Subnet6.IsValid() {
		return nil
	}
	for _, a := range []netip.Addr{ns.cfg.Gateway6, ns.cfg.Host6} {
		if !a.IsValid() {
			continue
		}
		pa := tcpip.ProtocolAddress{Protocol: ipv6.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: toAddr(a), PrefixLen: ns.cfg.Subnet6.Bits()}}
		if err := ns.stack.AddProtocolAddress(nicID, pa, stack.AddressProperties{}); err != nil {
			ns.log.Warn("netstack: cannot assign an IPv6 address; IPv6 is off", "addr", a, "err", err)
			return nil
		}
	}
	return []tcpip.Route{{Destination: header.IPv6EmptySubnet, NIC: nicID}}
}

type ICMP6Host interface {
	ListenICMP6() (PingConn, error)
}

const icmpQuote6 = header.IPv6MinimumSize + 8

type icmp6Flow struct {
	conn     PingConn
	guestMAC tcpip.LinkAddress
	mu       sync.Mutex
	sent     map[uint16][icmpQuote6]byte
	order    []uint16
}

type icmp6Forwarder struct {
	s     *Stack
	mu    sync.Mutex
	flows map[icmpFlowKey]*icmp6Flow
}

func newICMP6Forwarder(s *Stack) *icmp6Forwarder {
	return &icmp6Forwarder{s: s, flows: map[icmpFlowKey]*icmp6Flow{}}
}

func (f *icmp6Forwarder) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, flow := range f.flows {
		flow.conn.Close()
	}
}

func (f *icmp6Forwarder) handle(_ stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	ip := header.IPv6(pkt.NetworkHeader().Slice())
	if len(ip) < header.IPv6MinimumSize {
		return false
	}
	src, dst := fromAddr(ip.SourceAddress()), fromAddr(ip.DestinationAddress())
	if dst == f.s.cfg.Gateway6 || dst.IsMulticast() || dst.IsLinkLocalUnicast() {
		return false
	}
	transport := stack.PayloadSince(pkt.TransportHeader())
	defer transport.Release()
	msg := header.ICMPv6(transport.AsSlice())
	if len(msg) < header.ICMPv6EchoMinimumSize || msg.Type() != header.ICMPv6EchoRequest {
		return false
	}
	if !f.s.forwardable(dst) {
		return true
	}
	guestMAC := header.Ethernet(pkt.LinkHeader().Slice()).SourceAddress()
	var quote [icmpQuote6]byte
	copy(quote[:], ip[:header.IPv6MinimumSize])
	copy(quote[header.IPv6MinimumSize:], msg[:8])
	hops := int(ip.HopLimit())
	if hops <= 1 {
		f.s.injectICMP6(guestMAC, f.s.cfg.Gateway6, src, header.ICMPv6TimeExceeded, header.ICMPv6HopLimitExceeded, quote[:])
		return true
	}
	h, ok := f.s.host.(ICMP6Host)
	if !ok {
		return true
	}
	key := icmpFlowKey{guest: src, dst: dst, ident: msg.Ident()}
	flow := f.flow(h, key, guestMAC)
	if flow == nil {
		return true
	}
	flow.remember(msg.Sequence(), quote)
	payload := append([]byte(nil), msg[header.ICMPv6EchoMinimumSize:]...)
	if err := flow.conn.Send(dst, msg.Ident(), msg.Sequence(), payload, hops-1); err != nil {
		f.s.injectICMP6(guestMAC, f.s.cfg.Gateway6, src, header.ICMPv6DstUnreachable, header.ICMPv6NetworkUnreachable, quote[:])
	}
	return true
}

func (f *icmp6Forwarder) flow(h ICMP6Host, key icmpFlowKey, guestMAC tcpip.LinkAddress) *icmp6Flow {
	f.mu.Lock()
	defer f.mu.Unlock()
	if flow := f.flows[key]; flow != nil {
		return flow
	}
	if len(f.flows) >= icmpMaxFlows || f.s.ctx.Err() != nil {
		return nil
	}
	conn, err := h.ListenICMP6()
	if err != nil {
		f.s.log.Debug("icmp6: cannot open a socket", "err", err)
		return nil
	}
	flow := &icmp6Flow{conn: conn, guestMAC: guestMAC, sent: map[uint16][icmpQuote6]byte{}}
	f.flows[key] = flow
	f.s.flows.Add(1)
	go func() {
		defer f.s.flows.Done()
		f.receive(key, flow)
		conn.Close()
		f.mu.Lock()
		delete(f.flows, key)
		f.mu.Unlock()
	}()
	return flow
}

func (flow *icmp6Flow) remember(seq uint16, quote [icmpQuote6]byte) {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	if _, known := flow.sent[seq]; !known {
		flow.order = append(flow.order, seq)
		if len(flow.order) > icmpRemembered {
			delete(flow.sent, flow.order[0])
			flow.order = flow.order[1:]
		}
	}
	flow.sent[seq] = quote
}

func (flow *icmp6Flow) recall(seq uint16) ([icmpQuote6]byte, bool) {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	q, ok := flow.sent[seq]
	return q, ok
}

func (f *icmp6Forwarder) receive(key icmpFlowKey, flow *icmp6Flow) {
	buf := make([]byte, 65535)
	for {
		flow.conn.SetReadDeadline(time.Now().Add(icmpIdleTimeout))
		n, from, err := flow.conn.Recv(buf)
		if err != nil {
			return
		}
		msg := header.ICMPv6(buf[:n])
		if len(msg) < header.ICMPv6MinimumSize {
			continue
		}
		switch msg.Type() {
		case header.ICMPv6EchoReply:
			if len(msg) < header.ICMPv6EchoMinimumSize || msg.Ident() != key.ident || from != key.dst {
				continue
			}
			reply := append([]byte(nil), msg...)
			f.s.injectICMP6(flow.guestMAC, key.dst, key.guest, header.ICMPv6EchoReply, 0, reply[header.ICMPv6HeaderSize:])
		case header.ICMPv6DstUnreachable, header.ICMPv6TimeExceeded, header.ICMPv6PacketTooBig, header.ICMPv6ParamProblem:
			inner := msg[header.ICMPv6ErrorHeaderSize:]
			if len(inner) < header.IPv6MinimumSize+8 || header.IPv6(inner).TransportProtocol() != header.ICMPv6ProtocolNumber {
				continue
			}
			echo := header.ICMPv6(inner[header.IPv6MinimumSize:])
			if echo.Type() != header.ICMPv6EchoRequest || echo.Ident() != key.ident {
				continue
			}
			quote, ok := flow.recall(echo.Sequence())
			if !ok {
				continue
			}
			body := append(append([]byte(nil), msg[4:header.ICMPv6ErrorHeaderSize]...), quote[:]...)
			f.s.injectICMP6Raw(flow.guestMAC, from, key.guest, msg.Type(), msg.Code(), body)
		}
	}
}

func (s *Stack) injectICMP6(guestMAC tcpip.LinkAddress, src, to netip.Addr, typ header.ICMPv6Type, code header.ICMPv6Code, body []byte) {
	if typ != header.ICMPv6EchoReply {
		body = append(make([]byte, 4), body...)
	}
	s.injectICMP6Raw(guestMAC, src, to, typ, code, body)
}

func (s *Stack) injectICMP6Raw(guestMAC tcpip.LinkAddress, src, to netip.Addr, typ header.ICMPv6Type, code header.ICMPv6Code, body []byte) {
	const most = header.IPv6MinimumMTU - header.IPv6MinimumSize - header.ICMPv6HeaderSize
	if len(body) > most {
		body = body[:most]
	}
	msg := make([]byte, header.ICMPv6HeaderSize+len(body))
	m := header.ICMPv6(msg)
	m.SetType(typ)
	m.SetCode(code)
	copy(msg[header.ICMPv6HeaderSize:], body)
	m.SetChecksum(header.ICMPv6Checksum(header.ICMPv6ChecksumParams{Header: msg, Src: toAddr(src), Dst: toAddr(to)}))
	pkt := make([]byte, header.IPv6MinimumSize+len(msg))
	header.IPv6(pkt).Encode(&header.IPv6Fields{
		PayloadLength:     uint16(len(msg)),
		TransportProtocol: header.ICMPv6ProtocolNumber,
		HopLimit:          defaultTTL,
		SrcAddr:           toAddr(src),
		DstAddr:           toAddr(to),
	})
	copy(pkt[header.IPv6MinimumSize:], msg)
	if err := s.stack.WritePacketToRemote(nicID, guestMAC, ipv6.ProtocolNumber, buffer.MakeWithData(pkt)); err != nil {
		s.log.Debug("icmp6: cannot send to the guest", "err", err)
	}
}

type synQuotes struct {
	mu sync.Mutex
	m  map[stack.TransportEndpointID]synQuote
}

type synQuote struct {
	quote [icmpQuote6]byte
	mac   tcpip.LinkAddress
}

const synQuotesMax = 1024

func (q *synQuotes) keep(id stack.TransportEndpointID, pkt *stack.PacketBuffer) {
	if id.LocalAddress.Len() != 16 {
		return
	}
	th, nh := pkt.TransportHeader().Slice(), pkt.NetworkHeader().Slice()
	if len(th) < header.TCPMinimumSize || len(nh) < header.IPv6MinimumSize {
		return
	}
	if flags := header.TCP(th).Flags(); flags&header.TCPFlagSyn == 0 || flags&header.TCPFlagAck != 0 {
		return
	}
	var sq synQuote
	copy(sq.quote[:], nh[:header.IPv6MinimumSize])
	copy(sq.quote[header.IPv6MinimumSize:], th[:8])
	if eth := header.Ethernet(pkt.LinkHeader().Slice()); len(eth) >= header.EthernetMinimumSize {
		sq.mac = eth.SourceAddress()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.m == nil {
		q.m = map[stack.TransportEndpointID]synQuote{}
	}
	if _, known := q.m[id]; !known && len(q.m) >= synQuotesMax {
		return
	}
	q.m[id] = sq
}

func (q *synQuotes) take(id stack.TransportEndpointID) (synQuote, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	sq, ok := q.m[id]
	delete(q.m, id)
	return sq, ok
}
