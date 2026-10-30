package netstack

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

type TunnelConfig struct {
	Mac         netip.Addr
	Peer        netip.Addr
	Mac6, Peer6 netip.Addr
	Mac46       netip.Addr
	MTU         int
}

type TunnelStats struct {
	FromMac, ToGuest                 uint64
	FromGuest, ToMac                 uint64
	SpoofedFromMac, SpoofedFromGuest uint64
	GuestBusy                        uint64
	Unreachable                      uint64
	Malformed                        uint64
	WriteFailed                      uint64
	Translated                       uint64
}

type tunnelCounters struct {
	fromMac, toGuest, fromGuest, toMac, spoofedFromMac, spoofedFromGuest,
	guestBusy, unreachable, malformed, writeFailed, translated atomic.Uint64
}

type Tunnel struct {
	cfg  TunnelConfig
	file *os.File
	raw  syscall.RawConn
	log  *slog.Logger

	prefixes atomic.Pointer[[]netip.Prefix]
	nat64    atomic.Pointer[[]netip.Prefix]
	stack    atomic.Pointer[Stack]
	guestMAC atomic.Pointer[[6]byte]

	writeMu sync.Mutex

	n       tunnelCounters
	closing atomic.Bool
	done    chan struct{}
}

var vmmGuestMAC = [6]byte{0x02, 0x00, 0xbe, 0x27, 0x00, 0x02}

const (
	afInet  = 2
	afInet6 = 30
	tunHdr  = 4
)

func NewTunnel(fd int, cfg TunnelConfig, log *slog.Logger) (*Tunnel, error) {
	t, err := newTunnel(fd, cfg, log)
	if err == nil {
		go t.read()
	}
	return t, err
}

const (
	MinTunnelMTU = ipMTU
	MaxTunnelMTU = 65535
)

func newTunnel(fd int, cfg TunnelConfig, log *slog.Logger) (*Tunnel, error) {
	if !cfg.Mac.Is4() {
		syscall.Close(fd)
		return nil, errors.New("netstack: the tunnel needs the Mac's IPv4 address")
	}
	if cfg.MTU == 0 {
		cfg.MTU = ipMTU
	}
	if cfg.MTU < MinTunnelMTU || cfg.MTU > MaxTunnelMTU {
		syscall.Close(fd)
		return nil, fmt.Errorf("netstack: a tunnel MTU of %d is outside %d to %d", cfg.MTU, MinTunnelMTU, MaxTunnelMTU)
	}
	if log == nil {
		log = slog.Default()
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	syscall.CloseOnExec(fd)
	f := os.NewFile(uintptr(fd), "utun")
	raw, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, err
	}
	t := &Tunnel{cfg: cfg, file: f, raw: raw, log: log, done: make(chan struct{})}
	mac := vmmGuestMAC
	t.guestMAC.Store(&mac)
	empty := []netip.Prefix{}
	t.prefixes.Store(&empty)
	return t, nil
}

func (t *Tunnel) SetPrefixes(ps []netip.Prefix) {
	c := make([]netip.Prefix, 0, len(ps))
	for _, p := range ps {
		c = append(c, p.Masked())
	}
	t.prefixes.Store(&c)
}

func (t *Tunnel) Buffers() (rcv, snd int) {
	t.raw.Control(func(fd uintptr) {
		rcv, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
		snd, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF)
	})
	return rcv, snd
}

func (t *Tunnel) Prefixes() []netip.Prefix { return slices.Clone(*t.prefixes.Load()) }

