package netstack

import (
	"encoding/binary"
	"net/netip"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"

	"fev/proto/netplan"
)

const nat64Overhead = header.IPv6MinimumSize - header.IPv4MinimumSize

func (t *Tunnel) SetNAT64(ps []netip.Prefix) {
	c := make([]netip.Prefix, 0, len(ps))
	for _, p := range ps {
		if netplan.CheckNAT64(p) == nil {
			c = append(c, p)
		}
	}
	t.nat64.Store(&c)
}

func (t *Tunnel) NAT64() []netip.Prefix {
	if p := t.nat64.Load(); p != nil {
		return append([]netip.Prefix(nil), (*p)...)
	}
	return nil
}

func (t *Tunnel) nat64Dst(dst netip.Addr) (netip.Addr, bool) {
	if !t.cfg.Mac46.IsValid() || !t.cfg.Mac6.IsValid() {
		return netip.Addr{}, false
	}
	p := t.nat64.Load()
	if p == nil {
		return netip.Addr{}, false
	}
	for _, pref := range *p {
		if v4, ok := netplan.NAT64V4(pref, dst); ok {
			return v4, true
		}
	}
	return netip.Addr{}, false
}

func (t *Tunnel) nat64Out() (netip.Prefix, bool) {
	p := t.nat64.Load()
	if p == nil || len(*p) == 0 || !t.cfg.Mac6.IsValid() {
		return netip.Prefix{}, false
	}
	return (*p)[0], true
}

func csumWords(b ...[]byte) uint16 {
	var s uint16
	for _, x := range b {
		s = checksum.Combine(s, checksum.Checksum(x, 0))
	}
	return s
}

func csumAdjust(f []byte, old, new uint16) {
	hc := binary.BigEndian.Uint16(f)
	s := checksum.Combine(^hc, ^old)
	s = checksum.Combine(s, new)
	binary.BigEndian.PutUint16(f, ^s)
}

func l4ChecksumAt(proto uint8, l4 []byte) int {
	switch proto {
	case uint8(header.TCPProtocolNumber):
		if len(l4) >= header.TCPMinimumSize {
			return 16
		}
	case uint8(header.UDPProtocolNumber):
		if len(l4) >= header.UDPMinimumSize {
			return 6
		}
	}
	return -1
}

type v6Fragment struct {
	id     uint32
	offset uint16
	more   bool
}

func v6Headers(pkt []byte) (proto uint8, hl int, frag *v6Fragment, ok bool) {
	if len(pkt) < header.IPv6MinimumSize {
		return 0, 0, nil, false
	}
	proto, hl = pkt[6], header.IPv6MinimumSize
	if proto == uint8(header.IPv6FragmentExtHdrIdentifier) {
		if len(pkt) < hl+header.IPv6FragmentExtHdrLength {
			return 0, 0, nil, false
		}
		f := pkt[hl:]
		frag = &v6Fragment{id: binary.BigEndian.Uint32(f[4:]), offset: binary.BigEndian.Uint16(f[2:]) >> 3, more: f[3]&1 != 0}
		proto, hl = f[0], hl+header.IPv6FragmentExtHdrLength
	}
	switch proto {
	case uint8(header.TCPProtocolNumber), uint8(header.UDPProtocolNumber), uint8(header.ICMPv6ProtocolNumber):
		return proto, hl, frag, true
	}
	return 0, 0, nil, false
}

