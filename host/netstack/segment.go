package netstack

import (
	"encoding/binary"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

const (
	tcpFIN = 0x01
	tcpPSH = 0x08
	tcpCWR = 0x80
)

func ipHeaders(pkt []byte) (v4 bool, hl int, proto uint8, total int, ok bool) {
	if len(pkt) < 1 {
		return
	}
	switch pkt[0] >> 4 {
	case 4:
		if len(pkt) < header.IPv4MinimumSize {
			return
		}
		hl = int(pkt[0]&0x0f) * 4
		total = int(binary.BigEndian.Uint16(pkt[2:]))
		if hl < header.IPv4MinimumSize || total < hl || total > len(pkt) {
			return
		}
		return true, hl, pkt[9], total, true
	case 6:
		if len(pkt) < header.IPv6MinimumSize {
			return
		}
		total = header.IPv6MinimumSize + int(binary.BigEndian.Uint16(pkt[4:]))
		if total > len(pkt) {
			return
		}
		return false, header.IPv6MinimumSize, pkt[6], total, true
	}
	return
}

func completeChecksum(pkt []byte, start, offset int) bool {
	if start < 0 || offset < 0 || start+offset+2 > len(pkt) {
		return false
	}
	sum := ^checksum.Checksum(pkt[start:], 0)
	binary.BigEndian.PutUint16(pkt[start+offset:], sum)
	return true
}

func segmentTCP(pkt []byte, mss, headroom int, emit func([]byte)) bool {
	v4, ihl, proto, total, ok := ipHeaders(pkt)
	if !ok || proto != uint8(header.TCPProtocolNumber) || len(pkt) < ihl+header.TCPMinimumSize {
		return false
	}
	pkt = pkt[:total]
	thl := int(pkt[ihl+12]>>4) * 4
	if thl < header.TCPMinimumSize || ihl+thl > total || mss <= 0 {
		return false
	}
	hdrs := ihl + thl
	data := pkt[hdrs:]
	seq := binary.BigEndian.Uint32(pkt[ihl+4:])
	var id uint16
	if v4 {
		id = binary.BigEndian.Uint16(pkt[4:])
	}
	flags := pkt[ihl+13]
	buf := make([]byte, headroom+hdrs+min(mss, len(data)))
	for off, i := 0, 0; off < len(data) || (off == 0 && len(data) == 0); i++ {
		n := min(mss, len(data)-off)
		seg := buf[:headroom+hdrs+n]
		p := seg[headroom:]
		copy(p, pkt[:hdrs])
		copy(p[hdrs:], data[off:off+n])
		f := flags
		if off+n < len(data) {
			f &^= tcpFIN | tcpPSH
		}
		if i > 0 {
			f &^= tcpCWR
		}
		p[ihl+13] = f
		binary.BigEndian.PutUint32(p[ihl+4:], seq+uint32(off))
		if v4 {
			binary.BigEndian.PutUint16(p[4:], id+uint16(i))
		}
		fillChecksums(p, v4, ihl)
		emit(seg)
		off += n
		if n == 0 {
			break
		}
	}
	return true
}

func fillChecksums(p []byte, v4 bool, ihl int) {
	var src, dst tcpip.Address
	proto := p[9]
	if v4 {
		ip := header.IPv4(p)
		ip.SetTotalLength(uint16(len(p)))
		ip.SetChecksum(0)
		ip.SetChecksum(^ip.CalculateChecksum())
		src, dst = ip.SourceAddress(), ip.DestinationAddress()
	} else {
		ip := header.IPv6(p)
		ip.SetPayloadLength(uint16(len(p) - header.IPv6MinimumSize))
		src, dst = ip.SourceAddress(), ip.DestinationAddress()
		proto = p[6]
	}
	l4 := p[ihl:]
	var at int
	switch proto {
	case uint8(header.TCPProtocolNumber):
		if len(l4) < header.TCPMinimumSize {
			return
		}
		at = 16
	case uint8(header.UDPProtocolNumber):
		if len(l4) < header.UDPMinimumSize {
			return
		}
		at = 6
	default:
		return
	}
	binary.BigEndian.PutUint16(l4[at:], 0)
	sum := header.PseudoHeaderChecksum(tcpip.TransportProtocolNumber(proto), src, dst, uint16(len(l4)))
	c := ^checksum.Checksum(l4, sum)
	if c == 0 && proto == uint8(header.UDPProtocolNumber) {
		c = 0xffff
	}
	binary.BigEndian.PutUint16(l4[at:], c)
}
