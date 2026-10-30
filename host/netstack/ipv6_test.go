package netstack

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"fev/proto/netplan"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

var plan6 = func() netplan.Plan6 {
	p, err := netplan.Parse6("fd5b:6e2a:91c0::/48")
	if err != nil {
		panic(err)
	}
	return p
}()

type v6Host struct {
	*fakeHost
	pings6  chan *fakePing
	noRoute netip.Addr
	aaaa    atomic.Int32
}

func (h *v6Host) Resolve(ctx context.Context, name string, qtype uint16) ([]dns.RR, error) {
	if qtype == dns.TypeAAAA {
		h.aaaa.Add(1)
	}
	return h.fakeHost.Resolve(ctx, name, qtype)
}

func (h *v6Host) ListenICMP6() (PingConn, error) {
	p := &fakePing{sent: make(chan pingSent, 16), replies: make(chan pingReply, 16), closed: make(chan struct{})}
	h.pings6 <- p
	return p, nil
}

func (h *v6Host) DialTCP(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	if addr.Addr() == h.noRoute {
		h.fakeHost.mu.Lock()
		h.fakeHost.tcpDials = append(h.fakeHost.tcpDials, addr)
		h.fakeHost.mu.Unlock()
		return nil, &net.OpError{Op: "dial", Net: "tcp6", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}
	}
	return h.fakeHost.DialTCP(ctx, addr)
}

type world6 struct {
	host  *v6Host
	ns    *Stack
	guest *stack.Stack
	cfg   Config
}

func newWorld6(t testing.TB, opts Options, accepts uint32) (*world6, *largest) {
	t.Helper()
	hostEnd, guestEnd := socketPair(t)
	if accepts != 0 {
		announce(t, guestEnd, accepts)
	}
	h := &v6Host{fakeHost: newFakeHost(), pings6: make(chan *fakePing, 8), noRoute: netip.MustParseAddr("2001:db8::99")}
	opts.Host = h
	opts.Config = DefaultConfig.WithIPv6(plan6)
	toGuest := &largest{}
	ns, err := newWatched(hostEnd, opts, toGuest)
	if err != nil {
		t.Fatal(err)
	}
	g := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol, ipv6.NewProtocolWithOptions(ipv6Options())},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol6},
	})
	ep := guestEnd.endpoint()
	ep.offload.Store(accepts&acceptsTSO4 != 0)
	ep.announced.Store(true)
	if err := g.CreateNIC(1, ethernet.New(ep)); err != nil {
		t.Fatal(err)
	}
	ep.start(nil)
	cfg := opts.Config
	for _, a := range []tcpip.ProtocolAddress{
		{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: toAddr(guestAddr), PrefixLen: 24}},
		{Protocol: ipv6.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: toAddr(plan6.Guest6().Addr()), PrefixLen: 64}},
	} {
		if err := g.AddProtocolAddress(1, a, stack.AddressProperties{}); err != nil {
			t.Fatal(err)
		}
	}
	subnet6, _ := tcpip.NewSubnet(toAddr(cfg.Subnet6.Addr()), tcpip.MaskFromBytes(net.CIDRMask(64, 128)))
	g.SetRouteTable([]tcpip.Route{
		{Destination: subnet6, NIC: 1},
		{Destination: header.IPv6EmptySubnet, Gateway: toAddr(cfg.Gateway6), NIC: 1},
		{Destination: header.IPv4EmptySubnet, Gateway: toAddr(cfg.Gateway), NIC: 1},
	})
	t.Cleanup(func() {
		ns.Close()
		g.Close()
	})
	return &world6{host: h, ns: ns, guest: g, cfg: cfg}, toGuest
}

func (w *world6) dialTCP(t testing.TB, dst string) (*gonet.TCPConn, error) {
	t.Helper()
	ap := netip.MustParseAddrPort(dst)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return gonet.DialContextTCP(ctx, w.guest, tcpip.FullAddress{NIC: 1, Addr: toAddr(ap.Addr()), Port: ap.Port()}, ipv6.ProtocolNumber)
}

