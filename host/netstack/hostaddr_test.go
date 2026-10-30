package netstack

import (
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

func hostAddrPort(port uint16) netip.AddrPort {
	return netip.AddrPortFrom(DefaultConfig.Host, port)
}

func TestRouteSendsOnlyTheHostAddressToTheMacsLoopback(t *testing.T) {
	c := DefaultConfig
	got, ok := c.route(netip.MustParseAddrPort("198.19.1.254:5432"))
	if !ok || got != netip.MustParseAddrPort("127.0.0.1:5432") {
		t.Fatalf("the host address: %v, %v", got, ok)
	}
	if got, ok := c.route(netip.MustParseAddrPort("[::ffff:198.19.1.254]:80")); !ok || got != netip.MustParseAddrPort("127.0.0.1:80") {
		t.Fatalf("the host address, mapped: %v, %v", got, ok)
	}
	if got, ok := c.route(netip.MustParseAddrPort("1.1.1.1:53")); !ok || got != netip.MustParseAddrPort("1.1.1.1:53") {
		t.Fatalf("an ordinary destination: %v, %v", got, ok)
	}
	for _, dst := range []string{"127.0.0.1:5432", "127.9.9.9:80", "198.19.1.1:80", "198.19.1.253:80", "[::1]:80"} {
		if got, ok := c.route(netip.MustParseAddrPort(dst)); ok {
			t.Errorf("%s is routed to %v", dst, got)
		}
	}
	c.Host = netip.Addr{}
	if got, ok := c.route(netip.MustParseAddrPort("198.19.1.254:5432")); ok {
		t.Fatalf("with no host address, 198.19.1.254 is routed to %v", got)
	}
}

func TestTheHostNamesResolveToTheHostAddress(t *testing.T) {
	w := newWorld(t)
	w.host.add(t, "example.com. 300 IN A 93.184.215.14", "host.docker.internal. 300 IN A 10.9.9.9")
	for _, network := range []string{"udp", "tcp"} {
		for _, name := range []string{"host.docker.internal.", "host.fev.internal.", "Host.Docker.INTERNAL."} {
			r := w.query(t, network, name, dns.TypeA)
			want := name + " 60 IN A 198.19.1.254"
			if r.Rcode != dns.RcodeSuccess || answers(r) != want {
				t.Fatalf("%s A %s: rcode %d, %q, want %q", network, name, r.Rcode, answers(r), want)
			}
		}
	}
	for _, qtype := range []uint16{dns.TypeAAAA, dns.TypeTXT, dns.TypeMX} {
		if r := w.query(t, "udp", "host.fev.internal.", qtype); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
			t.Fatalf("type %d: rcode %d, %q", qtype, r.Rcode, answers(r))
		}
	}
	if len(w.host.asked) != 0 {
		t.Fatalf("the Mac's resolver was asked %v", w.host.asked)
	}
	if r := w.query(t, "udp", "example.com.", dns.TypeA); answers(r) != "example.com. 300 IN A 93.184.215.14" {
		t.Fatalf("example.com: %q", answers(r))
	}
	for _, name := range []string{"x.host.docker.internal.", "host.docker.internal.example.com.", "docker.internal."} {
		if r := w.query(t, "udp", name, dns.TypeA); len(r.Answer) != 0 {
			t.Fatalf("%s: %q", name, answers(r))
		}
	}
	if got := strings.Join(w.host.asked, " "); got != "example.com. x.host.docker.internal. x.host.docker.internal. host.docker.internal.example.com. host.docker.internal.example.com. docker.internal. docker.internal." {
		t.Fatalf("the Mac's resolver was asked %q", got)
	}
}

func TestWithNoHostAddressTheNamesAreTheMacsToAnswer(t *testing.T) {
	cfg := DefaultConfig
	cfg.Host = netip.Addr{}
	w := newWorldWith(t, cfg)
	if r := w.query(t, "udp", "host.docker.internal.", dns.TypeA); r.Rcode != dns.RcodeNameError || len(r.Answer) != 0 {
		t.Fatalf("rcode %d, %q", r.Rcode, answers(r))
	}
	if len(w.host.asked) != 2 {
		t.Fatalf("the Mac's resolver was asked %v", w.host.asked)
	}
	if _, err := w.dialTCP(t, "198.19.1.254:80"); err == nil {
		t.Fatal("a connection to 198.19.1.254 succeeded with the mapping off")
	}
	if len(w.host.tcpDials) != 0 {
		t.Fatalf("host dialled %v", w.host.tcpDials)
	}
}

