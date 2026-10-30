package netstack

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"fev/proto/netplan"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

var (
	tunnelMac6  = plan6.TunnelMac6()
	tunnelMac46 = plan.TunnelMac46()
	pref64      = netip.MustParsePrefix("64:ff9b::/96")
	machine6    = netip.MustParseAddr("fd5b:6e2a:91c0:1::5")
	gateway6    = netip.MustParseAddr("fd5b:6e2a:91c0::1")
)

func tunnel6World(t *testing.T, offload bool, mtu int) *tunnelWorld {
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
	g := newGuestStack6(t, guestEnd, offload)
	fd, mac := standInUtun(t)
	tun, err := NewTunnel(fd, TunnelConfig{Mac: tunnelMac, Peer: tunnelPeer, Mac6: tunnelMac6, Peer6: plan6.TunnelPeer6(), Mac46: tunnelMac46, MTU: mtu}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tun.SetPrefixes(append(plan.Routed(), plan6.Routed6()...))
	tun.SetNAT64([]netip.Prefix{pref64})
	tun.Attach(ns)
	waitLinkReady(t, ns)
	if offload {
		deadline := time.Now().Add(5 * time.Second)
		for !ns.link.offload.Load() {
			if time.Now().After(deadline) {
				t.Fatal("the stack never heard what the guest accepts")
			}
			time.Sleep(time.Millisecond)
		}
	}
	t.Cleanup(func() {
		tun.Close()
		ns.Close()
		g.Close()
	})
	return &tunnelWorld{world: &world{host: h, ns: ns, guest: g}, tun: tun, mac: mac}
}

func newGuestStack6(t testing.TB, guestEnd *vmmEnd, offload bool) *stack.Stack {
	t.Helper()
	g := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6},
	})
	ep := guestEnd.endpoint()
	ep.offload.Store(offload)
	ep.announced.Store(true)
	if err := g.CreateNIC(1, ethernet.New(ep)); err != nil {
		t.Fatal(err)
	}
	ep.start(nil)
	for _, a := range []tcpip.ProtocolAddress{
		{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: toAddr(guestAddr), PrefixLen: 24}},
		{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: toAddr(containerAddr), PrefixLen: 24}},
		{Protocol: ipv6.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom16(machine6.As16()), PrefixLen: 64}},
	} {
		if err := g.AddProtocolAddress(1, a, stack.AddressProperties{}); err != nil {
			t.Fatal(err)
		}
	}
	subnet, _ := tcpip.NewSubnet(toAddr(DefaultConfig.Subnet.Addr()), tcpip.MaskFromBytes([]byte{255, 255, 255, 0}))
	g.SetRouteTable([]tcpip.Route{
		{Destination: subnet, NIC: 1},
		{Destination: header.IPv4EmptySubnet, Gateway: toAddr(DefaultConfig.Gateway), NIC: 1},
		{Destination: header.IPv6EmptySubnet, Gateway: tcpip.AddrFrom16(gateway6.As16()), NIC: 1},
	})
	if err := g.AddStaticNeighbor(1, header.IPv6ProtocolNumber, tcpip.AddrFrom16(gateway6.As16()), GatewayMAC); err != nil {
		t.Fatal(err)
	}
	return g
}

func macStack6(t *testing.T, c *net.UnixConn, mtu uint32) *stack.Stack {
	t.Helper()
	ep := channel.New(512, mtu, "")
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol6},
	})
	if err := s.CreateNIC(1, ep); err != nil {
		t.Fatal(err)
	}
	a := tcpip.ProtocolAddress{Protocol: ipv6.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom16(tunnelMac6.As16()), PrefixLen: 128}}
	if err := s.AddProtocolAddress(1, a, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	var routes []tcpip.Route
	for _, p := range append(plan6.Routed6(), pref64) {
		sub, err := tcpip.NewSubnet(tcpip.AddrFrom16(p.Addr().As16()), tcpip.MaskFromBytes(net.CIDRMask(p.Bits(), 128)))
		if err != nil {
			t.Fatal(err)
		}
		routes = append(routes, tcpip.Route{Destination: sub, NIC: 1})
	}
	s.SetRouteTable(routes)
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
			c.Write(append(binary.BigEndian.AppendUint32(nil, afInet6), data...))
		}
	}()
	go func() {
		buf := make([]byte, 70000)
		for {
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			if n <= tunHdr || buf[tunHdr]>>4 != 6 {
				continue
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), buf[tunHdr:n]...))})
			ep.InjectInbound(ipv6.ProtocolNumber, pkt)
			pkt.DecRef()
		}
	}()
	t.Cleanup(func() {
		cancel()
		s.Close()
	})
	return s
}

