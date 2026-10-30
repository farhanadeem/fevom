package netstack

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fev/proto/netplan"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func TestNetworkProxyValues(t *testing.T) {
	for _, tc := range []struct {
		in    string
		fixed bool
		want  string
		err   bool
	}{
		{"", false, "", false},
		{"auto", false, "", false},
		{"none", true, "direct", false},
		{"http://proxy.example:3128", true, "http proxy proxy.example:3128", false},
		{"http://proxy.example", true, "http proxy proxy.example:80", false},
		{"https://u:p@proxy.example", true, "https proxy proxy.example:443", false},
		{"socks5://10.0.0.1", true, "socks5 proxy 10.0.0.1:1080", false},
		{"socks5h://[fd00::1]:9050", true, "socks5 proxy [fd00::1]:9050", false},
		{"ftp://proxy.example", false, "", true},
		{"proxy.example:3128", false, "", true},
		{"http://proxy.example:99999", false, "", true},
		{"http://proxy.example/path", false, "", true},
	} {
		up, fixed, err := ParseProxySetting(tc.in)
		if (err != nil) != tc.err || fixed != tc.fixed || (fixed && up.String() != tc.want) {
			t.Errorf("%q: %v %v %v", tc.in, up, fixed, err)
		}
	}
	if up, _, _ := ParseProxySetting("socks5h://x:1"); !up.RemoteNames {
		t.Error("socks5h does not send names")
	}
	if up, _, _ := ParseProxySetting("socks5://x:1"); up.RemoteNames {
		t.Error("socks5 sends names")
	}
	if got := MaskProxySetting("http://alice:s3cret@proxy.example:3128"); strings.Contains(got, "s3cret") || !strings.Contains(got, "alice") {
		t.Errorf("masked %q", got)
	}
	if _, _, err := ParseProxySetting("ftp://bob:hunter2@x"); err != nil && strings.Contains(err.Error(), "hunter2") {
		t.Errorf("an error shows the password: %v", err)
	}
}

func TestProxyExclusions(t *testing.T) {
	list := []string{"internal.example", ".corp.example", "*.lab.example", "10.0.0.0/8", "192.168.1.5", "fd00::/8"}
	yes := []string{"internal.example", "a.internal.example", "corp.example", "x.y.corp.example", "lab.example", "b.lab.example", "10.2.3.4", "192.168.1.5", "[fd00::9]", "INTERNAL.example."}
	no := []string{"example.com", "notinternal.example", "192.168.1.6", "11.0.0.1", "2606:4700::1"}
	for _, h := range yes {
		if !excluded(h, list) {
			t.Errorf("%s not excluded", h)
		}
	}
	for _, h := range no {
		if excluded(h, list) {
			t.Errorf("%s excluded", h)
		}
	}
}

type proxyHost struct {
	*fakeHost
	mu     sync.Mutex
	routes map[netip.AddrPort]string
}

func (h *proxyHost) DialTCP(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	h.mu.Lock()
	to, ok := h.routes[addr]
	h.mu.Unlock()
	if !ok {
		return h.fakeHost.DialTCP(ctx, addr)
	}
	h.fakeHost.mu.Lock()
	h.fakeHost.tcpDials = append(h.fakeHost.tcpDials, addr)
	h.fakeHost.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, "tcp", to)
}

func (h *proxyHost) route(addr string, to string) {
	h.mu.Lock()
	h.routes[netip.MustParseAddrPort(addr)] = to
	h.mu.Unlock()
}

func (h *proxyHost) dialled(addr string) bool {
	h.fakeHost.mu.Lock()
	defer h.fakeHost.mu.Unlock()
	for _, d := range h.fakeHost.tcpDials {
		if d == netip.MustParseAddrPort(addr) {
			return true
		}
	}
	return false
}

type proxyWorld struct {
	host        *proxyHost
	ns          *Stack
	guest       *stack.Stack
	setting     atomic.Value
	exclude     atomic.Value
	noLoop      atomic.Bool
	transparent atomic.Bool
	lans        atomic.Value
}

