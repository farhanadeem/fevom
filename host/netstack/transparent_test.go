package netstack

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

type presenceDecider struct {
	configured atomic.Bool
	mu         sync.Mutex
	rules      []decisionRule
	asked      []string
}

type decisionRule struct {
	contains string
	ups      []Upstream
}

func (d *presenceDecider) Configured() bool { return d.configured.Load() }

func (d *presenceDecider) Decide(_ context.Context, url string) ([]Upstream, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked = append(d.asked, url)
	for _, r := range d.rules {
		if strings.Contains(url, r.contains) {
			return r.ups, nil
		}
	}
	return []Upstream{{Kind: Direct}}, nil
}

func (d *presenceDecider) urls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.asked...)
}

type policyProxy struct {
	*testProxy
	refuse map[string]bool
}

func newPolicyProxy(t *testing.T, refuse ...string) *policyProxy {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &policyProxy{testProxy: &testProxy{l: l}, refuse: map[string]bool{}}
	for _, h := range refuse {
		p.refuse[h] = true
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go p.serve(c)
		}
	}()
	return p
}

func (p *policyProxy) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	r, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	p.mu.Lock()
	p.seen = append(p.seen, r.Method+" "+r.RequestURI)
	p.mu.Unlock()
	if r.Method == http.MethodConnect {
		host, _, _ := net.SplitHostPort(r.RequestURI)
		if p.refuse[host] {
			io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
			return
		}
		io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\n")
		io.Copy(c, br)
		io.WriteString(c, "<eof seen>")
		return
	}
	body := "via proxy " + r.RequestURI
	io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\nConnection: close\r\n\r\n"+body)
}

func transparentWorld(t *testing.T, refuse ...string) (*proxyWorld, *presenceDecider, *policyProxy) {
	t.Helper()
	d := &presenceDecider{}
	d.configured.Store(true)
	w := newProxyWorld(t, d)
	w.transparent.Store(true)
	up := newPolicyProxy(t, refuse...)
	w.host.route("192.0.2.10:3128", up.l.Addr().String())
	w.host.add(t, "named.example. 60 IN A 203.0.113.20", "refuse.example. 60 IN A 203.0.113.21",
		"one.example. 60 IN A 203.0.113.30", "two.example. 60 IN A 203.0.113.30", "lan.example. 60 IN A 10.20.30.41")
	return w, d, up
}

var viaTestProxy = []Upstream{{Kind: ViaHTTP, Addr: "proxy.test:3128"}}

func lookupFromGuest(t *testing.T, g *stack.Stack, name string) string {
	t.Helper()
	gw := tcpip.FullAddress{NIC: 1, Addr: toAddr(DefaultConfig.Gateway), Port: 53}
	conn, err := gonet.DialUDP(g, nil, &gw, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	dc := &dns.Conn{Conn: conn}
	if err := dc.WriteMsg(m); err != nil {
		t.Fatal(err)
	}
	resp, err := dc.ReadMsg()
	if err != nil {
		t.Fatal(err)
	}
	for _, rr := range resp.Answer {
		if a, ok := rr.(*dns.A); ok {
			return a.A.String()
		}
	}
	t.Fatalf("no address for %s", name)
	return ""
}

func (w *proxyWorld) dialFromGuest(t *testing.T, dst string) (*gonet.TCPConn, error) {
	t.Helper()
	ap := netip.MustParseAddrPort(dst)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return gonet.DialContextTCP(ctx, w.guest, tcpip.FullAddress{NIC: 1, Addr: toAddr(ap.Addr()), Port: ap.Port()}, ipv4.ProtocolNumber)
}

func echoed(t *testing.T, c net.Conn) string {
	t.Helper()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, "ping")
	c.(interface{ CloseWrite() error }).CloseWrite()
	got, _ := io.ReadAll(c)
	return string(got)
}

