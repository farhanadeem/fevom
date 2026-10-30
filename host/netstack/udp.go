package netstack

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

type UDPConn interface {
	net.Conn
	SetTTL(ttl int) error
	LocalPort() uint16
}

const (
	udpProbeTTL = 32
)

type udpFlow struct {
	guest, dst netip.AddrPort
	guestMAC   tcpip.LinkAddress
	mu         sync.Mutex
	quote      [icmpQuote6]byte
	quoteLen   int
	probing    bool
}

func (f *udpFlow) setProbing() {
	f.mu.Lock()
	f.probing = true
	f.mu.Unlock()
}

func (f *udpFlow) isProbing() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probing
}

func icmpSoftError(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTDOWN)
}

func (f *udpFlow) setQuote(ttl uint8, length int) {
	var q [icmpQuote6]byte
	n := header.IPv4MinimumSize
	if f.guest.Addr().Is6() {
		n = header.IPv6MinimumSize
		header.IPv6(q[:n]).Encode(&header.IPv6Fields{
			PayloadLength:     uint16(header.UDPMinimumSize + length),
			TransportProtocol: header.UDPProtocolNumber,
			HopLimit:          ttl,
			SrcAddr:           toAddr(f.guest.Addr()),
			DstAddr:           toAddr(f.dst.Addr()),
		})
	} else {
		ip := header.IPv4(q[:n])
		ip.Encode(&header.IPv4Fields{
			TotalLength: uint16(header.IPv4MinimumSize + header.UDPMinimumSize + length),
			TTL:         ttl,
			Protocol:    uint8(header.UDPProtocolNumber),
			SrcAddr:     toAddr(f.guest.Addr()),
			DstAddr:     toAddr(f.dst.Addr()),
		})
		ip.SetChecksum(^ip.CalculateChecksum())
	}
	u := q[n:]
	binary.BigEndian.PutUint16(u[0:], f.guest.Port())
	binary.BigEndian.PutUint16(u[2:], f.dst.Port())
	binary.BigEndian.PutUint16(u[4:], uint16(header.UDPMinimumSize+length))
	f.mu.Lock()
	f.quote = q
	f.quoteLen = n + header.UDPMinimumSize
	f.mu.Unlock()
}

func (f *udpFlow) getQuote() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.quote
	return q[:f.quoteLen]
}

type flowError int

const (
	errNoRoute flowError = iota
	errPortUnreachable
	errTimeExceeded
)

func (s *Stack) injectFlowError(flow *udpFlow, from netip.Addr, e flowError) {
	if flow.guest.Addr().Is6() {
		typ, code := header.ICMPv6DstUnreachable, header.ICMPv6NetworkUnreachable
		switch e {
		case errPortUnreachable:
			code = header.ICMPv6PortUnreachable
		case errTimeExceeded:
			typ, code = header.ICMPv6TimeExceeded, header.ICMPv6HopLimitExceeded
		}
		s.injectICMP6(flow.guestMAC, from, flow.guest.Addr(), typ, code, flow.getQuote())
		return
	}
	typ, code := header.ICMPv4DstUnreachable, header.ICMPv4NetUnreachable
	switch e {
	case errPortUnreachable:
		code = header.ICMPv4PortUnreachable
	case errTimeExceeded:
		typ, code = header.ICMPv4TimeExceeded, 0
	}
	s.injectICMPError(flow.guestMAC, from, flow.guest.Addr(), typ, code, flow.getQuote())
}

func (s *Stack) gateway(a netip.Addr) netip.Addr {
	if a.Is6() && !a.Is4In6() {
		return s.cfg.Gateway6
	}
	return s.cfg.Gateway
}

func (s *Stack) forwardUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	flow := &udpFlow{
		guest: netip.AddrPortFrom(fromAddr(id.RemoteAddress), id.RemotePort),
		dst:   netip.AddrPortFrom(fromAddr(id.LocalAddress), id.LocalPort),
	}
	mac, ok := s.route(flow.dst)
	if !ok {
		return true
	}
	flow.guestMAC = s.guestMAC(flow.guest.Addr())
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		return true
	}
	ep.SocketOptions().SetReceiveTTL(true)
	s.flows.Add(1)
	go func() {
		defer s.flows.Done()
		s.relayUDP(flow, mac, ep, &wq)
	}()
	return true
}