func (t *Tunnel) routed(a netip.Addr) bool {
	for _, p := range *t.prefixes.Load() {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (s *Stack) forwardable(dst netip.Addr) bool {
	if t := s.tunnelRef.Load(); t != nil && t.routed(dst.Unmap()) {
		return false
	}
	return s.cfg.forwardable(dst)
}

func (t *Tunnel) uplink(a netip.Addr) bool {
	s := t.stack.Load()
	if s == nil {
		return false
	}
	c := s.cfg
	switch a {
	case c.Gateway, c.Host, c.Gateway6, c.Host6:
		return false
	}
	return (c.Subnet.IsValid() && c.Subnet.Contains(a)) || (c.Subnet6.IsValid() && c.Subnet6.Contains(a))
}

func (t *Tunnel) Attach(s *Stack) {
	if old := t.stack.Swap(s); old != nil && old != s {
		old.link.tunnel.CompareAndSwap(t, nil)
	}
	s.tunnelRef.Store(t)
	s.link.tunnel.Store(t)
}

func (t *Tunnel) detach(s *Stack) {
	t.stack.CompareAndSwap(s, nil)
	s.link.tunnel.CompareAndSwap(t, nil)
}

func (t *Tunnel) Stats() TunnelStats {
	return TunnelStats{
		FromMac: t.n.fromMac.Load(), ToGuest: t.n.toGuest.Load(),
		FromGuest: t.n.fromGuest.Load(), ToMac: t.n.toMac.Load(),
		SpoofedFromMac: t.n.spoofedFromMac.Load(), SpoofedFromGuest: t.n.spoofedFromGuest.Load(),
		GuestBusy: t.n.guestBusy.Load(), Unreachable: t.n.unreachable.Load(),
		Malformed: t.n.malformed.Load(), WriteFailed: t.n.writeFailed.Load(),
		Translated: t.n.translated.Load(),
	}
}

func (t *Tunnel) Close() error {
	t.closing.Store(true)
	err := t.file.Close()
	<-t.done
	if s := t.stack.Swap(nil); s != nil {
		s.link.tunnel.CompareAndSwap(t, nil)
	}
	return err
}

func (t *Tunnel) Done() <-chan struct{} { return t.done }

const (
	readBatch      = 32
	readBatchBytes = 256 << 10
)

type batch struct {
	owed          *linkEndpoint
	frames, bytes int
}

func (b *batch) put(l *linkEndpoint, wake bool, n int) {
	if wake {
		if b.owed != nil && b.owed != l {
			b.owed.wakePeer()
		}
		b.owed = l
	}
	if b.owed != nil {
		b.frames++
		b.bytes += n
	}
}

func (b *batch) full() bool { return b.frames >= readBatch || b.bytes >= readBatchBytes }

func (b *batch) flush() {
	if b.owed != nil {
		b.owed.wakePeer()
		b.owed = nil
	}
	b.frames, b.bytes = 0, 0
}

func (t *Tunnel) read() {
	defer close(t.done)
	buf := make([]byte, header.EthernetMinimumSize-tunHdr+tunHdr+65535)
	at := header.EthernetMinimumSize - tunHdr
	var b batch
	var rerr error
	err := t.raw.Read(func(fd uintptr) bool {
		for !t.closing.Load() {
			n, err := syscall.Read(int(fd), buf[at:])
			switch {
			case err == syscall.EINTR:
				continue
			case err == syscall.EAGAIN:
				b.flush()
				return false
			case err != nil:
				rerr = err
				return true
			case n == 0:
				rerr = io.EOF
				return true
			}
			t.n.fromMac.Add(1)
			if n < tunHdr {
				t.n.malformed.Add(1)
				continue
			}
			t.fromMac(&b, buf[:at+n], buf[at+tunHdr:at+n], binary.BigEndian.Uint32(buf[at:]))
			if b.full() {
				b.flush()
			}
		}
		return true
	})
	b.flush()
	if err == nil {
		err = rerr
	}
	if err != nil && !t.closing.Load() {
		t.log.Warn("tunnel: read failed", "err", err)
	}
}

func (t *Tunnel) fromMac(b *batch, frame, pkt []byte, family uint32) {
	v4, _, _, total, ok := ipHeaders(pkt)
	if !ok || (v4 && family != afInet) || (!v4 && family != afInet6) {
		t.n.malformed.Add(1)
		return
	}
	pkt = pkt[:total]
	frame = frame[:header.EthernetMinimumSize+total]
	var src, dst netip.Addr
	if v4 {
		src, dst = netip.AddrFrom4([4]byte(pkt[12:16])), netip.AddrFrom4([4]byte(pkt[16:20]))
	} else {
		src, dst = netip.AddrFrom16([16]byte(pkt[8:24])), netip.AddrFrom16([16]byte(pkt[24:40]))
	}
	want := t.cfg.Mac
	if !v4 {
		want = t.cfg.Mac6
	}
	if !v4 {
		if dst4, ok := t.nat64Dst(dst); ok {
			if src != want || !t.routed(dst4) {
				t.n.spoofedFromMac.Add(1)
				return
			}
			s := t.stack.Load()
			if s == nil {
				t.unreachable(pkt, false)
				return
			}
			room := header.EthernetMinimumSize
			out, ok := to4(frame[:room+total], room, t.cfg.Mac46, dst4, t.nat64Quoted, t.cfg.Mac46)
			if !ok {
				t.n.malformed.Add(1)
				return
			}
			t.n.translated.Add(1)
			t.toGuestPacket(b, s, out, out[room:], true)
			return
		}
	}
	if !want.IsValid() || src != want || !t.routed(dst) {
		t.n.spoofedFromMac.Add(1)
		return
	}
	s := t.stack.Load()
	if s == nil {
		t.unreachable(pkt, v4)
		return
	}
	t.toGuestPacket(b, s, frame, pkt, v4)
}

func (t *Tunnel) toGuestPacket(b *batch, s *Stack, frame, pkt []byte, v4 bool) {
	_, ihl, proto, total, _ := ipHeaders(pkt)
	etherType := header.IPv4ProtocolNumber
	if !v4 {
		etherType = header.IPv6ProtocolNumber
	}
	mac := t.guestMAC.Load()
	eth := header.EthernetFields{SrcAddr: GatewayMAC, DstAddr: tcpip.LinkAddress(mac[:]), Type: etherType}
	var hdr [netHdrLen]byte
	if total > ipMTU {
		thl := tcpHeaderLen(pkt, ihl, proto)
		switch {
		case thl > 0 && s.link.guestTakesLarge(v4):
			largeSegmentHeader(&hdr, pkt, v4, ihl, thl)
		case thl > 0:
			if !segmentTCP(pkt, ipMTU-ihl-thl, header.EthernetMinimumSize, func(seg []byte) {
				header.Ethernet(seg).Encode(&eth)
				t.toGuest(b, s, &hdr, seg)
			}) {
				t.n.malformed.Add(1)
			}
			return
		case !s.link.guestTakesLarge(true):
			t.n.malformed.Add(1)
			return
		}
	}
	header.Ethernet(frame).Encode(&eth)
	t.toGuest(b, s, &hdr, frame)
}

func (t *Tunnel) toGuest(b *batch, s *Stack, hdr *[netHdrLen]byte, frame []byte) {
	ok, wake := s.link.tryWrite(hdr, frame)
	if !ok {
		t.n.guestBusy.Add(1)
		return
	}
	t.n.toGuest.Add(1)
	b.put(s.link, wake, len(frame))
}

func tcpHeaderLen(pkt []byte, ihl int, proto uint8) int {
	if proto != uint8(header.TCPProtocolNumber) || len(pkt) < ihl+header.TCPMinimumSize {
		return 0
	}
	thl := int(pkt[ihl+12]>>4) * 4
	if thl < header.TCPMinimumSize || ihl+thl > len(pkt) {
		return 0
	}
	return thl
}

func largeSegmentHeader(hdr *[netHdrLen]byte, pkt []byte, v4 bool, ihl, thl int) {
	var src, dst tcpip.Address
	gso := uint8(hdrGSOTCPv4)
	if v4 {
		ip := header.IPv4(pkt)
		src, dst = ip.SourceAddress(), ip.DestinationAddress()
	} else {
		ip := header.IPv6(pkt)
		src, dst = ip.SourceAddress(), ip.DestinationAddress()
		gso = hdrGSOTCPv6
	}
	l4 := pkt[ihl:]
	binary.BigEndian.PutUint16(l4[16:], header.PseudoHeaderChecksum(header.TCPProtocolNumber, src, dst, uint16(len(l4))))
	hdr[0] = hdrNeedsCsum
	hdr[1] = gso
	binary.LittleEndian.PutUint16(hdr[2:], uint16(header.EthernetMinimumSize+ihl+thl))
	binary.LittleEndian.PutUint16(hdr[4:], uint16(ipMTU-ihl-thl))
	binary.LittleEndian.PutUint16(hdr[6:], uint16(header.EthernetMinimumSize+ihl))
	binary.LittleEndian.PutUint16(hdr[8:], 16)
}

func (t *Tunnel) forMac(frame []byte) bool {
	if len(frame) < header.EthernetMinimumSize+header.IPv4MinimumSize {
		return false
	}
	switch binary.BigEndian.Uint16(frame[12:]) {
	case uint16(header.IPv4ProtocolNumber):
		if frame[14]>>4 != 4 {
			return false
		}
		d := netip.AddrFrom4([4]byte(frame[30:34]))
		return d == t.cfg.Mac || (d == t.cfg.Mac46 && t.cfg.Mac46.IsValid())
	case uint16(header.IPv6ProtocolNumber):
		return t.cfg.Mac6.IsValid() && len(frame) >= header.EthernetMinimumSize+header.IPv6MinimumSize &&
			netip.AddrFrom16([16]byte(frame[38:54])) == t.cfg.Mac6
	}
	return false
}

func (t *Tunnel) learnGuestMAC(frame []byte) {
	if len(frame) < header.EthernetMinimumSize {
		return
	}
	cur := t.guestMAC.Load()
	if bytes.Equal(cur[:], frame[6:12]) || frame[6]&1 != 0 {
		return
	}
	mac := [6]byte(frame[6:12])
	t.guestMAC.Store(&mac)
}

func (t *Tunnel) fromGuest(vhdr [netHdrLen]byte, frame []byte) {
	t.n.fromGuest.Add(1)
	pkt := frame[header.EthernetMinimumSize:]
	v4, ihl, proto, total, ok := ipHeaders(pkt)
	if !ok {
		t.n.malformed.Add(1)
		return
	}
	pkt = pkt[:total]
	var src netip.Addr
	family := uint32(afInet)
	if v4 {
		src = netip.AddrFrom4([4]byte(pkt[12:16]))
	} else {
		src = netip.AddrFrom16([16]byte(pkt[8:24]))
		family = afInet6
	}
	if !t.routed(src) && !t.uplink(src) {
		t.n.spoofedFromGuest.Add(1)
		return
	}
	if v4 && t.cfg.Mac46.IsValid() && netip.AddrFrom4([4]byte(pkt[16:20])) == t.cfg.Mac46 {
		t.toMac46(vhdr, frame, pkt, ihl, proto, total)
		return
	}
	whole := frame[header.EthernetMinimumSize-tunHdr : header.EthernetMinimumSize+total]
	thl := tcpHeaderLen(pkt, ihl, proto)
	if thl > 0 && pkt[ihl+13]&uint8(header.TCPFlagSyn) != 0 {
		t.raiseMSS(pkt, v4, ihl, thl, vhdr[0]&hdrNeedsCsum != 0, t.cfg.MTU)
	}
	gsoType := vhdr[1] &^ hdrGSOECN
	if gsoType == hdrGSOTCPv4 || gsoType == hdrGSOTCPv6 {
		if thl == 0 {
			t.n.malformed.Add(1)
			return
		}
		if total <= t.cfg.MTU {
			fillChecksums(pkt, v4, ihl)
			t.writeMac(family, whole)
			return
		}
		mss := t.cfg.MTU - ihl - thl
		if g := int(binary.LittleEndian.Uint16(vhdr[4:])); t.cfg.MTU <= ipMTU && g > 0 && g < mss {
			mss = g
		}
		if !segmentTCP(pkt, mss, tunHdr, func(seg []byte) { t.writeMac(family, seg) }) {
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
	if total > t.cfg.MTU {
		t.n.malformed.Add(1)
		return
	}
	t.writeMac(family, whole)
}

func (t *Tunnel) raiseMSS(pkt []byte, v4 bool, ihl, thl int, partial bool, mtu int) {
	most := mtu - ihl - header.TCPMinimumSize
	least := ipMTU - ihl - header.TCPMinimumSize
	opts := pkt[ihl+header.TCPMinimumSize : ihl+thl]
	for i := 0; i < len(opts); {
		switch kind := opts[i]; kind {
		case 0:
			return
		case 1:
			i++
			continue
		}
		if i+1 >= len(opts) || opts[i+1] < 2 || i+int(opts[i+1]) > len(opts) {
			return
		}
		if opts[i] == 2 && opts[i+1] == 4 {
			if mss := int(binary.BigEndian.Uint16(opts[i+2:])); mss >= most || mss < least {
				return
			}
			binary.BigEndian.PutUint16(opts[i+2:], uint16(most))
			if !partial {
				fillChecksums(pkt, v4, ihl)
			}
			return
		}
		i += int(opts[i+1])
	}
}

func (t *Tunnel) writeMac(family uint32, buf []byte) {
	if len(buf)-tunHdr > t.cfg.MTU {
		t.n.malformed.Add(1)
		return
	}
	binary.BigEndian.PutUint32(buf, family)
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	var werr error
	err := t.raw.Write(func(fd uintptr) bool {
		_, werr = syscall.Write(int(fd), buf)
		return true
	})
	if err == nil {
		err = werr
	}
	if err != nil {
		t.n.writeFailed.Add(1)
		return
	}
	t.n.toMac.Add(1)
}

func (t *Tunnel) unreachable(pkt []byte, v4 bool) {
	if !v4 {
		t.unreachable6(pkt)
		return
	}
	ihl := int(pkt[0]&0x0f) * 4
	if pkt[9] == uint8(header.ICMPv4ProtocolNumber) && len(pkt) > ihl {
		switch header.ICMPv4Type(pkt[ihl]) {
		case header.ICMPv4Echo, header.ICMPv4Timestamp, header.ICMPv4InfoRequest:
		default:
			return
		}
	}
	if !t.cfg.Peer.Is4() {
		return
	}
	quote := pkt[:min(len(pkt), ihl+8)]
	msg := make([]byte, header.ICMPv4MinimumSize+len(quote))
	icmp := header.ICMPv4(msg)
	icmp.SetType(header.ICMPv4DstUnreachable)
	icmp.SetCode(header.ICMPv4HostUnreachable)
	copy(msg[header.ICMPv4MinimumSize:], quote)
	icmp.SetChecksum(^checksum.Checksum(msg, 0))
	out := append(make([]byte, tunHdr), ipv4Packet(t.cfg.Peer, t.cfg.Mac, msg)...)
	t.writeMac(afInet, out)
	t.n.unreachable.Add(1)
}

func (t *Tunnel) unreachable6(pkt []byte) {
	if len(pkt) > header.IPv6MinimumSize && pkt[6] == uint8(header.ICMPv6ProtocolNumber) &&
		header.ICMPv6Type(pkt[header.IPv6MinimumSize]) != header.ICMPv6EchoRequest {
		return
	}
	if !t.cfg.Peer6.Is6() || !t.cfg.Mac6.Is6() {
		return
	}
	quote := pkt[:min(len(pkt), header.IPv6MinimumMTU-header.IPv6MinimumSize-header.ICMPv6MinimumSize)]
	msg := make([]byte, header.ICMPv6MinimumSize+len(quote))
	icmp := header.ICMPv6(msg)
	icmp.SetType(header.ICMPv6DstUnreachable)
	icmp.SetCode(header.ICMPv6AddressUnreachable)
	copy(msg[header.ICMPv6MinimumSize:], quote)
	src, dst := tcpip.AddrFrom16(t.cfg.Peer6.As16()), tcpip.AddrFrom16(t.cfg.Mac6.As16())
	icmp.SetChecksum(header.ICMPv6Checksum(header.ICMPv6ChecksumParams{Header: icmp, Src: src, Dst: dst}))
	out := make([]byte, tunHdr+header.IPv6MinimumSize+len(msg))
	header.IPv6(out[tunHdr:]).Encode(&header.IPv6Fields{
		PayloadLength: uint16(len(msg)), TransportProtocol: header.ICMPv6ProtocolNumber, HopLimit: 64, SrcAddr: src, DstAddr: dst,
	})
	copy(out[tunHdr+header.IPv6MinimumSize:], msg)
	t.writeMac(afInet6, out)
	t.n.unreachable.Add(1)
}

func (s TunnelStats) String() string {
	return fmt.Sprintf("from-mac=%d to-guest=%d from-guest=%d to-mac=%d spoofed-from-mac=%d spoofed-from-guest=%d guest-busy=%d unreachable=%d malformed=%d write-failed=%d translated=%d",
		s.FromMac, s.ToGuest, s.FromGuest, s.ToMac, s.SpoofedFromMac, s.SpoofedFromGuest, s.GuestBusy, s.Unreachable, s.Malformed, s.WriteFailed, s.Translated)
}