func addr6(a netip.Addr) tcpip.Address { return tcpip.AddrFrom16(a.As16()) }

func echoTCP(t *testing.T, g *stack.Stack, ap netip.AddrPort) <-chan netip.Addr {
	t.Helper()
	proto := ipv4.ProtocolNumber
	a := toAddr(ap.Addr())
	if ap.Addr().Is6() {
		proto, a = ipv6.ProtocolNumber, addr6(ap.Addr())
	}
	l, err := gonet.ListenTCP(g, tcpip.FullAddress{NIC: 1, Addr: a, Port: ap.Port()}, proto)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	peers := make(chan netip.Addr, 8)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			peers <- c.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap()
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	return peers
}

func roundTrip(t *testing.T, w *tunnelWorld, mac *stack.Stack, ap netip.AddrPort, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := gonet.DialContextTCP(ctx, mac, tcpip.FullAddress{NIC: 1, Addr: addr6(ap.Addr()), Port: ap.Port()}, ipv6.ProtocolNumber)
	if err != nil {
		t.Fatalf("dial %v from the Mac: %v (%v)", ap, err, w.tun.Stats())
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	payload := make([]byte, n)
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
}

func TestTheMacReachesAGuestIPv6AddressThroughTheTunnel(t *testing.T) {
	for _, offload := range []bool{false, true} {
		t.Run(map[bool]string{false: "no offload", true: "offload"}[offload], func(t *testing.T) {
			w := tunnel6World(t, offload, 1500)
			mac := macStack6(t, w.mac, 1500)
			peers := echoTCP(t, w.guest, netip.AddrPortFrom(machine6, 8080))
			roundTrip(t, w, mac, netip.AddrPortFrom(machine6, 8080), 1<<20)
			if p := <-peers; p != tunnelMac6 {
				t.Fatalf("the machine saw %s, want the Mac's %s", p, tunnelMac6)
			}
			if s := w.tun.Stats(); s.SpoofedFromMac+s.SpoofedFromGuest+s.Malformed+s.WriteFailed+s.Translated != 0 {
				t.Fatalf("stats %v", s)
			}
		})
	}
}

func TestTheMacReachesAGuestAddressThroughItsNAT64Address(t *testing.T) {
	for _, c := range []struct {
		mtu     int
		offload bool
	}{{1500, false}, {1500, true}, {65535, true}, {16384, false}} {
		t.Run(map[bool]string{false: "no offload", true: "offload"}[c.offload]+" mtu "+strconv.Itoa(c.mtu), func(t *testing.T) {
			w := tunnel6World(t, c.offload, c.mtu)
			mac := macStack6(t, w.mac, uint32(c.mtu))
			peers := echoTCP(t, w.guest, netip.AddrPortFrom(containerAddr, 8080))
			roundTrip(t, w, mac, netip.AddrPortFrom(netplan.NAT64Address(pref64, containerAddr), 8080), 1<<20)
			if p := <-peers; p != tunnelMac46 {
				t.Fatalf("the container saw %s, want the Mac's NAT64 address %s", p, tunnelMac46)
			}
			s := w.tun.Stats()
			if s.SpoofedFromMac+s.SpoofedFromGuest+s.Malformed+s.WriteFailed != 0 || s.Translated == 0 {
				t.Fatalf("stats %v", s)
			}
			if len(w.host.tcpDials) != 0 {
				t.Fatalf("the stack dialled %v", w.host.tcpDials)
			}
		})
	}
}

func TestUDPAndFragmentsCrossTheNAT64Translation(t *testing.T) {
	w := tunnel6World(t, true, 1500)
	mac := macStack6(t, w.mac, 1500)
	pc, err := gonet.DialUDP(w.guest, &tcpip.FullAddress{NIC: 1, Addr: toAddr(containerAddr), Port: 5353}, nil, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	from := make(chan netip.Addr, 4)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, f, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			from <- f.(*net.UDPAddr).AddrPort().Addr().Unmap()
			pc.WriteTo(append([]byte("echo "), buf[:n]...), f)
		}
	}()
	c, err := gonet.DialUDP(mac, nil, &tcpip.FullAddress{NIC: 1, Addr: addr6(netplan.NAT64Address(pref64, containerAddr)), Port: 5353}, ipv6.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, size := range []int{5, 4000} {
		msg := bytes.Repeat([]byte("u"), size)
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write(msg); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 8192)
		n, err := c.Read(buf)
		if err != nil || !bytes.Equal(buf[:n], append([]byte("echo "), msg...)) {
			t.Fatalf("%d bytes: read %d, %v (%v)", size, n, err, w.tun.Stats())
		}
		if f := <-from; f != tunnelMac46 {
			t.Fatalf("the container saw %s", f)
		}
	}
	if s := w.tun.Stats(); s.Malformed+s.WriteFailed != 0 {
		t.Fatalf("stats %v", s)
	}
}

