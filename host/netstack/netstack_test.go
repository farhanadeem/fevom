package netstack

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

var (
	guestAddr = netip.MustParseAddr("198.19.1.2")
	guestMAC  = tcpip.LinkAddress([]byte{0x02, 0x00, 0xbe, 0x27, 0x00, 0x02})
)

type fakeHost struct {
	mu       sync.Mutex
	tcpDials []netip.AddrPort
	udpDials []netip.AddrPort
	refuse   map[netip.AddrPort]bool
	silent   map[netip.AddrPort]bool
	records  map[string][]dns.RR
	search   []string
	asked    []string
	pings    chan *fakePing
	udpConns chan *fakeUDPConn
}

func newFakeHost() *fakeHost {
	return &fakeHost{refuse: map[netip.AddrPort]bool{}, silent: map[netip.AddrPort]bool{}, records: map[string][]dns.RR{}, pings: make(chan *fakePing, 8), udpConns: make(chan *fakeUDPConn, 8)}
}

func echoConn() net.Conn {
	a, b := net.Pipe()
	go func() {
		io.Copy(b, b)
		b.Close()
	}()
	return a
}

func (h *fakeHost) DialTCP(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	h.mu.Lock()
	h.tcpDials = append(h.tcpDials, addr)
	refuse := h.refuse[addr]
	h.mu.Unlock()
	if refuse {
		return nil, errors.New("connection refused")
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer l.Close()
	client, err := net.Dial("tcp4", l.Addr().String())
	if err != nil {
		return nil, err
	}
	server, err := l.Accept()
	if err != nil {
		client.Close()
		return nil, err
	}
	go func() {
		defer server.Close()
		io.Copy(server, server)
		server.Write([]byte("<eof seen>"))
	}()
	return client, nil
}

func (h *fakeHost) DialUDP(ctx context.Context, addr netip.AddrPort) (UDPConn, error) {
	h.mu.Lock()
	h.udpDials = append(h.udpDials, addr)
	port := uint16(40000 + len(h.udpDials))
	silent := h.silent[addr]
	h.mu.Unlock()
	far := echoConn()
	if silent {
		var sink net.Conn
		far, sink = net.Pipe()
		go io.Copy(io.Discard, sink)
	}
	c := &fakeUDPConn{Conn: far, port: port, ttls: make(chan int, 16), softErrors: make(chan error, 4)}
	select {
	case h.udpConns <- c:
	default:
	}
	return c, nil
}

type fakeUDPConn struct {
	net.Conn
	port       uint16
	ttls       chan int
	softErrors chan error
	once       sync.Once
	pending    chan []byte
}

func (c *fakeUDPConn) Read(b []byte) (int, error) {
	c.once.Do(func() {
		c.pending = make(chan []byte, 16)
		go func() {
			buf := make([]byte, 65535)
			for {
				n, err := c.Conn.Read(buf)
				if err != nil {
					close(c.pending)
					return
				}
				c.pending <- append([]byte{}, buf[:n]...)
			}
		}()
	})
	select {
	case err := <-c.softErrors:
		return 0, err
	case data, ok := <-c.pending:
		if !ok {
			return 0, net.ErrClosed
		}
		return copy(b, data), nil
	}
}

func (c *fakeUDPConn) SetTTL(ttl int) error {
	c.ttls <- ttl
	return nil
}

func (c *fakeUDPConn) LocalPort() uint16 { return c.port }

func (h *fakeHost) Resolve(ctx context.Context, name string, qtype uint16) ([]dns.RR, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked = append(h.asked, name)
	if name == "broken.example." {
		return nil, errors.New("resolver unavailable")
	}
	var out []dns.RR
	for _, rr := range h.records[name] {
		if rr.Header().Rrtype == qtype || rr.Header().Rrtype == dns.TypeCNAME {
			out = append(out, rr)
		}
	}
	return out, nil
}

func (h *fakeHost) SearchDomains() []string { return h.search }

func (h *fakeHost) add(t *testing.T, records ...string) {
	t.Helper()
	for _, text := range records {
		rr, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		h.records[rr.Header().Name] = append(h.records[rr.Header().Name], rr)
	}
}

type fakePing struct {
	sent     chan pingSent
	replies  chan pingReply
	closed   chan struct{}
	once     sync.Once
	mu       sync.Mutex
	deadline time.Time
}

type pingSent struct {
	dst        netip.Addr
	ident, seq uint16
	payload    []byte
	ttl        int
}

type pingReply struct {
	from netip.Addr
	msg  []byte
}

func (h *fakeHost) ListenICMP() (PingConn, error) {
	p := &fakePing{sent: make(chan pingSent, 16), replies: make(chan pingReply, 16), closed: make(chan struct{})}
	h.pings <- p
	return p, nil
}

func (p *fakePing) Send(dst netip.Addr, ident, seq uint16, payload []byte, ttl int) error {
	if dst == netip.MustParseAddr("203.0.113.99") {
		return errors.New("no route to host")
	}
	p.sent <- pingSent{dst, ident, seq, payload, ttl}
	return nil
}

func (p *fakePing) Recv(buf []byte) (int, netip.Addr, error) {
	p.mu.Lock()
	deadline := p.deadline
	p.mu.Unlock()
	var timeout <-chan time.Time
	if !deadline.IsZero() {
		timeout = time.After(time.Until(deadline))
	}
	select {
	case r := <-p.replies:
		return copy(buf, r.msg), r.from, nil
	case <-p.closed:
		return 0, netip.Addr{}, net.ErrClosed
	case <-timeout:
		return 0, netip.Addr{}, context.DeadlineExceeded
	}
}

func (p *fakePing) SetReadDeadline(t time.Time) error {
	p.mu.Lock()
	p.deadline = t
	p.mu.Unlock()
	return nil
}

func (p *fakePing) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

type world struct {
	host  *fakeHost
	ns    *Stack
	guest *stack.Stack
}

func newWorld(t *testing.T) *world {
	t.Helper()
	return newWorldWith(t, DefaultConfig)
}

func newWorldWith(t *testing.T, cfg Config) *world {
	t.Helper()
	hostEnd, guestEnd := socketPair(t)
	h := newFakeHost()
	ns, err := New(hostEnd, Options{Host: h, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	g := newGuestStack(t, guestEnd, false)
	t.Cleanup(func() {
		ns.Close()
		g.Close()
	})
	return &world{host: h, ns: ns, guest: g}
}

func newGuestStack(t testing.TB, guestEnd *vmmEnd, guestOffload bool) *stack.Stack {
	t.Helper()
	g := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	ep := guestEnd.endpoint()
	ep.offload.Store(guestOffload)
	ep.announced.Store(true)
	if err := g.CreateNIC(1, ethernet.New(ep)); err != nil {
		t.Fatal(err)
	}
	ep.start(nil)
	addr := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: toAddr(guestAddr), PrefixLen: 24}}
	if err := g.AddProtocolAddress(1, addr, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	subnet, _ := tcpip.NewSubnet(toAddr(DefaultConfig.Subnet.Addr()), tcpip.MaskFromBytes([]byte{255, 255, 255, 0}))
	g.SetRouteTable([]tcpip.Route{
		{Destination: subnet, NIC: 1},
		{Destination: header.IPv4EmptySubnet, Gateway: toAddr(DefaultConfig.Gateway), NIC: 1},
	})
	return g
}

func (w *world) dialTCP(t testing.TB, dst string) (*gonet.TCPConn, error) {
	t.Helper()
	ap := netip.MustParseAddrPort(dst)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return gonet.DialContextTCP(ctx, w.guest, tcpip.FullAddress{NIC: 1, Addr: toAddr(ap.Addr()), Port: ap.Port()}, ipv4.ProtocolNumber)
}

func TestTCPIsForwardedToTheHostAndBack(t *testing.T) {
	w := newWorld(t)
	c, err := w.dialTCP(t, "93.184.215.14:80")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))

	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i * 31)
	}
	go func() {
		c.Write(payload)
		c.CloseWrite()
	}()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, payload...), "<eof seen>"...)
	if !bytes.Equal(got, want) {
		t.Fatalf("got %d bytes back, want %d (the echo and the host's goodbye after our half-close)", len(got), len(want))
	}
	if dials := w.host.tcpDials; len(dials) != 1 || dials[0] != netip.MustParseAddrPort("93.184.215.14:80") {
		t.Fatalf("host dialled %v", dials)
	}
}