func TestIPv6Forwardable(t *testing.T) {
	c := DefaultConfig.WithIPv6(plan6)
	yes := []string{"2606:4700::1111", "2001:db8::1", "fdc5:1:2:3::9efb", "fd7a:115c:a1e0::1"}
	no := []string{"::1", "::", "fe80::1", "ff02::1", "fd5b:6e2a:91c0::1", "fd5b:6e2a:91c0:3::7", "fd5b:6e2a:91c0:ffff::1"}
	for _, a := range yes {
		if !c.forwardable(netip.MustParseAddr(a)) {
			t.Errorf("%s should be forwardable", a)
		}
	}
	for _, a := range no {
		if c.forwardable(netip.MustParseAddr(a)) {
			t.Errorf("%s should not be forwardable", a)
		}
	}
	if DefaultConfig.forwardable(netip.MustParseAddr("2606:4700::1111")) {
		t.Error("IPv6 forwardable with no IPv6 configured")
	}
	if got, ok := c.route(netip.AddrPortFrom(c.Host6, 5432)); !ok || got != netip.MustParseAddrPort("[::1]:5432") {
		t.Fatalf("host6: %v %v", got, ok)
	}
}

func TestTCPOverIPv6IsForwardedToTheMac(t *testing.T) {
	w, _ := newWorld6(t, Options{}, 0)
	c, err := w.dialTCP(t, "[2606:4700::1111]:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("hello six"))
	c.CloseWrite()
	got, _ := io.ReadAll(c)
	if string(got) != "hello six<eof seen>" {
		t.Fatalf("got %q", got)
	}
	if w.host.tcpDials[0] != netip.MustParseAddrPort("[2606:4700::1111]:443") {
		t.Fatalf("dialled %v", w.host.tcpDials)
	}
	c2, err := w.dialTCP(t, netip.AddrPortFrom(w.cfg.Host6, 5432).String())
	if err != nil {
		t.Fatal(err)
	}
	c2.Close()
	if last := w.host.tcpDials[len(w.host.tcpDials)-1]; last != netip.MustParseAddrPort("[::1]:5432") {
		t.Fatalf("host6 dialled as %v", last)
	}
	if _, err := w.dialTCP(t, "[fd5b:6e2a:91c0:3::5]:80"); err == nil {
		t.Fatal("a connection into fev's own /48 succeeded")
	}
}

type wire6 struct {
	t    *testing.T
	conn *vmmEnd
	host *v6Host
	cfg  Config
}

func newWire6(t *testing.T) *wire6 {
	t.Helper()
	hostEnd, guestEnd := socketPair(t)
	h := &v6Host{fakeHost: newFakeHost(), pings6: make(chan *fakePing, 8), noRoute: netip.MustParseAddr("2001:db8::99")}
	cfg := DefaultConfig.WithIPv6(plan6)
	ns, err := New(hostEnd, Options{Host: h, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		guestEnd.Close()
		ns.Close()
	})
	return &wire6{t: t, conn: guestEnd, host: h, cfg: cfg}
}

var guest6 = plan6.Guest6().Addr()

func (w *wire6) send6(dst netip.Addr, hops uint8, proto tcpip.TransportProtocolNumber, l4 []byte) []byte {
	w.t.Helper()
	frame := make([]byte, header.EthernetMinimumSize+header.IPv6MinimumSize+len(l4))
	header.Ethernet(frame).Encode(&header.EthernetFields{SrcAddr: guestMAC, DstAddr: GatewayMAC, Type: ipv6.ProtocolNumber})
	header.IPv6(frame[header.EthernetMinimumSize:]).Encode(&header.IPv6Fields{
		PayloadLength: uint16(len(l4)), TransportProtocol: proto, HopLimit: hops,
		SrcAddr: toAddr(guest6), DstAddr: toAddr(dst),
	})
	copy(frame[header.EthernetMinimumSize+header.IPv6MinimumSize:], l4)
	w.conn.sendFrame(frame)
	return frame[header.EthernetMinimumSize:]
}

func (w *wire6) sendEcho6(dst netip.Addr, hops uint8, ident, seq uint16, payload string) []byte {
	msg := make([]byte, header.ICMPv6EchoMinimumSize+len(payload))
	m := header.ICMPv6(msg)
	m.SetType(header.ICMPv6EchoRequest)
	m.SetIdent(ident)
	m.SetSequence(seq)
	copy(msg[header.ICMPv6EchoMinimumSize:], payload)
	m.SetChecksum(header.ICMPv6Checksum(header.ICMPv6ChecksumParams{Header: msg, Src: toAddr(guest6), Dst: toAddr(dst)}))
	return w.send6(dst, hops, header.ICMPv6ProtocolNumber, msg)
}

func (w *wire6) nextICMP6() (src netip.Addr, m header.ICMPv6) {
	w.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		msg, ok := w.conn.recv(deadline)
		if !ok {
			w.t.Fatal("no ICMPv6 message from the stack")
		}
		frame := msg[netHdrLen:]
		if header.Ethernet(frame).Type() != ipv6.ProtocolNumber {
			continue
		}
		ip := header.IPv6(frame[header.EthernetMinimumSize:])
		if ip.TransportProtocol() != header.ICMPv6ProtocolNumber || fromAddr(ip.DestinationAddress()) != guest6 {
			continue
		}
		m := header.ICMPv6(ip.Payload())
		if t := m.Type(); t == header.ICMPv6NeighborSolicit || t == header.ICMPv6NeighborAdvert || t == header.ICMPv6RouterAdvert {
			continue
		}
		want := header.ICMPv6Checksum(header.ICMPv6ChecksumParams{Header: m, Src: ip.SourceAddress(), Dst: ip.DestinationAddress()})
		if m.Checksum() != want {
			w.t.Fatalf("ICMPv6 type %d with a bad checksum", m.Type())
		}
		return fromAddr(ip.SourceAddress()), m
	}
}