func TestAPingToTheNAT64AddressIsAnswered(t *testing.T) {
	w := tunnel6World(t, false, 1500)
	dst := netplan.NAT64Address(pref64, containerAddr)
	macWrite(t, w.mac, echoRequest6(tunnelMac6, dst, 91, 3))
	for {
		pkt, ok := macRead(t, w.mac, 5*time.Second)
		if !ok {
			t.Fatalf("no echo reply (%v)", w.tun.Stats())
		}
		ip := header.IPv6(pkt)
		if pkt[0]>>4 != 6 || ip.TransportProtocol() != header.ICMPv6ProtocolNumber {
			continue
		}
		m := header.ICMPv6(ip.Payload())
		if m.Type() != header.ICMPv6EchoReply || m.Ident() != 91 || m.Sequence() != 3 || fromAddr(ip.SourceAddress()) != dst || fromAddr(ip.DestinationAddress()) != tunnelMac6 {
			t.Fatalf("got type %d ident %d from %v to %v", m.Type(), m.Ident(), fromAddr(ip.SourceAddress()), fromAddr(ip.DestinationAddress()))
		}
		if header.ICMPv6Checksum(header.ICMPv6ChecksumParams{Header: m, Src: ip.SourceAddress(), Dst: ip.DestinationAddress()}) != m.Checksum() {
			t.Fatal("the reply's checksum is wrong")
		}
		return
	}
}

func echoRequest6(src, dst netip.Addr, ident, seq uint16) []byte {
	msg := make([]byte, header.ICMPv6EchoMinimumSize+4)
	m := header.ICMPv6(msg)
	m.SetType(header.ICMPv6EchoRequest)
	m.SetIdent(ident)
	m.SetSequence(seq)
	copy(msg[header.ICMPv6EchoMinimumSize:], "ping")
	m.SetChecksum(header.ICMPv6Checksum(header.ICMPv6ChecksumParams{Header: m, Src: addr6(src), Dst: addr6(dst)}))
	pkt := make([]byte, header.IPv6MinimumSize+len(msg))
	header.IPv6(pkt).Encode(&header.IPv6Fields{PayloadLength: uint16(len(msg)), TransportProtocol: header.ICMPv6ProtocolNumber, HopLimit: 64, SrcAddr: addr6(src), DstAddr: addr6(dst)})
	copy(pkt[header.IPv6MinimumSize:], msg)
	return pkt
}