func TestTCPToARefusingDestinationIsRefused(t *testing.T) {
	w := newWorld(t)
	dst := netip.MustParseAddrPort("203.0.113.5:81")
	w.host.refuse[dst] = true
	_, err := w.dialTCP(t, dst.String())
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("got %v, want connection refused", err)
	}
}

func TestDestinationsTheGuestMayNotReachAreNeverDialled(t *testing.T) {
	w := newWorld(t)
	for _, dst := range []string{"127.0.0.1:22", "169.254.169.254:80", "198.19.1.77:80"} {
		if _, err := w.dialTCP(t, dst); err == nil {
			t.Errorf("connection to %s succeeded", dst)
		}
	}
	for _, dst := range []string{"127.0.0.1:53", "255.255.255.255:67", "224.0.0.251:5353"} {
		ap := netip.MustParseAddrPort(dst)
		c, err := gonet.DialUDP(w.guest, nil, &tcpip.FullAddress{NIC: 1, Addr: toAddr(ap.Addr()), Port: ap.Port()}, ipv4.ProtocolNumber)
		if err != nil {
			continue
		}
		c.Write([]byte("x"))
		c.Close()
	}
	time.Sleep(50 * time.Millisecond)
	w.host.mu.Lock()
	defer w.host.mu.Unlock()
	if len(w.host.tcpDials) != 0 || len(w.host.udpDials) != 0 {
		t.Fatalf("host dialled tcp %v udp %v", w.host.tcpDials, w.host.udpDials)
	}
}

func TestUDPIsForwardedToTheHostAndBack(t *testing.T) {
	w := newWorld(t)
	dst := netip.MustParseAddrPort("8.8.4.4:9999")
	c, err := gonet.DialUDP(w.guest, nil, &tcpip.FullAddress{NIC: 1, Addr: toAddr(dst.Addr()), Port: dst.Port()}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	for _, msg := range []string{"first datagram", "second", strings.Repeat("big", 400)} {
		if _, err := c.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 2000)
		n, err := c.Read(buf)
		if err != nil || string(buf[:n]) != msg {
			t.Fatalf("sent %d bytes, got %d back, %v", len(msg), n, err)
		}
	}
	if dials := w.host.udpDials; len(dials) != 1 || dials[0] != dst {
		t.Fatalf("host dialled %v", dials)
	}
}