func to4(frame []byte, room int, src4, dst4 netip.Addr, quoteSrc func(netip.Addr) (netip.Addr, bool), quoteDst netip.Addr) ([]byte, bool) {
	pkt := frame[room:]
	proto, hl, frag, ok := v6Headers(pkt)
	if !ok {
		return nil, false
	}
	tc := uint8(binary.BigEndian.Uint16(pkt[0:]) >> 4)
	hop := pkt[7]
	src6, dst6 := pkt[8:24], pkt[24:40]
	payload := pkt[hl:]
	first := frag == nil || frag.offset == 0
	var out []byte
	if proto == uint8(header.ICMPv6ProtocolNumber) {
		if frag != nil {
			return nil, false
		}
		msg, ok := icmp6to4(payload, quoteSrc, quoteDst)
		if !ok {
			return nil, false
		}
		out = make([]byte, room+header.IPv4MinimumSize+len(msg))
		copy(out[room+header.IPv4MinimumSize:], msg)
		proto = uint8(header.ICMPv4ProtocolNumber)
	} else {
		if first {
			if at := l4ChecksumAt(proto, payload); at >= 0 {
				s4, d4 := src4.As4(), dst4.As4()
				csumAdjust(payload[at:], csumWords(src6, dst6), csumWords(s4[:], d4[:]))
				if proto == uint8(header.UDPProtocolNumber) && binary.BigEndian.Uint16(payload[at:]) == 0 {
					binary.BigEndian.PutUint16(payload[at:], 0xffff)
				}
			} else if frag == nil {
				return nil, false
			}
		}
		out = frame[hl-header.IPv4MinimumSize:]
	}
	ip := header.IPv4(out[room:])
	f := header.IPv4Fields{
		TOS:         tc,
		TotalLength: uint16(len(out) - room),
		TTL:         hop,
		Protocol:    proto,
		SrcAddr:     tcpip.AddrFrom4(src4.As4()),
		DstAddr:     tcpip.AddrFrom4(dst4.As4()),
	}
	if frag != nil {
		f.ID = uint16(frag.id)
		f.FragmentOffset = frag.offset * 8
		if frag.more {
			f.Flags = header.IPv4FlagMoreFragments
		}
	} else {
		f.Flags = header.IPv4FlagDontFragment
	}
	ip.Encode(&f)
	ip.SetChecksum(^ip.CalculateChecksum())
	if proto == uint8(header.ICMPv4ProtocolNumber) {
		m := out[room+header.IPv4MinimumSize:]
		binary.BigEndian.PutUint16(m[2:], 0)
		binary.BigEndian.PutUint16(m[2:], ^checksum.Checksum(m, 0))
	}
	return out, true
}

func icmp6to4(m []byte, quoteSrc func(netip.Addr) (netip.Addr, bool), quoteDst netip.Addr) ([]byte, bool) {
	if len(m) < header.ICMPv6MinimumSize {
		return nil, false
	}
	typ, code := header.ICMPv6Type(m[0]), m[1]
	var t4, c4 uint8
	var mtu uint16
	switch typ {
	case header.ICMPv6EchoRequest, header.ICMPv6EchoReply:
		out := append([]byte(nil), m...)
		out[0] = uint8(header.ICMPv4Echo)
		if typ == header.ICMPv6EchoReply {
			out[0] = uint8(header.ICMPv4EchoReply)
		}
		return out, true
	case header.ICMPv6DstUnreachable:
		t4 = uint8(header.ICMPv4DstUnreachable)
		switch code {
		case 0, 2, 3:
			c4 = uint8(header.ICMPv4HostUnreachable)
		case 1:
			c4 = uint8(header.ICMPv4HostProhibited)
		case 4:
			c4 = uint8(header.ICMPv4PortUnreachable)
		default:
			return nil, false
		}
	case header.ICMPv6PacketTooBig:
		t4, c4 = uint8(header.ICMPv4DstUnreachable), uint8(header.ICMPv4FragmentationNeeded)
		m6 := binary.BigEndian.Uint32(m[4:])
		if m6 < header.IPv4MinimumMTU+nat64Overhead {
			m6 = header.IPv4MinimumMTU + nat64Overhead
		}
		mtu = uint16(min(m6-nat64Overhead, 65535))
	case header.ICMPv6TimeExceeded:
		t4, c4 = uint8(header.ICMPv4TimeExceeded), code
	case header.ICMPv6ParamProblem:
		if code != 1 {
			return nil, false
		}
		t4, c4 = uint8(header.ICMPv4DstUnreachable), uint8(header.ICMPv4ProtoUnreachable)
	default:
		return nil, false
	}
	inner, ok := quote6to4(m[header.ICMPv6MinimumSize:], quoteSrc, quoteDst)
	if !ok {
		return nil, false
	}
	out := make([]byte, header.ICMPv4MinimumSize+len(inner))
	out[0], out[1] = t4, c4
	binary.BigEndian.PutUint16(out[6:], mtu)
	copy(out[header.ICMPv4MinimumSize:], inner)
	return out, true
}