func newProxyWorld(t *testing.T, system ProxyDecider) *proxyWorld {
	t.Helper()
	hostEnd, guestEnd := socketPair(t)
	w := &proxyWorld{host: &proxyHost{fakeHost: newFakeHost(), routes: map[netip.AddrPort]string{}}}
	w.setting.Store("auto")
	w.exclude.Store([]string(nil))
	w.lans.Store([]netip.Prefix(nil))
	if system == nil {
		system = fixedDecider{{Kind: Direct}}
	}
	opts := Options{Host: w.host, NoHostLoopback: &w.noLoop, Proxy: &ProxyOptions{
		Setting:     func() string { return w.setting.Load().(string) },
		Exclude:     func() []string { return w.exclude.Load().([]string) },
		System:      system,
		Transparent: func() bool { return w.transparent.Load() },
		LANs:        func() []netip.Prefix { return w.lans.Load().([]netip.Prefix) },
	}}
	ns, err := New(hostEnd, opts)
	if err != nil {
		t.Fatal(err)
	}
	g := newGuestStack(t, guestEnd, false)
	t.Cleanup(func() {
		ns.Close()
		g.Close()
	})
	w.ns, w.guest = ns, g
	w.host.add(t, "example.com. 60 IN A 93.184.215.14", "proxy.test. 60 IN A 192.0.2.10", "socks.test. 60 IN A 192.0.2.11",
		"localhost. 60 IN A 127.0.0.1", "svc.internal.example. 60 IN A 10.20.30.40", "down.test. 60 IN A 192.0.2.99",
		"inside.example. 60 IN A 198.19.70.4")
	return w
}

var endpoint = netplan.Default().ProxyEndpoint()

func (w *proxyWorld) dial(ctx context.Context) (net.Conn, error) {
	return gonet.DialContextTCP(ctx, w.guest, tcpip.FullAddress{NIC: 1, Addr: toAddr(endpoint.Addr()), Port: endpoint.Port()}, ipv4.ProtocolNumber)
}

func (w *proxyWorld) connect(t *testing.T, target string) (string, net.Conn, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := w.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("CONNECT %s: %v", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return resp.Status + " " + string(body), c, br
	}
	return resp.Status, c, br
}

func echoThroughTunnel(t *testing.T, c net.Conn, br *bufio.Reader) {
	t.Helper()
	io.WriteString(c, "ping")
	c.(interface{ CloseWrite() error }).CloseWrite()
	got, _ := io.ReadAll(br)
	if string(got) != "ping<eof seen>" {
		t.Fatalf("through the tunnel: %q", got)
	}
}

func (w *proxyWorld) client() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy:       http.ProxyURL(&url.URL{Scheme: "http", Host: endpoint.String()}),
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return w.dial(ctx) },
	}}
}

type fixedDecider []Upstream

func (f fixedDecider) Decide(context.Context, string) ([]Upstream, error) { return f, nil }

func TestTheProxyEndpointCarriesConnectAndPlainHTTPDirect(t *testing.T) {
	w := newProxyWorld(t, nil)
	status, c, br := w.connect(t, "example.com:443")
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT: %s", status)
	}
	echoThroughTunnel(t, c, br)
	if !w.host.dialled("93.184.215.14:443") {
		t.Fatalf("dials %v", w.host.tcpDials)
	}

	var seen *http.Request
	origin := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		seen = r
		io.WriteString(rw, "origin says hi")
	}))
	defer origin.Close()
	w.host.route("93.184.215.14:80", origin.Listener.Addr().String())
	resp, err := w.client().Get("http://example.com/some/path?q=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "origin says hi" || seen.RequestURI != "/some/path?q=1" || seen.Header.Get("Proxy-Connection") != "" {
		t.Fatalf("got %q; origin saw %q %v", body, seen.RequestURI, seen.Header)
	}
}

type testProxy struct {
	l    net.Listener
	mu   sync.Mutex
	seen []string
}