func (w *world) query(t *testing.T, network, name string, qtype uint16) *dns.Msg {
	t.Helper()
	gw := tcpip.FullAddress{NIC: 1, Addr: toAddr(DefaultConfig.Gateway), Port: 53}
	var conn net.Conn
	var err error
	if network == "tcp" {
		conn, err = gonet.DialTCP(w.guest, gw, ipv4.ProtocolNumber)
	} else {
		conn, err = gonet.DialUDP(w.guest, nil, &gw, ipv4.ProtocolNumber)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	dc := &dns.Conn{Conn: conn}
	if err := dc.WriteMsg(m); err != nil {
		t.Fatal(err)
	}
	resp, err := dc.ReadMsg()
	if err != nil {
		t.Fatalf("%s query for %s: %v", network, name, err)
	}
	return resp
}

func answers(m *dns.Msg) string {
	var parts []string
	for _, rr := range m.Answer {
		parts = append(parts, strings.Join(strings.Fields(rr.String()), " "))
	}
	return strings.Join(parts, " | ")
}

func TestDNSOnTheGatewayAnswersFromTheHostResolver(t *testing.T) {
	w := newWorld(t)
	w.host.add(t,
		"example.com. 300 IN A 93.184.215.14",
		"example.com. 300 IN AAAA 2606:2800:21f:cb07:6820:80da:af6b:8b2c",
		"www.example.com. 60 IN CNAME example.com.",
		"example.com. 300 IN TXT \"v=spf1 -all\"",
	)
	for _, network := range []string{"udp", "tcp"} {
		r := w.query(t, network, "example.com.", dns.TypeA)
		if r.Rcode != dns.RcodeSuccess || answers(r) != "example.com. 300 IN A 93.184.215.14" || !r.RecursionAvailable {
			t.Fatalf("%s A: rcode %d, %q", network, r.Rcode, answers(r))
		}
	}
	if r := w.query(t, "udp", "example.com.", dns.TypeAAAA); !strings.Contains(answers(r), "2606:2800") {
		t.Fatalf("AAAA: %q", answers(r))
	}
	if r := w.query(t, "udp", "example.com.", dns.TypeTXT); !strings.Contains(answers(r), "v=spf1") {
		t.Fatalf("TXT: %q", answers(r))
	}
	if r := w.query(t, "udp", "example.com.", dns.TypeMX); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
		t.Fatalf("MX: rcode %d, %q", r.Rcode, answers(r))
	}
	if r := w.query(t, "udp", "broken.example.", dns.TypeA); r.Rcode != dns.RcodeServerFailure {
		t.Fatalf("resolver failure: rcode %d", r.Rcode)
	}
}

func TestDNSTriesTheMacsSearchDomainsForBareNames(t *testing.T) {
	w := newWorld(t)
	w.host.search = []string{"lab.example", "corp.example"}
	w.host.add(t, "printer.corp.example. 120 IN A 10.1.2.3", "a.b.corp.example. 120 IN A 10.9.9.9")

	r := w.query(t, "udp", "printer.", dns.TypeA)
	want := "printer. 0 IN CNAME printer.corp.example. | printer.corp.example. 120 IN A 10.1.2.3"
	if answers(r) != want {
		t.Fatalf("got %q", answers(r))
	}
	if got := strings.Join(w.host.asked, " "); got != "printer. printer.lab.example. printer.corp.example." {
		t.Fatalf("asked %q", got)
	}
	w.host.asked = nil
	if r := w.query(t, "udp", "a.b.", dns.TypeA); len(r.Answer) != 0 || len(w.host.asked) != 2 || w.host.asked[0] != "a.b." || w.host.asked[1] != "a.b." {
		t.Fatalf("a.b.: %q, asked %v", answers(r), w.host.asked)
	}
}

func TestDNSRequestsItDoesNotServe(t *testing.T) {
	h := newFakeHost()
	ask := func(mutate func(*dns.Msg)) int {
		m := new(dns.Msg)
		m.SetQuestion("example.com.", dns.TypeA)
		mutate(m)
		return answerDNS(context.Background(), h, m).Rcode
	}
	cases := map[string]struct {
		mutate func(*dns.Msg)
		want   int
	}{
		"ANY":           {func(m *dns.Msg) { m.Question[0].Qtype = dns.TypeANY }, dns.RcodeNotImplemented},
		"AXFR":          {func(m *dns.Msg) { m.Question[0].Qtype = dns.TypeAXFR }, dns.RcodeNotImplemented},
		"CHAOS class":   {func(m *dns.Msg) { m.Question[0].Qclass = dns.ClassCHAOS }, dns.RcodeNotImplemented},
		"update opcode": {func(m *dns.Msg) { m.Opcode = dns.OpcodeUpdate }, dns.RcodeNotImplemented},
		"no question":   {func(m *dns.Msg) { m.Question = nil }, dns.RcodeFormatError},
		"two questions": {func(m *dns.Msg) { m.Question = append(m.Question, m.Question[0]) }, dns.RcodeFormatError},
	}
	for name, tc := range cases {
		if got := ask(tc.mutate); got != tc.want {
			t.Errorf("%s: rcode %d, want %d", name, got, tc.want)
		}
	}
	if len(h.asked) != 0 {
		t.Fatalf("the resolver was asked %v for requests that should not reach it", h.asked)
	}
}

