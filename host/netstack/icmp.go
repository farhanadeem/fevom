package netstack

import (
	"net/netip"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

type PingConn interface {
	Send(dst netip.Addr, ident, seq uint16, payload []byte, ttl int) error
	Recv(buf []byte) (n int, from netip.Addr, err error)
	SetReadDeadline(time.Time) error
	Close() error
}

const (
	icmpIdleTimeout = 30 * time.Second
	icmpMaxFlows    = 256
	icmpQuote       = header.IPv4MinimumSize + 8
	icmpRemembered  = 64
	defaultTTL      = 64
)

type icmpFlowKey struct {
	guest, dst netip.Addr
	ident      uint16
}

type icmpFlow struct {
	conn     PingConn
	guestMAC tcpip.LinkAddress
	mu       sync.Mutex
	sent     map[uint16][icmpQuote]byte
	order    []uint16
}

type icmpForwarder struct {
	s     *Stack
	mu    sync.Mutex
	flows map[icmpFlowKey]*icmpFlow
}

func newICMPForwarder(s *Stack) *icmpForwarder {
	return &icmpForwarder{s: s, flows: map[icmpFlowKey]*icmpFlow{}}
}

func (f *icmpForwarder) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, flow := range f.flows {
		flow.conn.Close()
	}
}

func (f *icmpForwarder) handle(_ stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	ip := header.IPv4(pkt.NetworkHeader().Slice())
	if len(ip) < header.IPv4MinimumSize {
		return false
	}
	src, dst := fromAddr(ip.SourceAddress()), fromAddr(ip.DestinationAddress())
	if dst == f.s.cfg.Gateway {
		return false
	}
	transport := stack.PayloadSince(pkt.TransportHeader())
	defer transport.Release()
	msg := header.ICMPv4(transport.AsSlice())
	if len(msg) < header.ICMPv4MinimumSize || msg.Type() != header.ICMPv4Echo {
		return true
	}
	if !f.s.forwardable(dst) {
		return true
	}
	guestMAC := header.Ethernet(pkt.LinkHeader().Slice()).SourceAddress()

	var quote [icmpQuote]byte
	copy(quote[:], ip[:header.IPv4MinimumSize])
	copy(quote[header.IPv4MinimumSize:], msg[:8])

	ttl := int(ip.TTL())
	if ttl <= 1 {
		f.s.injectICMPError(guestMAC, f.s.cfg.Gateway, src, header.ICMPv4TimeExceeded, 0, quote[:])
		return true
	}

	key := icmpFlowKey{guest: src, dst: dst, ident: msg.Ident()}
	flow := f.flow(key, guestMAC)
	if flow == nil {
		return true
	}
	flow.remember(msg.Sequence(), quote)
	payload := append([]byte(nil), msg.Payload()...)
	f.s.log.Debug("icmp: to the host", "dst", dst, "ident", msg.Ident(), "seq", msg.Sequence(), "ttl", ttl-1, "payload", len(payload))
	if err := flow.conn.Send(dst, msg.Ident(), msg.Sequence(), payload, ttl-1); err != nil {
		f.s.log.Debug("icmp: send failed", "dst", dst, "err", err)
		f.s.injectICMPError(guestMAC, f.s.cfg.Gateway, src, header.ICMPv4DstUnreachable, header.ICMPv4NetUnreachable, quote[:])
	}
	return true
}