func TestTheGuestsErrorsReachTheMacTranslatedWithWhatTheyQuote(t *testing.T) {
	w := tunnel6World(t, false, 1500)
	dst := netplan.NAT64Address(pref64, containerAddr)
	macWrite(t, w.mac, udp6(tunnelMac6, dst, 40000, 9, []byte("anyone")))
	for {
		pkt, ok := macRead(t, w.mac, 5*time.Second)
		if !ok {
			t.Fatalf("no error (%v)", w.tun.Stats())
		}
		ip := header.IPv6(pkt)
		if pkt[0]>>4 != 6 || ip.TransportProtocol() != header.ICMPv6ProtocolNumber {
			continue
		}
		m := header.ICMPv6(ip.Payload())
		if m.Type() != header.ICMPv6DstUnreachable || m.Code() != header.ICMPv6PortUnreachable {
			t.Fatalf("type %d code %d", m.Type(), m.Code())
		}
		if header.ICMPv6Checksum(header.ICMPv6ChecksumParams{Header: m, Src: ip.SourceAddress(), Dst: ip.DestinationAddress()}) != m.Checksum() {
			t.Fatal("the error's checksum is wrong")
		}
		q := header.IPv6(m.Payload())
		if fromAddr(q.SourceAddress()) != tunnelMac6 || fromAddr(q.DestinationAddress()) != dst || q.TransportProtocol() != header.UDPProtocolNumber {
			t.Fatalf("it quotes %v > %v proto %d", fromAddr(q.SourceAddress()), fromAddr(q.DestinationAddress()), q.TransportProtocol())
		}
		u := header.UDP(q.Payload())
		if u.SourcePort() != 40000 || u.DestinationPort() != 9 {
			t.Fatalf("it quotes ports %d > %d", u.SourcePort(), u.DestinationPort())
		}
		sum := header.PseudoHeaderChecksum(header.UDPProtocolNumber, q.SourceAddress(), q.DestinationAddress(), u.Length())
		if checksum.Checksum(q.Payload()[:u.Length()], sum) != 0xffff {
			t.Fatal("the quoted datagram's checksum is not whole under the Mac's addresses")
		}
		return
	}
}

func udp6(src, dst netip.Addr, sport, dport uint16, data []byte) []byte {
	pkt := make([]byte, header.IPv6MinimumSize+header.UDPMinimumSize+len(data))
	header.IPv6(pkt).Encode(&header.IPv6Fields{PayloadLength: uint16(header.UDPMinimumSize + len(data)), TransportProtocol: header.UDPProtocolNumber, HopLimit: 64, SrcAddr: addr6(src), DstAddr: addr6(dst)})
	u := header.UDP(pkt[header.IPv6MinimumSize:])
	u.Encode(&header.UDPFields{SrcPort: sport, DstPort: dport, Length: uint16(header.UDPMinimumSize + len(data))})
	copy(u[header.UDPMinimumSize:], data)
	sum := header.PseudoHeaderChecksum(header.UDPProtocolNumber, addr6(src), addr6(dst), uint16(len(u)))
	u.SetChecksum(^checksum.Checksum(u, sum))
	return pkt
}

func TestANAT64PacketWithAnotherSourceOrForAnotherPrefixIsDropped(t *testing.T) {
	w := tunnel6World(t, false, 1500)
	other := netip.MustParseAddr("2001:db8::7")
	macWrite(t, w.mac, echoRequest6(other, netplan.NAT64Address(pref64, containerAddr), 1, 1))
	macWrite(t, w.mac, echoRequest6(tunnelMac6, netplan.NAT64Address(pref64, guestAddr), 1, 2))
	if pkt, ok := macRead(t, w.mac, 300*time.Millisecond); ok {
		t.Fatalf("answered: % x", pkt[:min(len(pkt), 40)])
	}
	if s := w.tun.Stats(); s.SpoofedFromMac != 2 || s.ToGuest != 0 || s.Translated != 0 {
		t.Fatalf("stats %v", s)
	}
	w.tun.SetNAT64(nil)
	macWrite(t, w.mac, echoRequest6(tunnelMac6, netplan.NAT64Address(pref64, containerAddr), 1, 3))
	if pkt, ok := macRead(t, w.mac, 300*time.Millisecond); ok {
		t.Fatalf("answered with no NAT64 prefix: % x", pkt[:min(len(pkt), 40)])
	}
	if s := w.tun.Stats(); s.SpoofedFromMac != 3 || s.Translated != 0 {
		t.Fatalf("stats %v", s)
	}
}