func quote6to4(q []byte, quoteSrc func(netip.Addr) (netip.Addr, bool), quoteDst netip.Addr) ([]byte, bool) {
	if len(q) < header.IPv6MinimumSize {
		return nil, false
	}
	src4, ok := quoteSrc(netip.AddrFrom16([16]byte(q[8:24])))
	if !ok {
		return nil, false
	}
	proto, hl, frag, ok := v6Headers(q)
	if !ok {
		proto, hl, frag = q[6], header.IPv6MinimumSize, nil
		if proto == uint8(header.IPv6FragmentExtHdrIdentifier) {
			return nil, false
		}
	}
	if proto == uint8(header.ICMPv6ProtocolNumber) {
		proto = uint8(header.ICMPv4ProtocolNumber)
	}
	body := q[min(hl, len(q)):]
	out := make([]byte, header.IPv4MinimumSize+len(body))
	copy(out[header.IPv4MinimumSize:], body)
	f := header.IPv4Fields{
		TOS: uint8(binary.BigEndian.Uint16(q[0:]) >> 4), TotalLength: uint16(header.IPv4MinimumSize + int(binary.BigEndian.Uint16(q[4:])) - (hl - header.IPv6MinimumSize)),
		TTL: q[7], Protocol: proto, SrcAddr: tcpip.AddrFrom4(src4.As4()), DstAddr: tcpip.AddrFrom4(quoteDst.As4()),
	}
	if frag != nil {
		f.ID, f.FragmentOffset = uint16(frag.id), frag.offset*8
		if frag.more {
			f.Flags = header.IPv4FlagMoreFragments
		}
	} else {
		f.Flags = header.IPv4FlagDontFragment
	}
	header.IPv4(out).Encode(&f)
	header.IPv4(out).SetChecksum(^header.IPv4(out).CalculateChecksum())
	if at := l4ChecksumAt(proto, out[header.IPv4MinimumSize:]); at >= 0 && (frag == nil || frag.offset == 0) && at+2 <= len(body) {
		s4, d4 := src4.As4(), quoteDst.As4()
		csumAdjust(out[header.IPv4MinimumSize+at:], csumWords(q[8:24], q[24:40]), csumWords(s4[:], d4[:]))
	}
	return out, true
}