func TestTheTransparentProxyConnectsByTheNameItsDNSGaveOut(t *testing.T) {
	w, d, up := transparentWorld(t)
	d.rules = []decisionRule{{"named.example", viaTestProxy}}
	addr := lookupFromGuest(t, w.guest, "named.example")
	c, err := w.dialFromGuest(t, addr+":443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := echoed(t, c); got != "ping<eof seen>" {
		t.Fatalf("through the proxy: %q", got)
	}
	if got := up.requests(); len(got) != 1 || got[0] != "CONNECT named.example:443" {
		t.Fatalf("the proxy saw %q", got)
	}
	if w.host.dialled("203.0.113.20:443") {
		t.Fatal("the target was dialled from the Mac")
	}
	if urls := d.urls(); len(urls) != 1 || urls[0] != "https://named.example:443/" {
		t.Fatalf("decided for %q", urls)
	}
}

func TestTheTransparentProxyReadsTheServerNameOrGoesByAddress(t *testing.T) {
	w, d, up := transparentWorld(t)
	d.rules = []decisionRule{{"sni.example", viaTestProxy}, {"203.0.113.40", viaTestProxy}}
	c, err := w.dialFromGuest(t, "203.0.113.41:443")
	if err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(c, &tls.Config{ServerName: "sni.example", InsecureSkipVerify: true})
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	tc.Handshake()
	c.Close()
	waitFor(t, func() bool { return len(up.requests()) == 1 })
	if got := up.requests(); got[0] != "CONNECT sni.example:443" {
		t.Fatalf("the proxy saw %q", got)
	}

	c, err = w.dialFromGuest(t, "203.0.113.40:5432")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := echoed(t, c); got != "ping<eof seen>" {
		t.Fatalf("by address: %q", got)
	}
	if got := up.requests(); len(got) != 2 || got[1] != "CONNECT 203.0.113.40:5432" {
		t.Fatalf("the proxy saw %q", got)
	}
}

func TestTheTransparentProxySendsPlainHTTPInAbsoluteForm(t *testing.T) {
	w, d, up := transparentWorld(t)
	d.rules = []decisionRule{{"http://web.example", viaTestProxy}}
	c, err := w.dialFromGuest(t, "203.0.113.50:80")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	for _, path := range []string{"/a", "/b?q=1"} {
		io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: web.example\r\nProxy-Authorization: not-yours\r\n\r\n")
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "via proxy http://web.example"+path {
			t.Fatalf("%s: %q", path, body)
		}
	}
	if got := up.requests(); len(got) != 2 || got[0] != "GET http://web.example/a" || got[1] != "GET http://web.example/b?q=1" {
		t.Fatalf("the proxy saw %q", got)
	}
	if w.host.dialled("203.0.113.50:80") {
		t.Fatal("the target was dialled from the Mac")
	}
}

func TestTheTransparentProxyLeavesExclusionsTheLANAndItsSettingOffDirect(t *testing.T) {
	w, d, up := transparentWorld(t)
	d.rules = []decisionRule{{"", viaTestProxy}}
	w.exclude.Store([]string{"named.example"})
	addr := lookupFromGuest(t, w.guest, "named.example")
	c, err := w.dialFromGuest(t, addr+":443")
	if err != nil {
		t.Fatal(err)
	}
	if got := echoed(t, c); got != "ping<eof seen>" {
		t.Fatalf("excluded by name: %q", got)
	}
	c.Close()
	if !w.host.dialled("203.0.113.20:443") || len(up.requests()) != 0 {
		t.Fatalf("an excluded name went through the proxy: %v", up.requests())
	}

	w.lans.Store([]netip.Prefix{netip.MustParsePrefix("10.20.0.0/16")})
	c, err = w.dialFromGuest(t, lookupFromGuest(t, w.guest, "lan.example")+":22")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if !w.host.dialled("10.20.30.41:22") || len(up.requests()) != 0 {
		t.Fatalf("the LAN went through the proxy: %v", up.requests())
	}

	w.transparent.Store(false)
	c, err = w.dialFromGuest(t, "203.0.113.60:443")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if !w.host.dialled("203.0.113.60:443") || len(up.requests()) != 0 {
		t.Fatalf("with the setting off: %v", up.requests())
	}
}

