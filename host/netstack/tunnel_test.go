package netstack

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	"fev/proto/netplan"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

var (
	plan          = netplan.Default()
	tunnelMac     = plan.TunnelMac()
	tunnelPeer    = plan.TunnelPeer()
	containerAddr = plan.Docker0().Addr()
)

func standInUtun(t testing.TB) (int, *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<20)
		syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 4<<20)
	}
	f := os.NewFile(uintptr(fds[1]), "mac")
	c, err := net.FileConn(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return fds[0], c.(*net.UnixConn)
}

func macWrite(t testing.TB, c *net.UnixConn, pkt []byte) {
	t.Helper()
	family := uint32(afInet)
	if pkt[0]>>4 == 6 {
		family = afInet6
	}
	if _, err := c.Write(append(binary.BigEndian.AppendUint32(nil, family), pkt...)); err != nil {
		t.Fatal(err)
	}
}

func macRead(t testing.TB, c *net.UnixConn, wait time.Duration) ([]byte, bool) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 70000)
	n, err := c.Read(buf)
	if err != nil {
		return nil, false
	}
	if n < tunHdr || binary.BigEndian.Uint32(buf) != afInet && binary.BigEndian.Uint32(buf) != afInet6 {
		t.Fatalf("the tunnel wrote %d bytes without a family header: % x", n, buf[:min(n, 8)])
	}
	return buf[tunHdr:n], true
}

type tunnelWorld struct {
	*world
	tun *Tunnel
	mac *net.UnixConn
}

func newTunnelWorld(t *testing.T, offload bool) *tunnelWorld {
	t.Helper()
	hostEnd, guestEnd := socketPair(t)
	if offload {
		announce(t, guestEnd, acceptsPartialCsum|acceptsTSO4)
	}
	h := newFakeHost()
	ns, err := New(hostEnd, Options{Host: h})
	if err != nil {
		t.Fatal(err)
	}
	g := newGuestStack(t, guestEnd, offload)
	addr := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: toAddr(containerAddr), PrefixLen: 24}}
	if err := g.AddProtocolAddress(1, addr, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	fd, mac := standInUtun(t)
	tun, err := NewTunnel(fd, TunnelConfig{Mac: tunnelMac, Peer: tunnelPeer}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tun.SetPrefixes(plan.Routed())
	tun.Attach(ns)
	waitLinkReady(t, ns)
	t.Cleanup(func() {
		tun.Close()
		ns.Close()
		g.Close()
	})
	return &tunnelWorld{world: &world{host: h, ns: ns, guest: g}, tun: tun, mac: mac}
}

func waitLinkReady(t testing.TB, ns *Stack) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ns.link.ringMu.Lock()
		ready := ns.link.region != nil
		ns.link.ringMu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the stack never took the VMM's memory")
		}
		time.Sleep(time.Millisecond)
	}
}

func macStack(t *testing.T, c *net.UnixConn) *stack.Stack {
	t.Helper()
	return macStackOf(t, c, ipMTU)
}

func macStackOf(t *testing.T, c *net.UnixConn, mtu uint32) *stack.Stack {
	t.Helper()
	ep := channel.New(512, mtu, "")
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	if err := s.CreateNIC(1, ep); err != nil {
		t.Fatal(err)
	}
	a := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: toAddr(tunnelMac), PrefixLen: 32}}
	if err := s.AddProtocolAddress(1, a, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	base, _ := tcpip.NewSubnet(toAddr(plan.Base().Addr()), tcpip.MaskFromBytes([]byte{255, 255, 0, 0}))
	s.SetRouteTable([]tcpip.Route{{Destination: base, NIC: 1}})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			pkt := ep.ReadContext(ctx)
			if pkt == nil {
				return
			}
			b := pkt.ToBuffer()
			data := b.Flatten()
			b.Release()
			pkt.DecRef()
			c.Write(append(binary.BigEndian.AppendUint32(nil, afInet), data...))
		}
	}()
	go func() {
		buf := make([]byte, 70000)
		for {
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			if n <= tunHdr {
				continue
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), buf[tunHdr:n]...))})
			ep.InjectInbound(ipv4.ProtocolNumber, pkt)
			pkt.DecRef()
		}
	}()
	t.Cleanup(func() {
		cancel()
		s.Close()
	})
	return s
}