func TestALargeDNSAnswerIsTruncatedOverUDPAndWholeOverTCP(t *testing.T) {
	w := newWorld(t)
	for i := range 60 {
		w.host.add(t, "many.example. 60 IN TXT \""+strings.Repeat("x", 40)+string(rune('a'+i%26))+"\"")
	}
	r := w.query(t, "udp", "many.example.", dns.TypeTXT)
	r.Compress = true
	if !r.Truncated || r.Len() > dnsUDPSize || len(r.Answer) == 0 || len(r.Answer) >= 60 {
		t.Fatalf("udp: truncated=%v, %d bytes, %d answers", r.Truncated, r.Len(), len(r.Answer))
	}
	if r := w.query(t, "tcp", "many.example.", dns.TypeTXT); r.Truncated || len(r.Answer) != 60 {
		t.Fatalf("tcp: truncated=%v, %d answers", r.Truncated, len(r.Answer))
	}
}

type wire struct {
	t    *testing.T
	conn *vmmEnd
	host *fakeHost
}

func newWire(t *testing.T) *wire {
	t.Helper()
	hostEnd, guestEnd := socketPair(t)
	h := newFakeHost()
	ns, err := New(hostEnd, Options{Host: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		guestEnd.Close()
		ns.Close()
	})
	return &wire{t: t, conn: guestEnd, host: h}
}

func (w *wire) sendEcho(dst string, ttl uint8, ident, seq uint16, payload string) {
	w.t.Helper()
	icmpMsg := make([]byte, header.ICMPv4MinimumSize+len(payload))
	m := header.ICMPv4(icmpMsg)
	m.SetType(header.ICMPv4Echo)
	m.SetIdent(ident)
	m.SetSequence(seq)
	copy(icmpMsg[header.ICMPv4MinimumSize:], payload)
	m.SetChecksum(^checksum.Checksum(icmpMsg, 0))

	frame := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+len(icmpMsg))
	header.Ethernet(frame).Encode(&header.EthernetFields{SrcAddr: guestMAC, DstAddr: GatewayMAC, Type: ipv4.ProtocolNumber})
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + len(icmpMsg)), TTL: ttl, ID: 0x1234,
		Protocol: uint8(header.ICMPv4ProtocolNumber), SrcAddr: toAddr(guestAddr), DstAddr: toAddr(netip.MustParseAddr(dst)),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	copy(frame[header.EthernetMinimumSize+header.IPv4MinimumSize:], icmpMsg)

	w.conn.sendFrame(frame)
}

type icmpSeen struct {
	src, dst netip.Addr
	typ      header.ICMPv4Type
	code     header.ICMPv4Code
	ident    uint16
	seq      uint16
	body     []byte
	ipOK     bool
	icmpOK   bool
}

func (w *wire) nextICMP() icmpSeen {
	w.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		msg, ok := w.conn.recv(deadline)
		if !ok {
			w.t.Fatal("no ICMP packet from the stack")
		}
		frame := msg[netHdrLen:]
		eth := header.Ethernet(frame)
		if eth.Type() == arp.ProtocolNumber {
			w.answerARP(frame)
			continue
		}
		if eth.Type() != ipv4.ProtocolNumber || eth.DestinationAddress() != guestMAC {
			continue
		}
		ip := header.IPv4(frame[header.EthernetMinimumSize:])
		if ip.Protocol() != uint8(header.ICMPv4ProtocolNumber) {
			continue
		}
		m := header.ICMPv4(ip.Payload())
		return icmpSeen{
			src: fromAddr(ip.SourceAddress()), dst: fromAddr(ip.DestinationAddress()),
			typ: m.Type(), code: m.Code(), ident: m.Ident(), seq: m.Sequence(), body: m.Payload(),
			ipOK:   ip.IsChecksumValid(),
			icmpOK: checksum.Checksum(ip.Payload(), 0) == 0xffff,
		}
	}
}

func (w *wire) answerARP(frame []byte) {
	req := header.ARP(frame[header.EthernetMinimumSize:])
	if len(req) < header.ARPSize || req.Op() != header.ARPRequest || fromAddr(tcpip.AddrFrom4Slice(req.ProtocolAddressTarget())) != guestAddr {
		return
	}
	reply := make([]byte, header.EthernetMinimumSize+header.ARPSize)
	header.Ethernet(reply).Encode(&header.EthernetFields{SrcAddr: guestMAC, DstAddr: GatewayMAC, Type: arp.ProtocolNumber})
	a := header.ARP(reply[header.EthernetMinimumSize:])
	a.SetIPv4OverEthernet()
	a.SetOp(header.ARPReply)
	copy(a.HardwareAddressSender(), guestMAC)
	copy(a.ProtocolAddressSender(), req.ProtocolAddressTarget())
	copy(a.HardwareAddressTarget(), req.HardwareAddressSender())
	copy(a.ProtocolAddressTarget(), req.ProtocolAddressSender())
	w.conn.sendFrame(reply)
}