func TestAnIPv6PingIsForwardedAndAnsweredAndOneHopLeftExpiresAtTheGateway(t *testing.T) {
	w := newWire6(t)
	dst := netip.MustParseAddr("2606:4700::1111")
	w.sendEcho6(dst, 64, 4242, 7, "six!")
	var p *fakePing
	select {
	case p = <-w.host.pings6:
	case <-time.After(5 * time.Second):
		t.Fatal("no ICMPv6 socket opened on the Mac")
	}
	sent := recvSent(t, p)
	if sent.dst != dst || sent.ttl != 63 || sent.ident != 4242 || sent.seq != 7 || string(sent.payload) != "six!" {
		t.Fatalf("sent %+v", sent)
	}
	p.replies <- pingReply{from: netip.MustParseAddr("2001:db8::7"), msg: append([]byte{129, 0, 0, 0, 0x10, 0x92, 0, 7}, "six!"...)}
	p.replies <- pingReply{from: dst, msg: append([]byte{129, 0, 0, 0, 0x10, 0x92, 0, 7}, "six!"...)}
	src, m := w.nextICMP6()
	if src != dst || m.Type() != header.ICMPv6EchoReply || m.Ident() != 4242 || m.Sequence() != 7 || string(m[header.ICMPv6EchoMinimumSize:]) != "six!" {
		t.Fatalf("got type %d from %v ident %d seq %d", m.Type(), src, m.Ident(), m.Sequence())
	}

	sentPkt := w.sendEcho6(dst, 3, 4242, 8, "hop")
	recvSent(t, p)
	router := netip.MustParseAddr("2001:db8:ffff::1")
	theirs := make([]byte, header.IPv6MinimumSize+8)
	header.IPv6(theirs).Encode(&header.IPv6Fields{PayloadLength: 11, TransportProtocol: header.ICMPv6ProtocolNumber, HopLimit: 1, SrcAddr: toAddr(netip.MustParseAddr("2001:db8::5")), DstAddr: toAddr(dst)})
	copy(theirs[header.IPv6MinimumSize:], []byte{128, 0, 0, 0, 0x10, 0x92, 0, 8})
	p.replies <- pingReply{from: router, msg: append([]byte{3, 0, 0, 0, 0, 0, 0, 0}, theirs...)}
	src, m = w.nextICMP6()
	if src != router || m.Type() != header.ICMPv6TimeExceeded || !bytes.Equal(m[header.ICMPv6ErrorHeaderSize:], sentPkt[:header.IPv6MinimumSize+8]) {
		t.Fatalf("time exceeded from %v type %d quoting % x", src, m.Type(), m[header.ICMPv6ErrorHeaderSize:])
	}

	w.sendEcho6(dst, 1, 4242, 9, "last")
	src, m = w.nextICMP6()
	if src != w.cfg.Gateway6 || m.Type() != header.ICMPv6TimeExceeded {
		t.Fatalf("got type %d from %v", m.Type(), src)
	}
}