func TestWithNoGuestTheMacIsToldAnIPv6AddressIsUnreachable(t *testing.T) {
	fd, mac := standInUtun(t)
	tun, err := NewTunnel(fd, TunnelConfig{Mac: tunnelMac, Peer: tunnelPeer, Mac6: tunnelMac6, Peer6: plan6.TunnelPeer6(), Mac46: tunnelMac46}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	tun.SetPrefixes(append(plan.Routed(), plan6.Routed6()...))
	tun.SetNAT64([]netip.Prefix{pref64})
	for _, dst := range []netip.Addr{machine6, netplan.NAT64Address(pref64, containerAddr)} {
		sent := udp6(tunnelMac6, dst, 40001, 7, []byte("x"))
		macWrite(t, mac, sent)
		pkt, ok := macRead(t, mac, 2*time.Second)
		if !ok {
			t.Fatalf("%s: no answer (%v)", dst, tun.Stats())
		}
		ip := header.IPv6(pkt)
		m := header.ICMPv6(ip.Payload())
		if pkt[0]>>4 != 6 || fromAddr(ip.SourceAddress()) != plan6.TunnelPeer6() || fromAddr(ip.DestinationAddress()) != tunnelMac6 ||
			m.Type() != header.ICMPv6DstUnreachable || m.Code() != header.ICMPv6AddressUnreachable || !bytes.Equal(m.Payload(), sent) {
			t.Fatalf("%s: % x", dst, pkt[:min(len(pkt), 48)])
		}
		if header.ICMPv6Checksum(header.ICMPv6ChecksumParams{Header: m, Src: ip.SourceAddress(), Dst: ip.DestinationAddress()}) != m.Checksum() {
			t.Fatal("its checksum is wrong")
		}
	}
	macWrite(t, mac, append(echoRequest6(tunnelMac6, machine6, 1, 1)[:header.IPv6MinimumSize:header.IPv6MinimumSize], 1, 4, 0, 0, 0, 0, 0, 0))
	if _, ok := macRead(t, mac, 200*time.Millisecond); ok {
		t.Fatal("an error was answered")
	}
}

func TestASynToTheMacsNAT64AddressIsToldWhatTheUtunCarries(t *testing.T) {
	for _, c := range []struct {
		mtu  int
		want uint16
	}{{1500, 1460}, {16384, 16324}, {65535, 65475}} {
		t.Run(strconv.Itoa(c.mtu), func(t *testing.T) {
			hostEnd, guestEnd := socketPair(t)
			ns, err := New(hostEnd, Options{Host: newFakeHost()})
			if err != nil {
				t.Fatal(err)
			}
			defer ns.Close()
			fd, mac := standInUtun(t)
			tun, err := NewTunnel(fd, TunnelConfig{Mac: tunnelMac, Peer: tunnelPeer, Mac6: tunnelMac6, Peer6: plan6.TunnelPeer6(), Mac46: tunnelMac46, MTU: c.mtu}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tun.Close()
			tun.SetPrefixes(plan.Routed())
			tun.SetNAT64([]netip.Prefix{pref64})
			tun.Attach(ns)
			opts := []byte{2, 4, 0x05, 0xb4, 4, 2, 8, 10, 0, 0, 0, 9, 0, 0, 0, 1, 1, 3, 3, 7}
			frame := tcpFrameOpts(netip.AddrPortFrom(containerAddr, 8080), netip.AddrPortFrom(tunnelMac46, 51000), header.TCPFlagSyn|header.TCPFlagAck, opts)
			var vhdr [netHdrLen]byte
			guestEnd.send(append(vhdr[:], frame...))
			pkt, ok := macRead(t, mac, 5*time.Second)
			if !ok || pkt[0]>>4 != 6 {
				t.Fatalf("no IPv6 SYN-ACK (%v)", tun.Stats())
			}
			ip := header.IPv6(pkt)
			tc := header.TCP(ip.Payload())
			sum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(tc)))
			if checksum.Checksum(tc, sum) != 0xffff {
				t.Fatal("its checksum is wrong")
			}
			if fromAddr(ip.SourceAddress()) != netplan.NAT64Address(pref64, containerAddr) || fromAddr(ip.DestinationAddress()) != tunnelMac6 {
				t.Fatalf("%v > %v", fromAddr(ip.SourceAddress()), fromAddr(ip.DestinationAddress()))
			}
			if mss := mssOf(tc); mss != c.want {
				t.Fatalf("MSS %d, want %d", mss, c.want)
			}
		})
	}
}