func TestTheMacReachesAGuestAddressThroughTheTunnelByTCP(t *testing.T) {
	for _, offload := range []bool{false, true} {
		name := "no offload"
		if offload {
			name = "the guest sends large segments with unfinished checksums"
		}
		t.Run(name, func(t *testing.T) {
			w := newTunnelWorld(t, offload)
			mac := macStack(t, w.mac)
			l, err := gonet.ListenTCP(w.guest, tcpip.FullAddress{NIC: 1, Addr: toAddr(containerAddr), Port: 8080}, ipv4.ProtocolNumber)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			go func() {
				c, err := l.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				io.Copy(c, c)
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c, err := gonet.DialContextTCP(ctx, mac, tcpip.FullAddress{NIC: 1, Addr: toAddr(containerAddr), Port: 8080}, ipv4.ProtocolNumber)
			if err != nil {
				t.Fatalf("dial %v:8080 from the Mac: %v (%v)", containerAddr, err, w.tun.Stats())
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(20 * time.Second))
			payload := make([]byte, 1<<20)
			for i := range payload {
				payload[i] = byte(i*7 + i>>11)
			}
			go func() {
				c.Write(payload)
				c.CloseWrite()
			}()
			got, err := io.ReadAll(c)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("echo: %d bytes of %d, %v (%v)", len(got), len(payload), err, w.tun.Stats())
			}
			s := w.tun.Stats()
			if s.SpoofedFromMac+s.SpoofedFromGuest+s.Malformed+s.WriteFailed != 0 || s.ToGuest == 0 || s.ToMac == 0 {
				t.Fatalf("stats %v", s)
			}
			if offload && s.ToMac <= s.FromGuest {
				t.Fatalf("with offload no large segment was cut: %v", s)
			}
			if len(w.host.tcpDials) != 0 {
				t.Fatalf("the stack dialled %v", w.host.tcpDials)
			}
		})
	}
}

func TestTheMacReachesAGuestAddressThroughTheTunnelByUDP(t *testing.T) {
	w := newTunnelWorld(t, true)
	mac := macStack(t, w.mac)
	pc, err := gonet.DialUDP(w.guest, &tcpip.FullAddress{NIC: 1, Addr: toAddr(containerAddr), Port: 5353}, nil, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(append([]byte("echo "), buf[:n]...), from)
		}
	}()
	c, err := gonet.DialUDP(mac, nil, &tcpip.FullAddress{NIC: 1, Addr: toAddr(containerAddr), Port: 5353}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "echo hello" {
		t.Fatalf("read %q, %v (%v)", buf[:n], err, w.tun.Stats())
	}
}

func echoRequest(src, dst netip.Addr, ident, seq uint16) []byte {
	msg := make([]byte, header.ICMPv4MinimumSize+4)
	m := header.ICMPv4(msg)
	m.SetType(header.ICMPv4Echo)
	m.SetIdent(ident)
	m.SetSequence(seq)
	copy(msg[header.ICMPv4MinimumSize:], "ping")
	m.SetChecksum(^checksum.Checksum(msg, 0))
	return ipv4Packet(src, dst, msg)
}