func TestTCPToTheHostAddressReachesTheMacsLoopback(t *testing.T) {
	w := newWorld(t)
	c, err := w.dialTCP(t, hostAddrPort(5432).String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("select 1")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := c.Read(buf); err != nil || string(buf[:n]) != "select 1" {
		t.Fatalf("read %q, %v", buf[:n], err)
	}
	for _, dst := range []string{"127.0.0.1:5432", "198.19.1.1:5432"} {
		if _, err := w.dialTCP(t, dst); err == nil {
			t.Errorf("connection to %s succeeded", dst)
		}
	}
	w.host.mu.Lock()
	defer w.host.mu.Unlock()
	if dials := w.host.tcpDials; len(dials) != 1 || dials[0] != netip.MustParseAddrPort("127.0.0.1:5432") {
		t.Fatalf("host dialled %v, want only 127.0.0.1:5432", dials)
	}
}

func TestARefusedPortOnTheMacsLoopbackLooksRefused(t *testing.T) {
	w := newWorld(t)
	w.host.refuse[netip.MustParseAddrPort("127.0.0.1:81")] = true
	if _, err := w.dialTCP(t, hostAddrPort(81).String()); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("got %v, want connection refused", err)
	}
}

func TestUDPToTheHostAddressReachesTheMacsLoopback(t *testing.T) {
	w := newWorld(t)
	dst := hostAddrPort(8125)
	c, err := gonet.DialUDP(w.guest, nil, &tcpip.FullAddress{NIC: 1, Addr: toAddr(dst.Addr()), Port: dst.Port()}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("metric:1|c")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := c.Read(buf); err != nil || string(buf[:n]) != "metric:1|c" {
		t.Fatalf("read %q, %v", buf[:n], err)
	}
	w.host.mu.Lock()
	defer w.host.mu.Unlock()
	if dials := w.host.udpDials; len(dials) != 1 || dials[0] != netip.MustParseAddrPort("127.0.0.1:8125") {
		t.Fatalf("host dialled %v, want only 127.0.0.1:8125", dials)
	}
}

func TestTheGatewaysDNSIsNotTheMacsPort53(t *testing.T) {
	w := newWorld(t)
	w.host.add(t, "example.com. 300 IN A 93.184.215.14")
	if r := w.query(t, "tcp", "example.com.", dns.TypeA); answers(r) != "example.com. 300 IN A 93.184.215.14" {
		t.Fatalf("gateway DNS: %q", answers(r))
	}
	if len(w.host.tcpDials) != 0 {
		t.Fatalf("a question to the gateway dialled %v on the Mac", w.host.tcpDials)
	}
	c, err := w.dialTCP(t, hostAddrPort(53).String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	w.host.mu.Lock()
	defer w.host.mu.Unlock()
	if dials := w.host.tcpDials; len(dials) != 1 || dials[0] != netip.MustParseAddrPort("127.0.0.1:53") {
		t.Fatalf("host dialled %v", dials)
	}
}

func TestIdleEndsAQuietFlowAndAReplyBuysItLonger(t *testing.T) {
	const unreplied, assured = 40 * time.Millisecond, 200 * time.Millisecond
	expired := func(i **Idle) (*atomic.Int32, time.Time) {
		var n atomic.Int32
		start := time.Now()
		*i = NewIdle(unreplied, assured, func() { n.Add(1) })
		return &n, start
	}
	wait := func(n *atomic.Int32) time.Time {
		deadline := time.Now().Add(2 * time.Second)
		for n.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		return time.Now()
	}

	var i *Idle
	n, start := expired(&i)
	time.Sleep(25 * time.Millisecond)
	i.Sent()
	if took := wait(n).Sub(start); n.Load() != 1 || took < 25*time.Millisecond+unreplied || took > assured {
		t.Fatalf("unreplied: expired %d times after %v", n.Load(), took)
	}

	n, start = expired(&i)
	i.Sent()
	i.Replied()
	if took := wait(n).Sub(start); n.Load() != 1 || took < assured {
		t.Fatalf("assured: expired %d times after %v", n.Load(), took)
	}
	time.Sleep(2 * unreplied)
	if n.Load() != 1 {
		t.Fatalf("expired %d times", n.Load())
	}

	n, _ = expired(&i)
	i.Stop()
	time.Sleep(2 * unreplied)
	if n.Load() != 0 {
		t.Fatal("a stopped flow expired")
	}
}

func TestAUDPFlowLivesByWhetherItWasAnswered(t *testing.T) {
	udpUnreplied, udpAssured = 60*time.Millisecond, 400*time.Millisecond
	defer func() { udpUnreplied, udpAssured = UDPUnreplied, UDPAssured }()
	w := newWorld(t)
	answered, silent := netip.MustParseAddrPort("8.8.4.4:9999"), netip.MustParseAddrPort("8.8.4.4:7777")
	w.host.silent[silent] = true
	dials := func(dst netip.AddrPort) (n int) {
		w.host.mu.Lock()
		defer w.host.mu.Unlock()
		for _, d := range w.host.udpDials {
			if d == dst {
				n++
			}
		}
		return n
	}
	open := func(dst netip.AddrPort) *gonet.UDPConn {
		c, err := gonet.DialUDP(w.guest, nil, &tcpip.FullAddress{NIC: 1, Addr: toAddr(dst.Addr()), Port: dst.Port()}, ipv4.ProtocolNumber)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		c.SetDeadline(time.Now().Add(5 * time.Second))
		return c
	}
	a, s := open(answered), open(silent)
	a.Write([]byte("ping"))
	if n, err := a.Read(make([]byte, 16)); err != nil || n != 4 {
		t.Fatalf("no echo: %d, %v", n, err)
	}
	s.Write([]byte("ping"))
	time.Sleep(200 * time.Millisecond)
	a.Write([]byte("ping"))
	s.Write([]byte("ping"))
	if n, err := a.Read(make([]byte, 16)); err != nil || n != 4 {
		t.Fatalf("no echo on the flow that had been answered: %d, %v", n, err)
	}
	time.Sleep(30 * time.Millisecond)
	if got := dials(answered); got != 1 {
		t.Errorf("the answered flow was given %d host sockets within its lifetime, want 1", got)
	}
	if got := dials(silent); got != 2 {
		t.Errorf("the unanswered flow was given %d host sockets, want 2: it should have ended and begun again", got)
	}
	time.Sleep(550 * time.Millisecond)
	a.Write([]byte("ping"))
	if n, err := a.Read(make([]byte, 16)); err != nil || n != 4 {
		t.Fatalf("no echo after the flow began again: %d, %v", n, err)
	}
	if got := dials(answered); got != 2 {
		t.Errorf("the answered flow was given %d host sockets, want 2", got)
	}
}

func TestTheHostAddressAnswersNothingWhileItsMappingIsOff(t *testing.T) {
	hostEnd, guestEnd := socketPair(t)
	h := newFakeHost()
	var off atomic.Bool
	ns, err := New(hostEnd, Options{Host: h, Config: DefaultConfig, NoHostLoopback: &off})
	if err != nil {
		t.Fatal(err)
	}
	g := newGuestStack(t, guestEnd, false)
	t.Cleanup(func() {
		ns.Close()
		g.Close()
	})
	w := &world{host: h, ns: ns, guest: g}
	dials := func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.tcpDials)
	}
	c, err := w.dialTCP(t, hostAddrPort(5432).String())
	if err != nil {
		t.Fatalf("with the mapping on: %v", err)
	}
	c.Close()
	off.Store(true)
	if c, err := w.dialTCP(t, hostAddrPort(5432).String()); err == nil {
		c.Close()
		t.Error("the host address answered with its mapping off")
	}
	if n := dials(); n != 1 {
		t.Errorf("%d dials on the Mac, want the one made while the mapping was on", n)
	}
	if c, err := w.dialTCP(t, "93.184.215.14:80"); err != nil {
		t.Errorf("an ordinary destination with the mapping off: %v", err)
	} else {
		c.Close()
	}
	off.Store(false)
	if c, err := w.dialTCP(t, hostAddrPort(5432).String()); err != nil {
		t.Errorf("with the mapping on again: %v", err)
	} else {
		c.Close()
	}
}