func to6(buf []byte, room int, pref netip.Prefix, mac6 netip.Addr, mtu int, quoteDst func(netip.Addr) (netip.Addr, bool), emit func([]byte), tooBig func(mtu int)) bool {
	pkt := buf[room:]
	v4, ihl, proto, total, ok := ipHeaders(pkt)
	if !ok || !v4 {
		return false
	}
	pkt = pkt[:total]
	ip := header.IPv4(pkt)
	src4 := netip.AddrFrom4([4]byte(pkt[12:16]))
	src6 := netplan.NAT64Address(pref, src4)
	flagsOff := binary.BigEndian.Uint16(pkt[6:])
	df, more, offset := flagsOff&0x4000 != 0, flagsOff&0x2000 != 0, flagsOff&0x1fff
	fragmented := more || offset != 0
	payload := pkt[ihl:]
	nproto := proto
	if proto == uint8(header.ICMPv4ProtocolNumber) {
		if fragmented {
			return false
		}
		msg, ok := icmp4to6(payload, quoteDst, pref)
		if !ok {
			return false
		}
		payload, nproto = msg, uint8(header.ICMPv6ProtocolNumber)
		room = 0
	} else if !fragmented || offset == 0 {
		at := l4ChecksumAt(proto, payload)
		switch {
		case at < 0 && !fragmented:
			return false
		case at >= 0 && proto == uint8(header.UDPProtocolNumber) && binary.BigEndian.Uint16(payload[at:]) == 0:
			if fragmented {
				return false
			}
			sum := header.PseudoHeaderChecksum(header.UDPProtocolNumber, tcpip.AddrFrom16(src6.As16()), tcpip.AddrFrom16(mac6.As16()), uint16(len(payload)))
			c := ^checksum.Checksum(payload, sum)
			if c == 0 {
				c = 0xffff
			}
			binary.BigEndian.PutUint16(payload[at:], c)
		case at >= 0:
			s, m := src6.As16(), mac6.As16()
			csumAdjust(payload[at:], csumWords(pkt[12:16], pkt[16:20]), csumWords(s[:], m[:]))
			if proto == uint8(header.UDPProtocolNumber) && binary.BigEndian.Uint16(payload[at:]) == 0 {
				binary.BigEndian.PutUint16(payload[at:], 0xffff)
			}
		}
	}
	hdr6 := func(b []byte, plen int, next uint8) {
		header.IPv6(b).Encode(&header.IPv6Fields{
			TrafficClass: pkt[1], PayloadLength: uint16(plen), TransportProtocol: tcpip.TransportProtocolNumber(next),
			HopLimit: ip.TTL(), SrcAddr: tcpip.AddrFrom16(src6.As16()), DstAddr: tcpip.AddrFrom16(mac6.As16()),
		})
	}
	if nproto == uint8(header.ICMPv6ProtocolNumber) {
		m := payload
		binary.BigEndian.PutUint16(m[2:], 0)
		sum := header.PseudoHeaderChecksum(header.ICMPv6ProtocolNumber, tcpip.AddrFrom16(src6.As16()), tcpip.AddrFrom16(mac6.As16()), uint16(len(m)))
		binary.BigEndian.PutUint16(m[2:], ^checksum.Checksum(m, sum))
	}
	whole := header.IPv6MinimumSize + len(payload)
	if !fragmented && whole <= mtu {
		if lead := header.IPv6MinimumSize - ihl; room >= tunHdr+lead {
			b := buf[room-lead-tunHdr : room+total]
			hdr6(b[tunHdr:], len(payload), nproto)
			emit(b)
			return true
		}
		b := make([]byte, tunHdr+whole)
		hdr6(b[tunHdr:], len(payload), nproto)
		copy(b[tunHdr+header.IPv6MinimumSize:], payload)
		emit(b)
		return true
	}
	if df && !fragmented {
		tooBig(mtu - nat64Overhead)
		return true
	}
	per := (mtu - header.IPv6MinimumSize - header.IPv6FragmentExtHdrLength) &^ 7
	id := uint32(ip.ID())
	for off := 0; off < len(payload) || (off == 0 && len(payload) == 0); off += per {
		n := min(per, len(payload)-off)
		last := off+n >= len(payload)
		b := make([]byte, tunHdr+header.IPv6MinimumSize+header.IPv6FragmentExtHdrLength+n)
		hdr6(b[tunHdr:], header.IPv6FragmentExtHdrLength+n, uint8(header.IPv6FragmentExtHdrIdentifier))
		fh := b[tunHdr+header.IPv6MinimumSize:]
		fh[0] = nproto
		fo := (uint16(offset)*8 + uint16(off)) &^ 7
		if !last || more {
			fo |= 1
		}
		binary.BigEndian.PutUint16(fh[2:], fo)
		binary.BigEndian.PutUint32(fh[4:], id)
		copy(fh[header.IPv6FragmentExtHdrLength:], payload[off:off+n])
		emit(b)
		if n == 0 {
			break
		}
	}
	return true
}