func TestAPingFromTheMacIsAnsweredByTheGuestAddress(t *testing.T) {
	w := newTunnelWorld(t, false)
	macWrite(t, w.mac, echoRequest(tunnelMac, containerAddr, 77, 1))
	for {
		pkt, ok := macRead(t, w.mac, 5*time.Second)
		if !ok {
			t.Fatalf("no echo reply (%v)", w.tun.Stats())
		}
		ip := header.IPv4(pkt)
		if ip.Protocol() != uint8(header.ICMPv4ProtocolNumber) {
			continue
		}
		m := header.ICMPv4(ip.Payload())
		if m.Type() != header.ICMPv4EchoReply || m.Ident() != 77 || fromAddr(ip.SourceAddress()) != containerAddr || fromAddr(ip.DestinationAddress()) != tunnelMac {
			t.Fatalf("got type %d ident %d from %v to %v", m.Type(), m.Ident(), fromAddr(ip.SourceAddress()), fromAddr(ip.DestinationAddress()))
		}
		if !ip.IsChecksumValid() || checksum.Checksum(ip.Payload(), 0) != 0xffff {
			t.Fatal("the reply's checksums are wrong")
		}
		return
	}
}

func TestAPacketFromTheMacWithAnotherSourceOrForAnotherPrefixIsDropped(t *testing.T) {
	w := newTunnelWorld(t, false)
	macWrite(t, w.mac, echoRequest(netip.MustParseAddr("192.168.1.50"), containerAddr, 1, 1))
	macWrite(t, w.mac, echoRequest(tunnelMac, netip.MustParseAddr("198.19.1.2"), 1, 2))
	macWrite(t, w.mac, echoRequest(tunnelMac, netip.MustParseAddr("8.8.8.8"), 1, 3))
	deadline := time.Now().Add(5 * time.Second)
	for w.tun.Stats().SpoofedFromMac < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s := w.tun.Stats(); s.SpoofedFromMac != 3 || s.ToGuest != 0 {
		t.Fatalf("stats %v", s)
	}
	if pkt, ok := macRead(t, w.mac, 200*time.Millisecond); ok {
		t.Fatalf("something came back: % x", pkt[:20])
	}
	w.tun.SetPrefixes([]netip.Prefix{plan.Machines()})
	macWrite(t, w.mac, echoRequest(tunnelMac, containerAddr, 1, 4))
	for w.tun.Stats().SpoofedFromMac < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s := w.tun.Stats(); s.SpoofedFromMac != 4 {
		t.Fatalf("after the prefix went: %v", s)
	}
}

func tcpFrame(src, dst netip.AddrPort, seq uint32, flags header.TCPFlags, data []byte) []byte {
	tcpLen := header.TCPMinimumSize + len(data)
	frame := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+tcpLen)
	header.Ethernet(frame).Encode(&header.EthernetFields{SrcAddr: guestMAC, DstAddr: GatewayMAC, Type: ipv4.ProtocolNumber})
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{TotalLength: uint16(header.IPv4MinimumSize + tcpLen), TTL: 64, ID: 0x100,
		Protocol: uint8(header.TCPProtocolNumber), SrcAddr: toAddr(src.Addr()), DstAddr: toAddr(dst.Addr())})
	ip.SetChecksum(^ip.CalculateChecksum())
	tc := header.TCP(ip.Payload())
	tc.Encode(&header.TCPFields{SrcPort: src.Port(), DstPort: dst.Port(), SeqNum: seq, AckNum: 1, DataOffset: header.TCPMinimumSize, Flags: flags, WindowSize: 65535})
	copy(tc[header.TCPMinimumSize:], data)
	return frame
}

func validTCP(pkt []byte) bool {
	ip := header.IPv4(pkt)
	if !ip.IsChecksumValid() {
		return false
	}
	l4 := ip.Payload()
	sum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(l4)))
	return checksum.Checksum(l4, sum) == 0xffff
}