func TestTo6CutsOrRefusesWhatDoesNotFitTheUtun(t *testing.T) {
	big := func(df bool, sum bool) []byte {
		data := bytes.Repeat([]byte{0x5a}, 1472)
		pkt := make([]byte, header.IPv4MinimumSize+header.UDPMinimumSize+len(data))
		f := header.IPv4Fields{TotalLength: uint16(len(pkt)), TTL: 64, ID: 0x1234, Protocol: uint8(header.UDPProtocolNumber), SrcAddr: toAddr(containerAddr), DstAddr: toAddr(tunnelMac46)}
		if df {
			f.Flags = header.IPv4FlagDontFragment
		}
		header.IPv4(pkt).Encode(&f)
		u := header.UDP(pkt[header.IPv4MinimumSize:])
		u.Encode(&header.UDPFields{SrcPort: 53, DstPort: 40000, Length: uint16(header.UDPMinimumSize + len(data))})
		copy(u[header.UDPMinimumSize:], data)
		if sum {
			s := header.PseudoHeaderChecksum(header.UDPProtocolNumber, toAddr(containerAddr), toAddr(tunnelMac46), uint16(len(u)))
			u.SetChecksum(^checksum.Checksum(u, s))
		}
		return pkt
	}
	quote := func(a netip.Addr) (netip.Addr, bool) { return tunnelMac6, a == tunnelMac46 }
	var told int
	var out [][]byte
	emit := func(b []byte) { out = append(out, append([]byte(nil), b[tunHdr:]...)) }
	if !to6(big(true, true), 0, pref64, tunnelMac6, 1500, quote, emit, func(m int) { told = m }) || told != 1480 || len(out) != 0 {
		t.Fatalf("DF: told %d, %d packets", told, len(out))
	}
	if !to6(big(false, true), 0, pref64, tunnelMac6, 1500, quote, emit, func(int) { t.Fatal("told") }) || len(out) != 2 {
		t.Fatalf("no DF: %d packets", len(out))
	}
	var data []byte
	for i, p := range out {
		if len(p) > 1500 || header.IPv6(p).NextHeader() != uint8(header.IPv6FragmentExtHdrIdentifier) {
			t.Fatalf("fragment %d: %d bytes, next header %d", i, len(p), header.IPv6(p).NextHeader())
		}
		fh := p[header.IPv6MinimumSize:]
		if binary.BigEndian.Uint32(fh[4:]) != 0x1234 || int(binary.BigEndian.Uint16(fh[2:])&^7) != len(data) || (fh[3]&1 == 1) != (i == 0) {
			t.Fatalf("fragment %d: header % x", i, fh[:8])
		}
		data = append(data, fh[header.IPv6FragmentExtHdrLength:]...)
	}
	u := header.UDP(data)
	sum := header.PseudoHeaderChecksum(header.UDPProtocolNumber, addr6(netplan.NAT64Address(pref64, containerAddr)), addr6(tunnelMac6), uint16(len(u)))
	if len(data) != 1480 || checksum.Checksum(u, sum) != 0xffff {
		t.Fatalf("reassembled: %d bytes, checksum whole %v", len(data), checksum.Checksum(u, sum) == 0xffff)
	}
	out = nil
	small := big(true, false)
	small = small[:header.IPv4MinimumSize+header.UDPMinimumSize+100]
	header.IPv4(small).SetTotalLength(uint16(len(small)))
	header.UDP(small[header.IPv4MinimumSize:]).SetLength(header.UDPMinimumSize + 100)
	if !to6(small, 0, pref64, tunnelMac6, 1500, quote, emit, nil) || len(out) != 1 {
		t.Fatalf("no checksum: %d packets", len(out))
	}
	ip := header.IPv6(out[0])
	u = header.UDP(ip.Payload())
	if u.Checksum() == 0 || checksum.Checksum(u, header.PseudoHeaderChecksum(header.UDPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(u)))) != 0xffff {
		t.Fatal("no checksum made")
	}
	frag := big(false, false)
	binary.BigEndian.PutUint16(frag[6:], 0x2000)
	if to6(frag, 0, pref64, tunnelMac6, 1500, quote, emit, nil) {
		t.Fatal("a fragment with no checksum translated")
	}
}