func icmp4to6(m []byte, quoteDst func(netip.Addr) (netip.Addr, bool), pref netip.Prefix) ([]byte, bool) {
	if len(m) < header.ICMPv4MinimumSize {
		return nil, false
	}
	typ, code := header.ICMPv4Type(m[0]), m[1]
	var t6, c6 uint8
	var mtu uint32
	switch typ {
	case header.ICMPv4Echo, header.ICMPv4EchoReply:
		out := append([]byte(nil), m...)
		out[0] = uint8(header.ICMPv6EchoRequest)
		if typ == header.ICMPv4EchoReply {
			out[0] = uint8(header.ICMPv6EchoReply)
		}
		return out, true
	case header.ICMPv4DstUnreachable:
		t6 = uint8(header.ICMPv6DstUnreachable)
		switch code {
		case 0, 1, 5, 6, 7, 8, 11, 12:
			c6 = 0
		case 3:
			c6 = 4
		case 9, 10, 13, 15:
			c6 = 1
		case 4:
			t6, c6 = uint8(header.ICMPv6PacketTooBig), 0
			mtu = uint32(binary.BigEndian.Uint16(m[6:])) + nat64Overhead
			if mtu < header.IPv6MinimumMTU {
				mtu = header.IPv6MinimumMTU
			}
		default:
			return nil, false
		}
	case header.ICMPv4TimeExceeded:
		t6, c6 = uint8(header.ICMPv6TimeExceeded), code
	default:
		return nil, false
	}
	inner, ok := quote4to6(m[header.ICMPv4MinimumSize:], quoteDst, pref)
	if !ok {
		return nil, false
	}
	out := make([]byte, header.ICMPv6MinimumSize+len(inner))
	out[0], out[1] = t6, c6
	binary.BigEndian.PutUint32(out[4:], mtu)
	copy(out[header.ICMPv6MinimumSize:], inner)
	return out, true
}

func quote4to6(q []byte, quoteDst func(netip.Addr) (netip.Addr, bool), pref netip.Prefix) ([]byte, bool) {
	if len(q) < header.IPv4MinimumSize {
		return nil, false
	}
	ihl := int(q[0]&0x0f) * 4
	if q[0]>>4 != 4 || ihl < header.IPv4MinimumSize || ihl > len(q) {
		return nil, false
	}
	src6, ok := quoteDst(netip.AddrFrom4([4]byte(q[12:16])))
	if !ok {
		return nil, false
	}
	dst6 := netplan.NAT64Address(pref, netip.AddrFrom4([4]byte(q[16:20])))
	proto := q[9]
	if proto == uint8(header.ICMPv4ProtocolNumber) {
		proto = uint8(header.ICMPv6ProtocolNumber)
	}
	body := q[ihl:]
	out := make([]byte, header.IPv6MinimumSize+len(body))
	copy(out[header.IPv6MinimumSize:], body)
	plen := int(binary.BigEndian.Uint16(q[2:])) - ihl
	if plen < 0 {
		plen = 0
	}
	header.IPv6(out).Encode(&header.IPv6Fields{
		TrafficClass: q[1], PayloadLength: uint16(plen), TransportProtocol: tcpip.TransportProtocolNumber(proto),
		HopLimit: q[8], SrcAddr: tcpip.AddrFrom16(src6.As16()), DstAddr: tcpip.AddrFrom16(dst6.As16()),
	})
	if at := l4ChecksumAt(proto, body); at >= 0 && at+2 <= len(body) {
		s, d := src6.As16(), dst6.As16()
		csumAdjust(out[header.IPv6MinimumSize+at:], csumWords(q[12:16], q[16:20]), csumWords(s[:], d[:]))
	}
	return out, true
}

func (t *Tunnel) nat64Quoted(a netip.Addr) (netip.Addr, bool) {
	p := t.nat64.Load()
	if p == nil {
		return netip.Addr{}, false
	}
	for _, pref := range *p {
		if v4, ok := netplan.NAT64V4(pref, a); ok {
			return v4, true
		}
	}
	return netip.Addr{}, false
}

func (t *Tunnel) macOf46(a netip.Addr) (netip.Addr, bool) {
	if a != t.cfg.Mac46 {
		return netip.Addr{}, false
	}
	return t.cfg.Mac6, true
}