func (w *wire) expectNoICMP() {
	w.t.Helper()
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		msg, ok := w.conn.recv(deadline)
		if !ok {
			return
		}
		ip := header.IPv4(msg[netHdrLen+header.EthernetMinimumSize:])
		if header.Ethernet(msg[netHdrLen:]).Type() == ipv4.ProtocolNumber && ip.Protocol() == uint8(header.ICMPv4ProtocolNumber) {
			w.t.Fatalf("unexpected ICMP packet from %v", fromAddr(ip.SourceAddress()))
		}
	}
}

func (w *wire) hostPing() *fakePing {
	w.t.Helper()
	select {
	case p := <-w.host.pings:
		return p
	case <-time.After(5 * time.Second):
		w.t.Fatal("the stack opened no host ICMP socket")
		return nil
	}
}

func recvSent(t *testing.T, p *fakePing) pingSent {
	t.Helper()
	select {
	case s := <-p.sent:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("the stack sent no echo request to the host")
		return pingSent{}
	}
}

func echoReplyMsg(ident, seq uint16, payload string) []byte {
	msg := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(msg[4:], ident)
	binary.BigEndian.PutUint16(msg[6:], seq)
	copy(msg[8:], payload)
	return msg
}

func TestPingIsForwardedWithOneHopFewerAndTheReplyComesBack(t *testing.T) {
	w := newWire(t)
	target := netip.MustParseAddr("1.1.1.1")
	w.sendEcho("1.1.1.1", 64, 700, 3, "hello")
	p := w.hostPing()
	s := recvSent(t, p)
	if s.dst != target || s.ident != 700 || s.seq != 3 || string(s.payload) != "hello" || s.ttl != 63 {
		t.Fatalf("host sent %+v", s)
	}
	p.replies <- pingReply{target, echoReplyMsg(701, 3, "hello")}
	p.replies <- pingReply{netip.MustParseAddr("9.9.9.9"), echoReplyMsg(700, 3, "hello")}
	p.replies <- pingReply{target, echoReplyMsg(700, 3, "hello")}
	got := w.nextICMP()
	if got.typ != header.ICMPv4EchoReply || got.src != target || got.dst != guestAddr || got.ident != 700 || got.seq != 3 || string(got.body) != "hello" {
		t.Fatalf("guest got %+v", got)
	}
	if !got.ipOK || !got.icmpOK {
		t.Fatalf("bad checksums: ip %v icmp %v", got.ipOK, got.icmpOK)
	}
	w.expectNoICMP()

	w.sendEcho("1.1.1.1", 64, 700, 4, "again")
	if s := recvSent(t, p); s.seq != 4 {
		t.Fatalf("host sent %+v", s)
	}
	select {
	case <-w.host.pings:
		t.Fatal("a second host socket was opened for the same ping")
	default:
	}
}

func TestThePacketThatExpiresAtTheGatewayIsAnsweredByTheGateway(t *testing.T) {
	w := newWire(t)
	w.sendEcho("1.1.1.1", 1, 55, 9, "probe")
	got := w.nextICMP()
	if got.typ != header.ICMPv4TimeExceeded || got.src != DefaultConfig.Gateway || got.dst != guestAddr {
		t.Fatalf("guest got %+v", got)
	}
	quoted := header.IPv4(got.body)
	inner := header.ICMPv4(got.body[header.IPv4MinimumSize:])
	if len(got.body) != icmpQuote || fromAddr(quoted.SourceAddress()) != guestAddr || quoted.TTL() != 1 || quoted.ID() != 0x1234 || inner.Ident() != 55 || inner.Sequence() != 9 {
		t.Fatalf("quote is wrong: % x", got.body)
	}
	select {
	case <-w.host.pings:
		t.Fatal("an expired packet reached the host")
	default:
	}
}

func TestARoutersTimeExceededReachesTheGuestQuotingTheGuestsPacket(t *testing.T) {
	w := newWire(t)
	w.sendEcho("1.1.1.1", 3, 77, 21, "trace")
	p := w.hostPing()
	if s := recvSent(t, p); s.ttl != 2 {
		t.Fatalf("host ttl %d", s.ttl)
	}
	macPacket := make([]byte, 28)
	macIP := header.IPv4(macPacket)
	macIP.Encode(&header.IPv4Fields{TotalLength: 33, TTL: 1, Protocol: 1,
		SrcAddr: toAddr(netip.MustParseAddr("172.16.30.211")), DstAddr: toAddr(netip.MustParseAddr("1.1.1.1"))})
	copy(macPacket[20:], echoReplyMsg(77, 21, "")[:8])
	macPacket[20] = byte(header.ICMPv4Echo)
	router := netip.MustParseAddr("108.31.110.1")
	errMsg := append([]byte{byte(header.ICMPv4TimeExceeded), 0, 0, 0, 0, 0, 0, 0}, macPacket...)
	p.replies <- pingReply{router, errMsg}

	got := w.nextICMP()
	if got.typ != header.ICMPv4TimeExceeded || got.src != router || got.dst != guestAddr || !got.ipOK || !got.icmpOK {
		t.Fatalf("guest got %+v", got)
	}
	quoted := header.IPv4(got.body)
	inner := header.ICMPv4(got.body[header.IPv4MinimumSize:])
	if fromAddr(quoted.SourceAddress()) != guestAddr || quoted.TTL() != 3 || inner.Ident() != 77 || inner.Sequence() != 21 {
		t.Fatalf("the error does not quote the guest's packet: % x", got.body)
	}

	for _, bad := range [][]byte{
		errMsg[:20],
		append([]byte{byte(header.ICMPv4TimeExceeded), 0, 0, 0, 0, 0, 0, 0}, func() []byte {
			b := append([]byte{}, macPacket...)
			binary.BigEndian.PutUint16(b[26:], 99)
			return b
		}()...),
		append([]byte{byte(header.ICMPv4TimeExceeded), 0, 0, 0, 0, 0, 0, 0}, func() []byte {
			b := append([]byte{}, macPacket...)
			b[9] = 17
			return b
		}()...),
	} {
		p.replies <- pingReply{router, bad}
	}
	w.expectNoICMP()
}