func TestAnIPv6DestinationTheMacCannotRouteIsUnreachableNotRefused(t *testing.T) {
	w := newWire6(t)
	dst := netip.AddrPortFrom(w.host.noRoute, 443)
	syn := make([]byte, header.TCPMinimumSize)
	header.TCP(syn).Encode(&header.TCPFields{SrcPort: 40001, DstPort: dst.Port(), SeqNum: 0xabcdef01, DataOffset: header.TCPMinimumSize, Flags: header.TCPFlagSyn, WindowSize: 65535})
	sum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, toAddr(guest6), toAddr(dst.Addr()), uint16(len(syn)))
	header.TCP(syn).SetChecksum(^header.TCP(syn).CalculateChecksum(sum))
	sent := w.send6(dst.Addr(), 64, header.TCPProtocolNumber, syn)
	src, m := w.nextICMP6()
	if src != w.cfg.Gateway6 || m.Type() != header.ICMPv6DstUnreachable || m.Code() != header.ICMPv6NetworkUnreachable {
		t.Fatalf("got type %d code %d from %v", m.Type(), m.Code(), src)
	}
	if q := m[header.ICMPv6ErrorHeaderSize:]; !bytes.Equal(q, sent[:header.IPv6MinimumSize+8]) {
		t.Fatalf("quote % x, want % x", q, sent[:header.IPv6MinimumSize+8])
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for {
		msg, ok := w.conn.recv(deadline)
		if !ok {
			break
		}
		frame := msg[netHdrLen:]
		if header.Ethernet(frame).Type() == ipv6.ProtocolNumber && header.IPv6(frame[header.EthernetMinimumSize:]).TransportProtocol() == header.TCPProtocolNumber {
			t.Fatal("a TCP segment (a reset) was sent as well")
		}
	}
	w.host.mu.Lock()
	defer w.host.mu.Unlock()
	if len(w.host.tcpDials) != 1 {
		t.Fatalf("dials %v", w.host.tcpDials)
	}
	if len(w.host.fakeHost.tcpDials) == 0 {
		t.Fatal("not dialled")
	}
}

func TestUDPOverIPv6CarriesItsHopLimitAndComesBack(t *testing.T) {
	w, _ := newWorld6(t, Options{}, 0)
	c, err := gonet.DialUDP(w.guest, nil, &tcpip.FullAddress{NIC: 1, Addr: toAddr(netip.MustParseAddr("2606:4700::1111")), Port: 53}, ipv6.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("q6"))
	buf := make([]byte, 16)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "q6" {
		t.Fatalf("%q %v", buf[:n], err)
	}
	hc := <-w.host.udpConns
	if got := recvTTL(t, hc); got != defaultTTL-1 {
		t.Fatalf("hop limit %d, want %d", got, defaultTTL-1)
	}
}