func (t *Tunnel) toMac46(vhdr [netHdrLen]byte, frame, pkt []byte, ihl int, proto uint8, total int) {
	pref, ok := t.nat64Out()
	if !ok {
		t.n.malformed.Add(1)
		return
	}
	mtu4 := t.cfg.MTU - nat64Overhead
	emit := func(b []byte) {
		t.n.translated.Add(1)
		t.writeMac(afInet6, b)
	}
	tooBig := func(mtu int) { t.fragmentationNeeded(pkt, mtu) }
	xlate := func(buf []byte, room int) {
		if !to6(buf, room, pref, t.cfg.Mac6, t.cfg.MTU, t.macOf46, emit, tooBig) {
			t.n.malformed.Add(1)
		}
	}
	thl := tcpHeaderLen(pkt, ihl, proto)
	if thl > 0 && pkt[ihl+13]&uint8(header.TCPFlagSyn) != 0 {
		t.raiseMSS(pkt, true, ihl, thl, vhdr[0]&hdrNeedsCsum != 0, mtu4)
	}
	gsoType := vhdr[1] &^ hdrGSOECN
	if gsoType == hdrGSOTCPv4 {
		if thl == 0 {
			t.n.malformed.Add(1)
			return
		}
		mss := mtu4 - ihl - thl
		if g := int(binary.LittleEndian.Uint16(vhdr[4:])); t.cfg.MTU <= ipMTU && g > 0 && g < mss {
			mss = g
		}
		if total <= mtu4 {
			fillChecksums(pkt, true, ihl)
			xlate(frame, header.EthernetMinimumSize)
			return
		}
		room := tunHdr + nat64Overhead
		if !segmentTCP(pkt, mss, room, func(seg []byte) { xlate(seg, room) }) {
			t.n.malformed.Add(1)
		}
		return
	}
	if gsoType != 0 {
		t.n.malformed.Add(1)
		return
	}
	if vhdr[0]&hdrNeedsCsum != 0 {
		start := int(binary.LittleEndian.Uint16(vhdr[6:])) - header.EthernetMinimumSize
		offset := int(binary.LittleEndian.Uint16(vhdr[8:]))
		if !completeChecksum(pkt, start, offset) {
			t.n.malformed.Add(1)
			return
		}
		if proto == uint8(header.UDPProtocolNumber) && start == ihl && binary.BigEndian.Uint16(pkt[start+offset:]) == 0 {
			binary.BigEndian.PutUint16(pkt[start+offset:], 0xffff)
		}
	}
	xlate(frame, header.EthernetMinimumSize)
}

func (t *Tunnel) fragmentationNeeded(pkt []byte, mtu int) {
	s := t.stack.Load()
	if s == nil || len(pkt) < header.IPv4MinimumSize {
		return
	}
	ihl := int(pkt[0]&0x0f) * 4
	quote := pkt[:min(len(pkt), ihl+8)]
	msg := make([]byte, header.ICMPv4MinimumSize+len(quote))
	icmp := header.ICMPv4(msg)
	icmp.SetType(header.ICMPv4DstUnreachable)
	icmp.SetCode(header.ICMPv4FragmentationNeeded)
	icmp.SetMTU(uint16(mtu))
	copy(msg[header.ICMPv4MinimumSize:], quote)
	icmp.SetChecksum(^checksum.Checksum(msg, 0))
	ip := ipv4Packet(t.cfg.Mac46, netip.AddrFrom4([4]byte(pkt[12:16])), msg)
	frame := make([]byte, header.EthernetMinimumSize+len(ip))
	copy(frame[header.EthernetMinimumSize:], ip)
	mac := t.guestMAC.Load()
	header.Ethernet(frame).Encode(&header.EthernetFields{SrcAddr: GatewayMAC, DstAddr: tcpip.LinkAddress(mac[:]), Type: header.IPv4ProtocolNumber})
	var hdr [netHdrLen]byte
	var b batch
	t.toGuest(&b, s, &hdr, frame)
	b.flush()
}