func (f *icmpForwarder) flow(key icmpFlowKey, guestMAC tcpip.LinkAddress) *icmpFlow {
	f.mu.Lock()
	defer f.mu.Unlock()
	if flow := f.flows[key]; flow != nil {
		return flow
	}
	if len(f.flows) >= icmpMaxFlows || f.s.ctx.Err() != nil {
		return nil
	}
	conn, err := f.s.host.ListenICMP()
	if err != nil {
		f.s.log.Debug("icmp: cannot open a socket", "err", err)
		return nil
	}
	flow := &icmpFlow{conn: conn, guestMAC: guestMAC, sent: map[uint16][icmpQuote]byte{}}
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

func (flow *icmpFlow) remember(seq uint16, quote [icmpQuote]byte) {
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

func (flow *icmpFlow) recall(seq uint16) ([icmpQuote]byte, bool) {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	q, ok := flow.sent[seq]
	return q, ok
}

func (f *icmpForwarder) receive(key icmpFlowKey, flow *icmpFlow) {
	buf := make([]byte, 65535)
	for {
		flow.conn.SetReadDeadline(time.Now().Add(icmpIdleTimeout))
		n, from, err := flow.conn.Recv(buf)
		if err != nil {
			return
		}
		msg := header.ICMPv4(buf[:n])
		if len(msg) < header.ICMPv4MinimumSize {
			continue
		}
		f.s.log.Debug("icmp: from the host", "from", from, "type", msg.Type(), "code", msg.Code(), "bytes", n, "flow", key.ident)
		switch msg.Type() {
		case header.ICMPv4EchoReply:
			if msg.Ident() != key.ident || from != key.dst {
				continue
			}
			f.s.injectEchoReply(flow.guestMAC, key.dst, key.guest, key.ident, msg.Sequence(), msg.Payload())
		case header.ICMPv4TimeExceeded, header.ICMPv4DstUnreachable:
			inner := msg.Payload()
			if len(inner) < header.IPv4MinimumSize {
				continue
			}
			ihl := int(inner[0]&0x0f) * 4
			if ihl < header.IPv4MinimumSize || len(inner) < ihl+8 || inner[9] != byte(header.ICMPv4ProtocolNumber) {
				continue
			}
			echo := header.ICMPv4(inner[ihl : ihl+8])
			if echo.Type() != header.ICMPv4Echo || echo.Ident() != key.ident {
				continue
			}
			quote, ok := flow.recall(echo.Sequence())
			if !ok {
				continue
			}
			f.s.injectICMPError(flow.guestMAC, from, key.guest, msg.Type(), msg.Code(), quote[:])
		}
	}
}

func ipv4Packet(src, dst netip.Addr, icmpMsg []byte) []byte {
	total := header.IPv4MinimumSize + len(icmpMsg)
	pkt := make([]byte, total)
	ip := header.IPv4(pkt)
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(total),
		TTL:         defaultTTL,
		Protocol:    uint8(header.ICMPv4ProtocolNumber),
		SrcAddr:     toAddr(src),
		DstAddr:     toAddr(dst),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	copy(pkt[header.IPv4MinimumSize:], icmpMsg)
	return pkt
}

func (s *Stack) inject(guestMAC tcpip.LinkAddress, packet []byte) {
	if err := s.stack.WritePacketToRemote(nicID, guestMAC, ipv4.ProtocolNumber, buffer.MakeWithData(packet)); err != nil {
		s.log.Debug("icmp: cannot send to the guest", "err", err)
	}
}

func (s *Stack) injectEchoReply(guestMAC tcpip.LinkAddress, from, to netip.Addr, ident, seq uint16, payload []byte) {
	msg := make([]byte, header.ICMPv4MinimumSize+len(payload))
	icmp := header.ICMPv4(msg)
	icmp.SetType(header.ICMPv4EchoReply)
	icmp.SetIdent(ident)
	icmp.SetSequence(seq)
	copy(msg[header.ICMPv4MinimumSize:], payload)
	icmp.SetChecksum(^checksum.Checksum(msg, 0))
	s.inject(guestMAC, ipv4Packet(from, to, msg))
}

func (s *Stack) injectICMPError(guestMAC tcpip.LinkAddress, router, to netip.Addr, typ header.ICMPv4Type, code header.ICMPv4Code, quote []byte) {
	msg := make([]byte, header.ICMPv4MinimumSize+len(quote))
	icmp := header.ICMPv4(msg)
	icmp.SetType(typ)
	icmp.SetCode(code)
	copy(msg[header.ICMPv4MinimumSize:], quote)
	icmp.SetChecksum(^checksum.Checksum(msg, 0))
	s.inject(guestMAC, ipv4Packet(router, to, msg))
}

func stripIPv4Header(buf []byte, n int) int {
	if n < header.IPv4MinimumSize || buf[0]>>4 != 4 {
		return n
	}
	ihl := int(buf[0]&0x0f) * 4
	if ihl < header.IPv4MinimumSize || ihl > n {
		return n
	}
	return copy(buf, buf[ihl:n])
}