func TestTheTransparentProxysRefusalIsARefusalNeverADirectConnection(t *testing.T) {
	w, d, up := transparentWorld(t, "refuse.example")
	d.rules = []decisionRule{{"refuse.example", append(viaTestProxy, Upstream{Kind: Direct})}, {"named.example", []Upstream{{Kind: ViaHTTP, Addr: "down.test:3128"}}}}
	w.host.refuse[netip.MustParseAddrPort("192.0.2.99:3128")] = true
	addr := lookupFromGuest(t, w.guest, "refuse.example")
	if c, err := w.dialFromGuest(t, addr+":443"); err == nil {
		c.Close()
		t.Fatal("a flow the proxy refused was connected")
	}
	if got := up.requests(); len(got) != 1 || got[0] != "CONNECT refuse.example:443" {
		t.Fatalf("the proxy saw %q", got)
	}
	if w.host.dialled("203.0.113.21:443") {
		t.Fatal("a refused flow was dialled direct")
	}
	addr = lookupFromGuest(t, w.guest, "named.example")
	if c, err := w.dialFromGuest(t, addr+":443"); err == nil {
		c.Close()
		t.Fatal("a flow whose proxy is down was connected")
	}
	if w.host.dialled("203.0.113.20:443") {
		t.Fatal("a flow whose proxy is down was dialled direct")
	}
	d.rules = []decisionRule{{"", []Upstream{{Kind: ViaHTTP, Addr: "down.test:3128"}}}}
	w.ns.InvalidateProxyDecisions()
	c, err := w.dialFromGuest(t, "203.0.113.70:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := echoed(t, c); got != "" {
		t.Fatalf("after a reset: %q", got)
	}
	if w.host.dialled("203.0.113.70:443") {
		t.Fatal("dialled direct")
	}
}

func TestWithNoProxyConfiguredFlowsAreDirectAndNothingIsDecided(t *testing.T) {
	w, d, up := transparentWorld(t)
	d.configured.Store(false)
	d.rules = []decisionRule{{"", viaTestProxy}}
	c, err := w.dialFromGuest(t, "203.0.113.80:22")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if !w.host.dialled("203.0.113.80:22") || len(up.requests()) != 0 || len(d.urls()) != 0 {
		t.Fatalf("proxy %v, decided %v", up.requests(), d.urls())
	}
	d.configured.Store(true)
	w.setting.Store("none")
	c, err = w.dialFromGuest(t, "203.0.113.81:22")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if !w.host.dialled("203.0.113.81:22") || len(up.requests()) != 0 {
		t.Fatalf("network.proxy none: %v", up.requests())
	}
}

func TestAnAddressSharedByTwoNamesGoesByTheFlowsOwnName(t *testing.T) {
	w, d, up := transparentWorld(t)
	d.rules = []decisionRule{{"", viaTestProxy}}
	lookupFromGuest(t, w.guest, "one.example")
	addr := lookupFromGuest(t, w.guest, "two.example")
	c, err := w.dialFromGuest(t, addr+":443")
	if err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(c, &tls.Config{ServerName: "one.example", InsecureSkipVerify: true})
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	tc.Handshake()
	c.Close()
	waitFor(t, func() bool { return len(up.requests()) == 1 })
	if got := up.requests(); got[0] != "CONNECT one.example:443" {
		t.Fatalf("the proxy saw %q", got)
	}
}