func TestAGuestPacketToTheMacsNewAddressesWithAForeignSourceNeverReachesTheMac(t *testing.T) {
	hostEnd, guestEnd := socketPair(t)
	ns, err := New(hostEnd, Options{Host: newFakeHost()})
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	fd, mac := standInUtun(t)
	tun, err := NewTunnel(fd, TunnelConfig{Mac: tunnelMac, Peer: tunnelPeer, Mac6: tunnelMac6, Peer6: plan6.TunnelPeer6(), Mac46: tunnelMac46, MTU: 1500}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	tun.SetPrefixes(append(plan.Routed(), plan6.Routed6()...))
	tun.SetNAT64([]netip.Prefix{pref64})
	tun.Attach(ns)
	frame6 := func(src netip.Addr) []byte {
		pkt := udp6(src, tunnelMac6, 4000, 53, []byte("x"))
		f := make([]byte, header.EthernetMinimumSize, header.EthernetMinimumSize+len(pkt))
		header.Ethernet(f).Encode(&header.EthernetFields{SrcAddr: guestMAC, DstAddr: GatewayMAC, Type: header.IPv6ProtocolNumber})
		return append(f, pkt...)
	}
	to46 := netip.AddrPortFrom(tunnelMac46, 22)
	for _, src := range []string{"8.8.8.8", "192.168.1.10", "198.19.3.1"} {
		guestEnd.sendFrame(tcpFrame(netip.AddrPortFrom(netip.MustParseAddr(src), 4000), to46, 1, header.TCPFlagSyn, nil))
	}
	for _, src := range []string{"2001:db8::7", "64:ff9b::808:808", "fd00::1"} {
		guestEnd.sendFrame(frame6(netip.MustParseAddr(src)))
	}
	if pkt, ok := macRead(t, mac, 300*time.Millisecond); ok {
		t.Fatalf("a foreign source reached the Mac: % x (%v)", pkt[:min(len(pkt), 40)], tun.Stats())
	}
	if s := tun.Stats(); s.SpoofedFromGuest != 6 || s.Translated != 0 || s.ToMac != 0 {
		t.Fatalf("stats %v", s)
	}
	guestEnd.sendFrame(tcpFrame(netip.AddrPortFrom(containerAddr, 4000), to46, 1, header.TCPFlagSyn, nil))
	pkt, ok := macRead(t, mac, 5*time.Second)
	if !ok || pkt[0]>>4 != 6 || fromAddr(header.IPv6(pkt).SourceAddress()) != netplan.NAT64Address(pref64, containerAddr) ||
		fromAddr(header.IPv6(pkt).DestinationAddress()) != tunnelMac6 {
		t.Fatalf("fev's own source to the NAT64 address: %v (%v)", ok, tun.Stats())
	}
	guestEnd.sendFrame(frame6(machine6))
	pkt, ok = macRead(t, mac, 5*time.Second)
	if !ok || pkt[0]>>4 != 6 || fromAddr(header.IPv6(pkt).SourceAddress()) != machine6 {
		t.Fatalf("a machine's IPv6 source: %v (%v)", ok, tun.Stats())
	}
}