func TestTheGuestsUnfinishedChecksumsAndLargeSegmentsReachTheMacWhole(t *testing.T) {
	hostEnd, guestEnd := socketPair(t)
	ns, err := New(hostEnd, Options{Host: newFakeHost()})
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	fd, mac := standInUtun(t)
	tun, err := NewTunnel(fd, TunnelConfig{Mac: tunnelMac, Peer: tunnelPeer}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	tun.SetPrefixes(plan.Routed())
	tun.Attach(ns)
	src := netip.AddrPortFrom(netip.MustParseAddr("198.19.64.5"), 80)
	dst := netip.AddrPortFrom(tunnelMac, 51000)

	frame := tcpFrame(src, dst, 1000, header.TCPFlagAck|header.TCPFlagPsh, []byte("hello"))
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	tc := header.TCP(ip.Payload())
	tc.SetChecksum(0)
	tc.SetChecksum(header.PseudoHeaderChecksum(header.TCPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(tc))))
	var vhdr [netHdrLen]byte
	vhdr[0] = hdrNeedsCsum
	binary.LittleEndian.PutUint16(vhdr[6:], uint16(header.EthernetMinimumSize+header.IPv4MinimumSize))
	binary.LittleEndian.PutUint16(vhdr[8:], 16)
	guestEnd.send(append(vhdr[:], frame...))
	pkt, ok := macRead(t, mac, 5*time.Second)
	if !ok || !validTCP(pkt) || string(header.TCP(header.IPv4(pkt).Payload()).Payload()) != "hello" {
		t.Fatalf("small segment: %v, valid %v (%v)", ok, ok && validTCP(pkt), tun.Stats())
	}

	data := make([]byte, 65000)
	for i := range data {
		data[i] = byte(i)
	}
	frame = tcpFrame(src, dst, 5000, header.TCPFlagAck|header.TCPFlagPsh|header.TCPFlagFin|header.TCPFlagCwr, data)
	vhdr = [netHdrLen]byte{hdrNeedsCsum, hdrGSOTCPv4}
	binary.LittleEndian.PutUint16(vhdr[4:], 1448)
	binary.LittleEndian.PutUint16(vhdr[6:], uint16(header.EthernetMinimumSize+header.IPv4MinimumSize))
	binary.LittleEndian.PutUint16(vhdr[8:], 16)
	guestEnd.send(append(vhdr[:], frame...))
	var got []byte
	want := (len(data) + 1447) / 1448
	for i := 0; i < want; i++ {
		pkt, ok := macRead(t, mac, 5*time.Second)
		if !ok {
			t.Fatalf("segment %d of %d missing (%v)", i, want, tun.Stats())
		}
		if len(pkt) > ipMTU || !validTCP(pkt) {
			t.Fatalf("segment %d: %d bytes, checksums valid %v", i, len(pkt), validTCP(pkt))
		}
		ip := header.IPv4(pkt)
		tc := header.TCP(ip.Payload())
		if tc.SequenceNumber() != 5000+uint32(len(got)) || ip.ID() != 0x100+uint16(i) {
			t.Fatalf("segment %d: seq %d id %#x", i, tc.SequenceNumber(), ip.ID())
		}
		last := i == want-1
		if (tc.Flags()&header.TCPFlagFin != 0) != last || (tc.Flags()&header.TCPFlagPsh != 0) != last || (tc.Flags()&header.TCPFlagCwr != 0) != (i == 0) {
			t.Fatalf("segment %d flags %v", i, tc.Flags())
		}
		got = append(got, tc.Payload()...)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("reassembled %d bytes, not the %d sent", len(got), len(data))
	}
}