func TestPingsTheStackDoesNotForward(t *testing.T) {
	w := newWire(t)
	w.sendEcho(DefaultConfig.Gateway.String(), 64, 5, 1, "gw")
	got := w.nextICMP()
	if got.typ != header.ICMPv4EchoReply || got.src != DefaultConfig.Gateway || string(got.body) != "gw" {
		t.Fatalf("guest got %+v", got)
	}
	w.sendEcho("127.0.0.1", 64, 5, 2, "lo")
	w.expectNoICMP()
	select {
	case <-w.host.pings:
		t.Fatal("a ping the guest may not send reached the host")
	default:
	}
	w.sendEcho("203.0.113.99", 64, 6, 1, "nope")
	got = w.nextICMP()
	if got.typ != header.ICMPv4DstUnreachable || got.src != DefaultConfig.Gateway {
		t.Fatalf("guest got %+v", got)
	}
}

func TestAMalformedLengthFromTheVMMEndsTheLink(t *testing.T) {
	for _, size := range []int{0, 11, 25, maxMessage + 1} {
		hostEnd, guestEnd := socketPair(t)
		ns, err := New(hostEnd, Options{Host: newFakeHost()})
		if err != nil {
			t.Fatal(err)
		}
		guestEnd.send(make([]byte, size))
		select {
		case <-ns.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("link still up after a message of %d bytes", size)
		}
		guestEnd.Close()
	}
}

func TestStripIPv4Header(t *testing.T) {
	icmpMsg := echoReplyMsg(1, 2, "data")
	withHeader := append(make([]byte, 20), icmpMsg...)
	withHeader[0] = 0x45
	buf := append([]byte{}, withHeader...)
	if n := stripIPv4Header(buf, len(buf)); n != len(icmpMsg) || !bytes.Equal(buf[:n], icmpMsg) {
		t.Fatalf("header not stripped: %d bytes", n)
	}
	withOptions := append(make([]byte, 24), icmpMsg...)
	withOptions[0] = 0x46
	buf = append([]byte{}, withOptions...)
	if n := stripIPv4Header(buf, len(buf)); n != len(icmpMsg) || !bytes.Equal(buf[:n], icmpMsg) {
		t.Fatalf("header with options not stripped: %d bytes", n)
	}
	for _, b := range [][]byte{icmpMsg, {0x45, 0, 0}, append([]byte{0x4f}, make([]byte, 30)...)} {
		buf = append([]byte{}, b...)
		if n := stripIPv4Header(buf, len(buf)); n != len(b) {
			t.Fatalf("% x: changed to %d bytes", b, n)
		}
	}
}

func (w *wire) sendUDP(dst string, ttl uint8, srcPort uint16, payload string) {
	w.t.Helper()
	ap := netip.MustParseAddrPort(dst)
	udpLen := header.UDPMinimumSize + len(payload)
	frame := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+udpLen)
	header.Ethernet(frame).Encode(&header.EthernetFields{SrcAddr: guestMAC, DstAddr: GatewayMAC, Type: ipv4.ProtocolNumber})
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + udpLen), TTL: ttl,
		Protocol: uint8(header.UDPProtocolNumber), SrcAddr: toAddr(guestAddr), DstAddr: toAddr(ap.Addr()),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	u := header.UDP(frame[header.EthernetMinimumSize+header.IPv4MinimumSize:])
	u.Encode(&header.UDPFields{SrcPort: srcPort, DstPort: ap.Port(), Length: uint16(udpLen)})
	copy(u.Payload(), payload)
	w.conn.sendFrame(frame)
}

func (w *wire) hostUDP() *fakeUDPConn {
	w.t.Helper()
	select {
	case c := <-w.host.udpConns:
		return c
	case <-time.After(5 * time.Second):
		w.t.Fatal("the stack opened no host UDP socket")
		return nil
	}
}

func recvTTL(t *testing.T, c *fakeUDPConn) int {
	t.Helper()
	select {
	case ttl := <-c.ttls:
		return ttl
	case <-time.After(5 * time.Second):
		t.Fatal("no hop limit was set on the host socket")
		return 0
	}
}

func udpError(typ header.ICMPv4Type, code header.ICMPv4Code, hostPort uint16, dst string) []byte {
	ap := netip.MustParseAddrPort(dst)
	quoted := make([]byte, 28)
	header.IPv4(quoted).Encode(&header.IPv4Fields{TotalLength: 40, TTL: 1, Protocol: 17,
		SrcAddr: toAddr(netip.MustParseAddr("172.16.30.211")), DstAddr: toAddr(ap.Addr())})
	binary.BigEndian.PutUint16(quoted[20:], hostPort)
	binary.BigEndian.PutUint16(quoted[22:], ap.Port())
	return append([]byte{byte(typ), byte(code), 0, 0, 0, 0, 0, 0}, quoted...)
}