func newTestProxy(t *testing.T) *testProxy {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &testProxy{l: l}
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

func (p *testProxy) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	r, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	p.mu.Lock()
	p.seen = append(p.seen, r.Method+" "+r.RequestURI+" auth="+r.Header.Get("Proxy-Authorization"))
	p.mu.Unlock()
	if r.Method == http.MethodConnect {
		io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\n")
		io.Copy(c, br)
		io.WriteString(c, "<eof seen>")
		return
	}
	io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 9\r\nConnection: close\r\n\r\nvia proxy")
}

func (p *testProxy) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func TestNetworkProxyAURLSendsEverythingThroughItWithItsCredentials(t *testing.T) {
	w := newProxyWorld(t, nil)
	up := newTestProxy(t)
	w.host.route("192.0.2.10:3128", up.l.Addr().String())
	w.setting.Store("http://alice:s3cret@proxy.test:3128")
	status, c, br := w.connect(t, "registry.example:443")
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT: %s", status)
	}
	echoThroughTunnel(t, c, br)
	resp, err := w.client().Get("http://example.com/a")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	want := []string{"CONNECT registry.example:443 auth=" + auth, "GET http://example.com/a auth=" + auth}
	if got := up.requests(); string(body) != "via proxy" || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("body %q; the proxy saw %q", body, got)
	}
	if w.host.dialled("93.184.215.14:80") {
		t.Fatal("dialled the target direct")
	}

	w.exclude.Store([]string{"*.internal.example", "10.0.0.0/8"})
	status, c, br = w.connect(t, "svc.internal.example:443")
	if !strings.Contains(status, "200") {
		t.Fatalf("excluded CONNECT: %s", status)
	}
	echoThroughTunnel(t, c, br)
	if !w.host.dialled("10.20.30.40:443") || len(up.requests()) != 2 {
		t.Fatalf("an excluded name went through the proxy: %v", up.requests())
	}
}

func newTestSOCKS(t *testing.T) (net.Listener, chan string) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	targets := make(chan string, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 262)
				if _, err := io.ReadFull(c, buf[:2]); err != nil {
					return
				}
				io.ReadFull(c, buf[:buf[1]])
				c.Write([]byte{5, 0})
				io.ReadFull(c, buf[:4])
				var host string
				switch buf[3] {
				case 1:
					io.ReadFull(c, buf[:4])
					host = netip.AddrFrom4([4]byte(buf[:4])).String()
				case 3:
					io.ReadFull(c, buf[:1])
					n := int(buf[0])
					io.ReadFull(c, buf[:n])
					host = string(buf[:n])
				}
				io.ReadFull(c, buf[:2])
				targets <- net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(buf[:2]))))
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				io.Copy(c, c)
				c.(*net.TCPConn).CloseWrite()
			}(c)
		}
	}()
	return l, targets
}

func TestTheProxyEndpointGoesThroughSOCKS5(t *testing.T) {
	w := newProxyWorld(t, nil)
	l, targets := newTestSOCKS(t)
	w.host.route("192.0.2.11:1080", l.Addr().String())
	for _, tc := range []struct{ setting, want string }{
		{"socks5h://socks.test:1080", "example.com:443"},
		{"socks5://socks.test:1080", "93.184.215.14:443"},
	} {
		w.setting.Store(tc.setting)
		status, c, br := w.connect(t, "example.com:443")
		if !strings.Contains(status, "200") {
			t.Fatalf("%s: %s", tc.setting, status)
		}
		if got := <-targets; got != tc.want {
			t.Fatalf("%s: the SOCKS server was asked for %s, want %s", tc.setting, got, tc.want)
		}
		io.WriteString(c, "ping")
		c.(interface{ CloseWrite() error }).CloseWrite()
		if got, _ := io.ReadAll(br); string(got) != "ping" {
			t.Fatalf("%s: %q", tc.setting, got)
		}
	}
}