func TestAAAAAnswersFollowWhetherTheMacHasGlobalIPv6(t *testing.T) {
	usable := false
	w, _ := newWorld6(t, Options{IPv6Usable: func() bool { return usable }}, 0)
	w.host.add(t, "example.com. 300 IN A 93.184.215.14", "example.com. 300 IN AAAA 2606:2800:21f:cb07:6820:80da:af6b:8b2c")
	q := func(name string, qtype uint16) *dns.Msg {
		ww := &world{host: w.host.fakeHost, ns: w.ns, guest: w.guest}
		return ww.query(t, "udp", name, qtype)
	}
	if r := q("example.com.", dns.TypeAAAA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
		t.Fatalf("AAAA with no global IPv6: rcode %d %q", r.Rcode, answers(r))
	}
	if n := w.host.aaaa.Load(); n != 0 {
		t.Fatalf("the Mac was asked %d AAAA questions it could not use", n)
	}
	usable = true
	if r := q("example.com.", dns.TypeAAAA); !strings.Contains(answers(r), "2606:2800") {
		t.Fatalf("AAAA with global IPv6: %q", answers(r))
	}
	for _, u := range []bool{false, true} {
		usable = u
		for _, name := range HostNames {
			if r := q(name, dns.TypeAAAA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
				t.Fatalf("%s AAAA (global IPv6 %v): rcode %d %q", name, u, r.Rcode, answers(r))
			}
			if r := q(name, dns.TypeA); answers(r) != name+" 60 IN A "+w.cfg.Host.String() {
				t.Fatalf("%s A: %q", name, answers(r))
			}
		}
	}
}

func TestLargeIPv6SegmentsAreCutForAGuestWithoutTSO6(t *testing.T) {
	for _, tc := range []struct {
		name    string
		accepts uint32
		large   bool
	}{
		{"TSO for IPv4 only", acceptsPartialCsum | acceptsTSO4, false},
		{"TSO for both", acceptsPartialCsum | acceptsTSO4 | acceptsTSO6, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, toGuest := newWorld6(t, Options{}, tc.accepts)
			c, err := w.dialTCP(t, "[2606:4700::1111]:80")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(20 * time.Second))
			payload := make([]byte, 2<<20)
			for i := range payload {
				payload[i] = byte(i*13 + i>>10)
			}
			go func() {
				c.Write(payload)
				c.CloseWrite()
			}()
			got, err := io.ReadAll(c)
			if err != nil || !bytes.Equal(got, append(payload, "<eof seen>"...)) {
				t.Fatalf("%d bytes back, %v", len(got), err)
			}
			big := toGuest.largestFrame() > ipMTU+header.EthernetMinimumSize
			if big != tc.large {
				t.Fatalf("largest frame to the guest %d", toGuest.largestFrame())
			}
		})
	}
}

func TestNoTimerAfterIPv6FlowsEnd(t *testing.T) {
	saved := [2]time.Duration{udpUnreplied, udpAssured}
	udpUnreplied, udpAssured = 200*time.Millisecond, 400*time.Millisecond
	defer func() { udpUnreplied, udpAssured = saved[0], saved[1] }()
	w, _ := newWorld6(t, Options{}, 0)
	c, err := w.dialTCP(t, "[2606:4700::1111]:80")
	if err != nil {
		t.Fatal(err)
	}
	for _, proto := range []tcpip.NetworkProtocolNumber{ipv4.ProtocolNumber, ipv6.ProtocolNumber} {
		nud, terr := w.ns.stack.NUDConfigurations(nicID, proto)
		if terr != nil {
			t.Fatal(terr)
		}
		nud.BaseReachableTime = time.Second
		if terr := w.ns.stack.SetNUDConfigurations(nicID, proto, nud); terr != nil {
			t.Fatal(terr)
		}
	}
	c.Write([]byte("x"))
	c.CloseWrite()
	io.ReadAll(c)
	c.Close()
	u, err := gonet.DialUDP(w.guest, nil, &tcpip.FullAddress{NIC: 1, Addr: toAddr(netip.MustParseAddr("2606:4700::1111")), Port: 53}, ipv6.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	u.Write([]byte("q"))
	u.SetReadDeadline(time.Now().Add(2 * time.Second))
	u.Read(make([]byte, 8))
	u.Close()
	if counts, ok := quietFor(w.ns, time.Second, 90*time.Second); !ok {
		t.Fatalf("still busy: %+v", counts)
	}
}