func quotedUDP(body []byte) (src, dst netip.AddrPort, ttl uint8, proto uint8) {
	ip := header.IPv4(body)
	u := body[header.IPv4MinimumSize:]
	return netip.AddrPortFrom(fromAddr(ip.SourceAddress()), binary.BigEndian.Uint16(u[0:])),
		netip.AddrPortFrom(fromAddr(ip.DestinationAddress()), binary.BigEndian.Uint16(u[2:])),
		ip.TTL(), ip.Protocol()
}

func TestUDPProbesCarryTheirHopLimitAndRouterErrorsComeBack(t *testing.T) {
	w := newWire(t)
	const target = "1.1.1.1:33436"
	router := netip.MustParseAddr("108.31.110.1")

	w.sendUDP(target, 3, 45000, "probe")
	c := w.hostUDP()
	if ttl := recvTTL(t, c); ttl != 2 {
		t.Fatalf("host hop limit %d, want one fewer than the guest's 3", ttl)
	}
	errs := w.hostPing()

	errs.replies <- pingReply{router, udpError(header.ICMPv4TimeExceeded, 0, c.port+7, target)}
	errs.replies <- pingReply{router, udpError(header.ICMPv4TimeExceeded, 0, c.port, "1.1.1.1:9")}
	errs.replies <- pingReply{router, udpError(header.ICMPv4TimeExceeded, 0, c.port, "9.9.9.9:33436")}
	errs.replies <- pingReply{router, udpError(header.ICMPv4TimeExceeded, 0, c.port, target)}

	got := w.nextICMP()
	if got.typ != header.ICMPv4TimeExceeded || got.src != router || got.dst != guestAddr || !got.ipOK || !got.icmpOK {
		t.Fatalf("guest got %+v", got)
	}
	src, dst, ttl, proto := quotedUDP(got.body)
	if src != netip.AddrPortFrom(guestAddr, 45000) || dst != netip.MustParseAddrPort(target) || ttl != 3 || proto != 17 {
		t.Fatalf("the error quotes %v -> %v ttl %d proto %d, not the guest's datagram", src, dst, ttl, proto)
	}
	w.expectNoICMP()

	errs.replies <- pingReply{netip.MustParseAddr("1.1.1.1"), udpError(header.ICMPv4DstUnreachable, header.ICMPv4PortUnreachable, c.port, target)}
	got = w.nextICMP()
	if got.typ != header.ICMPv4DstUnreachable || got.code != header.ICMPv4PortUnreachable || got.src != netip.MustParseAddr("1.1.1.1") {
		t.Fatalf("guest got %+v", got)
	}
}