func (s *Stack) relayUDP(flow *udpFlow, mac netip.AddrPort, ep tcpip.Endpoint, wq *waiter.Queue) {
	guest := gonet.NewUDPConn(wq, ep)
	defer guest.Close()
	host, err := s.host.DialUDP(s.ctx, mac)
	if err != nil {
		s.log.Debug("udp: dial failed", "dst", flow.dst, "err", err)
		flow.setQuote(defaultTTL, 0)
		s.injectFlowError(flow, s.gateway(flow.guest.Addr()), errNoRoute)
		return
	}
	defer host.Close()
	defer s.udpErrors.unregister(host.LocalPort())

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	defer s.udpFlows.add(host, cancel)()
	idle := newIdle(&s.timers, udpUnreplied, udpAssured, cancel)
	defer idle.Stop()

	go func() {
		defer cancel()
		buf := make([]byte, 65535)
		for {
			n, err := host.Read(buf)
			if err != nil {
				if !icmpSoftError(err) {
					return
				}
				if errors.Is(err, syscall.ECONNREFUSED) && !flow.isProbing() {
					s.injectFlowError(flow, flow.dst.Addr(), errPortUnreachable)
				}
				continue
			}
			idle.Replied()
			if _, err := guest.Write(buf[:n]); err != nil {
				return
			}
		}
	}()

	entry, readable := waiter.NewChannelEntry(waiter.ReadableEvents)
	wq.EventRegister(&entry)
	defer wq.EventUnregister(&entry)
	hostTTL := -1
	var buf bytes.Buffer
	for {
		buf.Reset()
		res, rerr := ep.Read(&buf, tcpip.ReadOptions{})
		if _, wouldBlock := rerr.(*tcpip.ErrWouldBlock); wouldBlock {
			select {
			case <-readable:
				continue
			case <-ctx.Done():
				return
			}
		}
		if rerr != nil {
			return
		}
		idle.Sent()
		ttl := uint8(defaultTTL)
		if res.ControlMessages.HasTTL {
			ttl = res.ControlMessages.TTL
		}
		flow.setQuote(ttl, buf.Len())
		if ttl <= 1 {
			s.injectFlowError(flow, s.gateway(flow.guest.Addr()), errTimeExceeded)
			continue
		}
		if int(ttl)-1 != hostTTL {
			if err := host.SetTTL(int(ttl) - 1); err != nil {
				return
			}
			hostTTL = int(ttl) - 1
		}
		if ttl <= udpProbeTTL && flow.guest.Addr().Is4() {
			flow.setProbing()
			s.udpErrors.register(host.LocalPort(), flow)
		}
		if _, err := host.Write(buf.Bytes()); err != nil && !icmpSoftError(err) {
			return
		}
	}
}

type udpErrorRelay struct {
	s     *Stack
	mu    sync.Mutex
	conn  PingConn
	flows map[uint16]*udpFlow
}

func (r *udpErrorRelay) register(hostPort uint16, flow *udpFlow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, known := r.flows[hostPort]; known {
		return
	}
	if r.conn == nil {
		conn, err := r.s.host.ListenICMP()
		if err != nil {
			r.s.log.Debug("udp: cannot listen for ICMP errors", "err", err)
			return
		}
		r.conn = conn
		go r.receive(conn)
	}
	r.flows[hostPort] = flow
}

func (r *udpErrorRelay) unregister(hostPort uint16) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, known := r.flows[hostPort]; !known {
		return
	}
	delete(r.flows, hostPort)
	if len(r.flows) == 0 && r.conn != nil {
		r.conn.Close()
		r.conn = nil
	}
}

func (r *udpErrorRelay) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		r.conn.Close()
		r.conn = nil
	}
	r.flows = map[uint16]*udpFlow{}
}

func (r *udpErrorRelay) receive(conn PingConn) {
	buf := make([]byte, 65535)
	for {
		n, from, err := conn.Recv(buf)
		if err != nil {
			return
		}
		msg := header.ICMPv4(buf[:n])
		if len(msg) < header.ICMPv4MinimumSize {
			continue
		}
		r.s.log.Debug("udp: ICMP from the host", "from", from, "type", msg.Type(), "code", msg.Code(), "bytes", n, "head", fmt.Sprintf("% x", buf[:min(n, 40)]))
		if t := msg.Type(); t != header.ICMPv4TimeExceeded && t != header.ICMPv4DstUnreachable {
			continue
		}
		inner := msg.Payload()
		if len(inner) < header.IPv4MinimumSize {
			continue
		}
		ihl := int(inner[0]&0x0f) * 4
		if ihl < header.IPv4MinimumSize || len(inner) < ihl+4 || inner[9] != byte(header.UDPProtocolNumber) {
			continue
		}
		hostPort := binary.BigEndian.Uint16(inner[ihl:])
		dstPort := binary.BigEndian.Uint16(inner[ihl+2:])
		r.mu.Lock()
		flow := r.flows[hostPort]
		r.mu.Unlock()
		if flow == nil || flow.dst.Port() != dstPort || fromAddr(header.IPv4(inner).DestinationAddress()) != flow.dst.Addr() {
			continue
		}
		r.s.injectICMPError(flow.guestMAC, from, flow.guest.Addr(), msg.Type(), msg.Code(), flow.getQuote())
	}
}