func TestTheTransparentProxyArmsNoTimerAtIdle(t *testing.T) {
	saved := [2]time.Duration{udpUnreplied, udpAssured}
	udpUnreplied, udpAssured = 200*time.Millisecond, 400*time.Millisecond
	defer func() { udpUnreplied, udpAssured = saved[0], saved[1] }()
	w, d, _ := transparentWorld(t)
	d.rules = []decisionRule{{"", viaTestProxy}}
	c, err := w.dialFromGuest(t, "203.0.113.90:443")
	if err != nil {
		t.Fatal(err)
	}
	nud, terr := w.ns.stack.NUDConfigurations(nicID, ipv4.ProtocolNumber)
	if terr != nil {
		t.Fatal(terr)
	}
	nud.BaseReachableTime = time.Second
	w.ns.stack.SetNUDConfigurations(nicID, ipv4.ProtocolNumber, nud)
	if got := echoed(t, c); got != "ping<eof seen>" {
		t.Fatalf("through the proxy: %q", got)
	}
	c.Close()
	if counts, ok := quietFor(w.ns, time.Second, 60*time.Second); !ok {
		t.Fatalf("still busy: %+v", counts)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("not in 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAnsweredNamesKeepTheirAddressesForAWhileAndWithinABound(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	a := &answeredNames{now: func() time.Time { return now }}
	rr := func(s string) dns.RR {
		r, err := dns.NewRR(s)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a.record("Short.Example.", []dns.RR{rr("short.example. 5 IN A 192.0.2.1"), rr("short.example. 5 IN AAAA 2001:db8::1")})
	a.record("x.fev.local.", []dns.RR{rr("x.fev.local. 5 IN A 198.19.4.1")})
	a.record("host.docker.internal.", []dns.RR{rr("host.docker.internal. 5 IN A 198.19.1.254")})
	if n, sure := a.lookup(netip.MustParseAddr("192.0.2.1")); n != "short.example" || !sure {
		t.Fatalf("%q %v", n, sure)
	}
	if n, _ := a.lookup(netip.MustParseAddr("2001:db8::1")); n != "short.example" {
		t.Fatalf("AAAA: %q", n)
	}
	if n, _ := a.lookup(netip.MustParseAddr("198.19.4.1")); n != "" {
		t.Fatalf("fev's own name noted: %q", n)
	}
	if n, _ := a.lookup(netip.MustParseAddr("198.19.1.254")); n != "" {
		t.Fatalf("a host name noted: %q", n)
	}
	now = now.Add(answeredKeep - time.Second)
	if n, _ := a.lookup(netip.MustParseAddr("192.0.2.1")); n == "" {
		t.Fatal("lapsed at its TTL, before the least time kept")
	}
	now = now.Add(2 * time.Second)
	if n, _ := a.lookup(netip.MustParseAddr("192.0.2.1")); n != "" {
		t.Fatal("kept past its time")
	}
	for i := range answeredMax + 100 {
		ip := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		a.record("n.example.", []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "n.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: ip.AsSlice()}})
	}
	a.mu.Lock()
	n := len(a.m)
	a.mu.Unlock()
	if n > answeredMax {
		t.Fatalf("%d entries", n)
	}
}

func TestSniffingANameFromAFlowsFirstBytes(t *testing.T) {
	hello := clientHello(t, "Sni.Example")
	for cut := 1; cut < len(hello); cut += 37 {
		if _, kind, res := sniffName(hello[:cut]); res != sniffMore || kind != "tls" {
			t.Fatalf("a partial hello (%d of %d): %v %q", cut, len(hello), res, kind)
		}
	}
	if name, kind, res := sniffName(hello); name != "sni.example" || kind != "tls" || res != sniffDone {
		t.Fatalf("hello: %q %q %v", name, kind, res)
	}
	if name, _, _ := sniffName(clientHello(t, "")); name != "" {
		t.Fatalf("no server name: %q", name)
	}
	for _, tc := range []struct {
		in, name, kind string
		res            sniffResult
	}{
		{"GET / HTTP/1.1\r\nHost: Web.Example:8080\r\n\r\n", "web.example", "http", sniffDone},
		{"POST /x HTTP/1.1\r\nUser-Agent: a\r\nhost: api.example\r\n", "api.example", "http", sniffDone},
		{"GET / HTTP/1.1\r\nHost: 192.0.2.4\r\n\r\n", "", "http", sniffDone},
		{"GET / HTTP/1.1\r\nUser-Agent: a\r\n", "", "http", sniffMore},
		{"GE", "", "http", sniffMore},
		{"SSH-2.0-OpenSSH_9.9\r\n", "", "", sniffDone},
		{"\x00\x01binary", "", "", sniffDone},
		{"GET / HTTP/1.1\r\nHost: bad name!\r\n\r\n", "", "http", sniffDone},
	} {
		name, kind, res := sniffName([]byte(tc.in))
		if name != tc.name || kind != tc.kind || res != tc.res {
			t.Errorf("%q: %q %q %v", tc.in, name, kind, res)
		}
	}
}

func clientHello(t *testing.T, name string) []byte {
	t.Helper()
	a, b := net.Pipe()
	go func() {
		tc := tls.Client(a, &tls.Config{ServerName: name, InsecureSkipVerify: true})
		tc.Handshake()
		a.Close()
	}()
	defer b.Close()
	b.SetDeadline(time.Now().Add(5 * time.Second))
	head := make([]byte, 5)
	if _, err := io.ReadFull(b, head); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(head[3])<<8|int(head[4]))
	if _, err := io.ReadFull(b, body); err != nil {
		t.Fatal(err)
	}
	return append(head, body...)
}

func TestAProxyThatNeverAnswersTheConnectIsGivenUpOn(t *testing.T) {
	saved := proxyDialTimeout
	proxyDialTimeout = 300 * time.Millisecond
	defer func() { proxyDialTimeout = saved }()
	w, d, _ := transparentWorld(t)
	silent, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			go io.Copy(io.Discard, c)
		}
	}()
	w.host.route("192.0.2.12:3128", silent.Addr().String())
	w.host.add(t, "mute.test. 60 IN A 192.0.2.12")
	d.rules = []decisionRule{{"named.example", []Upstream{{Kind: ViaHTTP, Addr: "mute.test:3128"}}}}
	addr := lookupFromGuest(t, w.guest, "named.example")
	start := time.Now()
	if c, err := w.dialFromGuest(t, addr+":443"); err == nil {
		c.Close()
		t.Fatal("connected through a proxy that never answered")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("given up on after %v", took)
	}
	if w.host.dialled("203.0.113.20:443") {
		t.Fatal("dialled direct")
	}
}

func TestANameFromTheFlowsBytesOpensNoDirectPathToAnotherAddress(t *testing.T) {
	w, d, up := transparentWorld(t)
	d.rules = []decisionRule{{"inside.example", []Upstream{{Kind: Direct}}}, {"", viaTestProxy}}
	w.host.add(t, "inside.example. 60 IN A 203.0.113.25")
	w.exclude.Store([]string{"named.example"})
	hello := func(dst, name string) {
		t.Helper()
		c, err := w.dialFromGuest(t, dst)
		if err != nil {
			t.Fatal(err)
		}
		tc := tls.Client(c, &tls.Config{ServerName: name, InsecureSkipVerify: true})
		tc.SetDeadline(time.Now().Add(2 * time.Second))
		tc.Handshake()
		c.Close()
	}
	for i, name := range []string{"named.example", "inside.example"} {
		hello("203.0.113.99:443", name)
		waitFor(t, func() bool { return len(up.requests()) == i+1 })
		if got := up.requests(); got[i] != "CONNECT "+name+":443" {
			t.Fatalf("%s to another address: the proxy saw %q", name, got)
		}
		if w.host.dialled("203.0.113.99:443") {
			t.Fatalf("%s opened a direct path to an address that is not its own", name)
		}
	}
	for _, dst := range []string{"203.0.113.20:443", "203.0.113.25:443"} {
		name := map[string]string{"203.0.113.20:443": "named.example", "203.0.113.25:443": "inside.example"}[dst]
		hello(dst, name)
		waitFor(t, func() bool { return w.host.dialled(dst) })
	}
	if got := up.requests(); len(got) != 2 {
		t.Fatalf("a name's own address went through the proxy: %q", got)
	}
}