func TestAUDPDatagramThatExpiresAtTheGatewayIsAnsweredByTheGateway(t *testing.T) {
	w := newWire(t)
	w.sendUDP("1.1.1.1:33434", 1, 45001, "probe")
	got := w.nextICMP()
	if got.typ != header.ICMPv4TimeExceeded || got.src != DefaultConfig.Gateway {
		t.Fatalf("guest got %+v", got)
	}
	src, dst, ttl, _ := quotedUDP(got.body)
	if src.Port() != 45001 || dst != netip.MustParseAddrPort("1.1.1.1:33434") || ttl != 1 {
		t.Fatalf("quote: %v -> %v ttl %d", src, dst, ttl)
	}
	c := w.hostUDP()
	select {
	case ttl := <-c.ttls:
		t.Fatalf("an expired datagram was sent on with hop limit %d", ttl)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestOrdinaryUDPDoesNotListenForICMP(t *testing.T) {
	w := newWire(t)
	w.sendUDP("8.8.8.8:53", 64, 45002, "query")
	c := w.hostUDP()
	if ttl := recvTTL(t, c); ttl != 63 {
		t.Fatalf("host hop limit %d", ttl)
	}
	select {
	case <-w.host.pings:
		t.Fatal("an ICMP socket was opened for a flow that is not probing")
	case <-time.After(50 * time.Millisecond):
	}
}

func (w *wire) nextUDP() string {
	w.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		msg, ok := w.conn.recv(deadline)
		if !ok {
			w.t.Fatal("no UDP datagram from the stack")
		}
		frame := msg[netHdrLen:]
		eth := header.Ethernet(frame)
		if eth.Type() == arp.ProtocolNumber {
			w.answerARP(frame)
			continue
		}
		ip := header.IPv4(frame[header.EthernetMinimumSize:])
		if eth.Type() == ipv4.ProtocolNumber && ip.Protocol() == uint8(header.UDPProtocolNumber) {
			return string(header.UDP(ip.Payload()).Payload())
		}
	}
}

func TestAnICMPErrorReportedOnTheHostSocketDoesNotEndTheFlow(t *testing.T) {
	w := newWire(t)
	const target = "1.1.1.1:33437"
	w.sendUDP(target, 2, 45010, "probe")
	c := w.hostUDP()
	recvTTL(t, c)
	errs := w.hostPing()
	if got := w.nextUDP(); got != "probe" {
		t.Fatalf("echo %q", got)
	}

	c.softErrors <- syscall.EHOSTUNREACH
	time.Sleep(20 * time.Millisecond)
	select {
	case <-errs.closed:
		t.Fatal("the ICMP listener was closed by a soft error")
	default:
	}
	router := netip.MustParseAddr("172.16.30.1")
	errs.replies <- pingReply{router, udpError(header.ICMPv4TimeExceeded, 0, c.port, target)}
	if got := w.nextICMP(); got.typ != header.ICMPv4TimeExceeded || got.src != router {
		t.Fatalf("guest got %+v", got)
	}
	w.sendUDP(target, 2, 45010, "again")
	if got := w.nextUDP(); got != "again" {
		t.Fatalf("after a soft error, echo %q", got)
	}

	c.softErrors <- syscall.ECONNREFUSED
	w.expectNoICMP()
}

func TestPortUnreachableReachesAnOrdinaryFlow(t *testing.T) {
	w := newWire(t)
	w.sendUDP("9.9.9.9:4444", 64, 45011, "hello")
	c := w.hostUDP()
	recvTTL(t, c)
	c.softErrors <- syscall.ECONNREFUSED
	got := w.nextICMP()
	if got.typ != header.ICMPv4DstUnreachable || got.code != header.ICMPv4PortUnreachable || got.src != netip.MustParseAddr("9.9.9.9") {
		t.Fatalf("guest got %+v", got)
	}
	if src, dst, _, _ := quotedUDP(got.body); src.Port() != 45011 || dst.Port() != 4444 {
		t.Fatalf("quote: %v -> %v", src, dst)
	}
}

func announce(t testing.TB, guestEnd *vmmEnd, accepts uint32) {
	t.Helper()
	msg := binary.LittleEndian.AppendUint32(nil, controlGuestAccepts)
	guestEnd.send(binary.LittleEndian.AppendUint32(msg, accepts))
}

type largest struct{ atomic.Int64 }

func (l *largest) watch(e *linkEndpoint) {
	e.onSend = func(n int) {
		for {
			old := l.Load()
			if int64(n) <= old || l.CompareAndSwap(old, int64(n)) {
				return
			}
		}
	}
}

func (l *largest) largestFrame() int { return int(l.Load()) }

func offloadWorld(t testing.TB, accepts uint32) (*world, *largest, *largest) {
	t.Helper()
	hostEnd, guestEnd := socketPair(t)
	toGuest, toHost := &largest{}, &largest{}
	announce(t, guestEnd, accepts)
	h := newFakeHost()
	ns, err := newWatched(hostEnd, Options{Host: h}, toGuest)
	if err != nil {
		t.Fatal(err)
	}
	offload := accepts == acceptsPartialCsum|acceptsTSO4
	guestEnd.sends = toHost
	g := newGuestStack(t, guestEnd, offload)
	t.Cleanup(func() {
		ns.Close()
		g.Close()
	})
	return &world{host: h, ns: ns, guest: g}, toGuest, toHost
}

func echoThrough(t testing.TB, w *world, size int) {
	t.Helper()
	c, err := w.dialTCP(t, "93.184.215.14:80")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i*131 + i>>9)
	}
	go func() {
		c.Write(payload)
		c.CloseWrite()
	}()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if want := append(append([]byte{}, payload...), "<eof seen>"...); !bytes.Equal(got, want) {
		t.Fatalf("got %d bytes back, want %d", len(got), len(want))
	}
}

func TestSegmentationOffloadCarriesLargeSegmentsBothWays(t *testing.T) {
	w, toGuest, toHost := offloadWorld(t, acceptsPartialCsum|acceptsTSO4)
	echoThrough(t, w, 4<<20)
	if toGuest.largestFrame() < 16<<10 || toHost.largestFrame() < 16<<10 {
		t.Fatalf("largest frame to the guest %d, to the host %d: offload is not in use", toGuest.largestFrame(), toHost.largestFrame())
	}
	if toGuest.largestFrame() > maxMessage-netHdrLen {
		t.Fatalf("a frame of %d bytes is more than the link carries", toGuest.largestFrame())
	}
}

func TestWithoutTheGuestsAgreementNothingLargerThanTheMTUIsSent(t *testing.T) {
	for _, accepts := range []uint32{0, acceptsPartialCsum, acceptsTSO4} {
		w, toGuest, _ := offloadWorld(t, accepts)
		echoThrough(t, w, 1<<20)
		if got := toGuest.largestFrame(); got > ipMTU+header.EthernetMinimumSize {
			t.Fatalf("accepts=%d: a frame of %d bytes was sent to a guest that cannot take it", accepts, got)
		}
	}
}

func TestALaterDriverWithFewerOffloadsIsRespected(t *testing.T) {
	hostEnd, guestEnd := socketPair(t)
	toGuest := &largest{}
	announce(t, guestEnd, acceptsPartialCsum|acceptsTSO4)
	h := newFakeHost()
	ns, err := newWatched(hostEnd, Options{Host: h}, toGuest)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	announce(t, guestEnd, 0)
	for !ns.link.announced.Load() || ns.link.offload.Load() {
		time.Sleep(time.Millisecond)
	}
	g := newGuestStack(t, guestEnd, false)
	defer g.Close()
	echoThrough(t, &world{host: h, ns: ns, guest: g}, 1<<20)
	if got := toGuest.largestFrame(); got > ipMTU+header.EthernetMinimumSize {
		t.Fatalf("a frame of %d bytes was sent after the guest stopped accepting them", got)
	}
}