func TestAGuestPacketWithASourceFevomDoesNotRouteNeverReachesTheMac(t *testing.T) {
	hostEnd, guestEnd := socketPair(t)
	ns, err := New(hostEnd, Options{Host: newFakeHost()})
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	fd, mac := standInUtun(t)
	tun, err := NewTunnel(fd, TunnelConfig{Mac: tunnelMac, Peer: tunnelPeer}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	tun.SetPrefixes(plan.Routed())
	tun.Attach(ns)
	dst := netip.AddrPortFrom(tunnelMac, 22)
	for _, src := range []string{"192.168.1.10", "8.8.8.8", "198.19.1.1", "198.19.3.1"} {
		guestEnd.sendFrame(tcpFrame(netip.AddrPortFrom(netip.MustParseAddr(src), 4000), dst, 1, header.TCPFlagSyn, nil))
	}
	guestEnd.sendFrame(tcpFrame(netip.AddrPortFrom(netip.MustParseAddr("198.19.0.7"), 4000), dst, 1, header.TCPFlagSyn, nil))
	pkt, ok := macRead(t, mac, 5*time.Second)
	if !ok || fromAddr(header.IPv4(pkt).SourceAddress()) != netip.MustParseAddr("198.19.0.7") {
		t.Fatalf("the machine's packet: %v (%v)", ok, tun.Stats())
	}
	syn := tcpFrame(netip.AddrPortFrom(tunnelMac, 50000), netip.AddrPortFrom(netip.MustParseAddr("198.19.64.9"), 80), 1, header.TCPFlagSyn, nil)[header.EthernetMinimumSize:]
	icmpMsg := append([]byte{byte(header.ICMPv4DstUnreachable), byte(header.ICMPv4HostUnreachable), 0, 0, 0, 0, 0, 0}, syn[:header.IPv4MinimumSize+8]...)
	header.ICMPv4(icmpMsg).SetChecksum(^checksum.Checksum(icmpMsg, 0))
	errPkt := ipv4Packet(guestAddr, tunnelMac, icmpMsg)
	errFrame := make([]byte, header.EthernetMinimumSize, header.EthernetMinimumSize+len(errPkt))
	header.Ethernet(errFrame).Encode(&header.EthernetFields{SrcAddr: guestMAC, DstAddr: GatewayMAC, Type: ipv4.ProtocolNumber})
	guestEnd.sendFrame(append(errFrame, errPkt...))
	pkt, ok = macRead(t, mac, 5*time.Second)
	if !ok || fromAddr(header.IPv4(pkt).SourceAddress()) != guestAddr || header.ICMPv4(header.IPv4(pkt).Payload()).Type() != header.ICMPv4DstUnreachable {
		t.Fatalf("the guest's own ICMP error did not reach the Mac (%v)", tun.Stats())
	}
	if s := tun.Stats(); s.SpoofedFromGuest != 4 || s.ToMac != 2 {
		t.Fatalf("stats %v", s)
	}
	if _, ok := macRead(t, mac, 100*time.Millisecond); ok {
		t.Fatal("a spoofed packet reached the Mac")
	}
}

func TestAFullGuestRingDropsAndNeverStallsTheReader(t *testing.T) {
	hostEnd, _ := socketPair(t)
	ns, err := New(hostEnd, Options{Host: newFakeHost()})
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	fd, mac := standInUtun(t)
	tun, err := NewTunnel(fd, TunnelConfig{Mac: tunnelMac, Peer: tunnelPeer}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	tun.SetPrefixes(plan.Routed())
	tun.Attach(ns)
	waitLinkReady(t, ns)
	const n = 3000
	pkt := echoRequest(tunnelMac, containerAddr, 9, 0)
	pkt = append(pkt, make([]byte, 900)...)
	header.IPv4(pkt).SetTotalLength(uint16(len(pkt)))
	for i := 0; i < n; i++ {
		macWrite(t, mac, pkt)
	}
	deadline := time.Now().Add(10 * time.Second)
	for s := tun.Stats(); (s.FromMac < n || s.ToGuest+s.GuestBusy < s.FromMac) && time.Now().Before(deadline); s = tun.Stats() {
		time.Sleep(10 * time.Millisecond)
	}
	s := tun.Stats()
	if s.FromMac != n || s.GuestBusy == 0 || s.ToGuest+s.GuestBusy != n {
		t.Fatalf("stats %v: the reader stalled or nothing was dropped", s)
	}
}

func TestWithNoGuestTheMacIsToldTheHostIsUnreachable(t *testing.T) {
	fd, mac := standInUtun(t)
	tun, err := NewTunnel(fd, TunnelConfig{Mac: tunnelMac, Peer: tunnelPeer}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	tun.SetPrefixes(plan.Routed())
	syn := tcpFrame(netip.AddrPortFrom(tunnelMac, 50123), netip.AddrPortFrom(containerAddr, 80), 7, header.TCPFlagSyn, nil)[header.EthernetMinimumSize:]
	macWrite(t, mac, syn)
	pkt, ok := macRead(t, mac, 5*time.Second)
	if !ok {
		t.Fatal("no answer")
	}
	ip := header.IPv4(pkt)
	m := header.ICMPv4(ip.Payload())
	if fromAddr(ip.SourceAddress()) != tunnelPeer || fromAddr(ip.DestinationAddress()) != tunnelMac ||
		m.Type() != header.ICMPv4DstUnreachable || m.Code() != header.ICMPv4HostUnreachable || !ip.IsChecksumValid() || checksum.Checksum(ip.Payload(), 0) != 0xffff {
		t.Fatalf("answer from %v to %v type %d code %d", fromAddr(ip.SourceAddress()), fromAddr(ip.DestinationAddress()), m.Type(), m.Code())
	}
	if q := m.Payload(); !bytes.Equal(q, syn[:header.IPv4MinimumSize+8]) {
		t.Fatal("the error does not quote the Mac's packet")
	}
	macWrite(t, mac, ipv4Packet(tunnelMac, containerAddr, append([]byte{3, 3, 0, 0, 0, 0, 0, 0}, syn[:28]...)))
	if _, ok := macRead(t, mac, 200*time.Millisecond); ok {
		t.Fatal("an ICMP error was answered")
	}

	hostEnd, _ := socketPair(t)
	ns, err := New(hostEnd, Options{Host: newFakeHost()})
	if err != nil {
		t.Fatal(err)
	}
	tun.Attach(ns)
	ns.Close()
	macWrite(t, mac, syn)
	if _, ok := macRead(t, mac, 5*time.Second); !ok || tun.Stats().Unreachable != 2 {
		t.Fatalf("after the guest went: %v", tun.Stats())
	}
}

func TestTheStackNeverForwardsIntoTheTunnelsPrefixes(t *testing.T) {
	w := newTunnelWorld(t, false)
	if _, err := w.dialTCP(t, "198.19.64.9:80"); err == nil {
		t.Fatal("a connection to an unowned address in a routed prefix succeeded")
	}
	if len(w.host.tcpDials) != 0 {
		t.Fatalf("the Mac was asked to dial %v", w.host.tcpDials)
	}
	c, err := w.dialTCP(t, "93.184.215.14:80")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestAStackThatWasUpLeavesTheTunnelWhenItsLinkEnds(t *testing.T) {
	w := newTunnelWorld(t, false)
	mac := macStack(t, w.mac)
	l, err := gonet.ListenTCP(w.guest, tcpip.FullAddress{NIC: 1, Addr: toAddr(containerAddr), Port: 8080}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		if c, err := l.Accept(); err == nil {
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := gonet.DialContextTCP(ctx, mac, tcpip.FullAddress{NIC: 1, Addr: toAddr(containerAddr), Port: 8080}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("the guest was never reached: %v", err)
	}
	c.Close()

	w.ns.Close()
	select {
	case <-w.ns.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the stack's end never ran")
	}
	if w.ns.ctx.Err() == nil {
		t.Error("the stack's context outlived its link")
	}
	before := w.tun.Stats().Unreachable
	syn := tcpFrame(netip.AddrPortFrom(tunnelMac, 50124), netip.AddrPortFrom(containerAddr, 80), 7, header.TCPFlagSyn, nil)[header.EthernetMinimumSize:]
	macWrite(t, w.mac, syn)
	deadline := time.Now().Add(5 * time.Second)
	for w.tun.Stats().Unreachable == before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := w.tun.Stats(); got.Unreachable == before {
		t.Fatalf("after the guest's link ended the Mac was not told: %v", got)
	}
}