func TestTheProxyEndpointRefusesFevomsAddressesAndTheMacsLoopback(t *testing.T) {
	w := newProxyWorld(t, nil)
	for _, target := range []string{"127.0.0.1:22", "localhost:22", "[::1]:22", "198.19.64.9:80", "inside.example:80", "198.19.1.1:3128", "169.254.169.254:80"} {
		if status, _, _ := w.connect(t, target); !strings.Contains(status, "403") || !strings.Contains(status, "fev's proxy endpoint") {
			t.Errorf("CONNECT %s: %s", target, status)
		}
	}
	if w.host.dialled("127.0.0.1:22") || w.host.dialled("198.19.70.4:80") {
		t.Fatal("a refused target was dialled")
	}
	status, c, br := w.connect(t, "host.docker.internal:5432")
	if !strings.Contains(status, "200") || !w.host.dialled("127.0.0.1:5432") {
		t.Fatalf("host.docker.internal with the mapping on: %s", status)
	}
	echoThroughTunnel(t, c, br)
	w.noLoop.Store(true)
	if status, _, _ := w.connect(t, "host.fev.internal:5433"); !strings.Contains(status, "403") {
		t.Fatalf("host.fev.internal with the mapping off: %s", status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c2, err := w.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	io.WriteString(c2, "GET / HTTP/1.1\r\nHost: 198.19.1.1:3128\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c2), nil)
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a request to the endpoint itself: %v %v", resp, err)
	}
}

type countingDecider struct {
	n   atomic.Int32
	ups []Upstream
}

func (d *countingDecider) Decide(context.Context, string) ([]Upstream, error) {
	d.n.Add(1)
	return d.ups, nil
}

func TestProxyDecisionsAreKeptUntilTheNetworkChangesAndTriedInOrder(t *testing.T) {
	d := &countingDecider{ups: []Upstream{{Kind: ViaHTTP, Addr: "down.test:3128"}, {Kind: Direct}}}
	w := newProxyWorld(t, d)
	w.host.refuse[netip.MustParseAddrPort("192.0.2.99:3128")] = true
	for i := 0; i < 2; i++ {
		status, c, br := w.connect(t, "example.com:443")
		if !strings.Contains(status, "200") {
			t.Fatalf("CONNECT: %s", status)
		}
		echoThroughTunnel(t, c, br)
	}
	if d.n.Load() != 1 {
		t.Fatalf("%d decisions for two requests", d.n.Load())
	}
	w.ns.InvalidateProxyDecisions()
	w.connect(t, "example.com:443")
	if d.n.Load() != 2 {
		t.Fatalf("%d decisions after the network changed", d.n.Load())
	}
	d.ups = []Upstream{{Kind: ViaHTTP, Addr: "down.test:3128"}}
	w.ns.InvalidateProxyDecisions()
	status, _, _ := w.connect(t, "example.com:443")
	if !strings.Contains(status, "502") || !strings.Contains(status, "fev's proxy endpoint could not reach example.com:443") || !strings.Contains(status, "down.test") {
		t.Fatalf("a dead upstream: %s", status)
	}
}

func TestTheProxyEndpointArmsNoTimerAtIdle(t *testing.T) {
	saved := [2]time.Duration{udpUnreplied, udpAssured}
	udpUnreplied, udpAssured = 200*time.Millisecond, 400*time.Millisecond
	defer func() { udpUnreplied, udpAssured = saved[0], saved[1] }()
	w := newProxyWorld(t, nil)
	status, c, br := w.connect(t, "example.com:443")
	if !strings.Contains(status, "200") {
		t.Fatal(status)
	}
	nud, terr := w.ns.stack.NUDConfigurations(nicID, ipv4.ProtocolNumber)
	if terr != nil {
		t.Fatal(terr)
	}
	nud.BaseReachableTime = time.Second
	w.ns.stack.SetNUDConfigurations(nicID, ipv4.ProtocolNumber, nud)
	echoThroughTunnel(t, c, br)
	c.Close()
	if counts, ok := quietFor(w.ns, time.Second, 60*time.Second); !ok {
		t.Fatalf("still busy: %+v", counts)
	}
}
