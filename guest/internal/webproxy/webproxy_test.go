package webproxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fev/guest/internal/webproxy/webctl"
	api "fev/proto/agent"
)

type testCA struct {
	root   *x509.Certificate
	key    *ecdsa.PrivateKey
	issued atomic.Int64
	now    func() time.Time
	mu     sync.Mutex
	asked  []string
}

func newTestCA(t *testing.T) *testCA {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour * 365),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := x509.ParseCertificate(der)
	return &testCA{root: root, key: k, now: time.Now}
}

func (c *testCA) Issue(ctx context.Context, req api.TLSIssueRequest) (api.TLSIssued, error) {
	c.mu.Lock()
	c.asked = append(c.asked, req.Requester)
	c.mu.Unlock()
	names, err := api.CSRNames(req.CSR)
	if err != nil {
		return api.TLSIssued{Error: err.Error()}, nil
	}
	b, _ := pem.Decode([]byte(req.CSR))
	csr, _ := x509.ParseCertificateRequest(b.Bytes)
	now := c.now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(c.issued.Add(1) + 1), DNSNames: names, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(api.LeafLifetime),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.root, csr.PublicKey, c.key)
	if err != nil {
		return api.TLSIssued{}, err
	}
	return api.TLSIssued{Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}, nil
}

type rig struct {
	t        *testing.T
	p        *Proxy
	ca       *testCA
	mu       sync.Mutex
	origin   map[string]netip.AddrPort
	backends map[string]string
	listen   map[string][]Listener
	fronts   map[string][2]string
	unredir  []string
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, ca: newTestCA(t), origin: map[string]netip.AddrPort{}, backends: map[string]string{}, listen: map[string][]Listener{}, fronts: map[string][2]string{}}
	r.p = New(Config{
		OriginalDst: func(c net.Conn) (netip.AddrPort, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			a, ok := r.origin[c.LocalAddr().String()]
			if !ok {
				return netip.AddrPort{}, fmt.Errorf("no origin for %s", c.LocalAddr())
			}
			return a, nil
		},
		Lister: listerFunc(func(tg api.DomainTarget) ([]Listener, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.listen[tg.ID], nil
		}),
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			r.mu.Lock()
			real, ok := r.backends[addr]
			r.mu.Unlock()
			if !ok {
				return nil, fmt.Errorf("dial %s: connection refused", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, real)
		},
		Issuer:     r.ca,
		Unredirect: func(a netip.AddrPort) { r.mu.Lock(); r.unredir = append(r.unredir, a.String()); r.mu.Unlock() },
		Logf:       t.Logf,
	})
	t.Cleanup(r.p.Close)
	return r
}

type listerFunc func(api.DomainTarget) ([]Listener, error)

func (f listerFunc) Listening(t api.DomainTarget) ([]Listener, error) { return f(t) }

func (r *rig) front(addr string) {
	var pair [2]string
	for i, port := range []uint16{80, 443} {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			r.t.Fatal(err)
		}
		r.t.Cleanup(func() { l.Close() })
		r.mu.Lock()
		r.origin[l.Addr().String()] = netip.AddrPortFrom(netip.MustParseAddr(addr), port)
		r.mu.Unlock()
		pair[i] = l.Addr().String()
		if port == 80 {
			go r.p.ServeHTTP80(l)
		} else {
			go r.p.ServeHTTPS(l)
		}
	}
	r.fronts[addr] = pair
}

func (r *rig) backend(id, at string, h http.Handler, tlsToo bool, loopbackOnly bool) *httptest.Server {
	var s *httptest.Server
	if tlsToo {
		s = httptest.NewTLSServer(h)
	} else {
		s = httptest.NewServer(h)
	}
	r.t.Cleanup(s.Close)
	r.raw(id, at, s.Listener.Addr().String(), loopbackOnly)
	return s
}

func (r *rig) raw(id, at, real string, loopbackOnly bool) {
	ap := netip.MustParseAddrPort(at)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backends[at] = real
	bind := netip.IPv4Unspecified()
	if loopbackOnly {
		bind = netip.MustParseAddr("127.0.0.1")
	}
	r.listen[id] = append(r.listen[id], Listener{Addr: bind, Port: int(ap.Port())})
}

func (r *rig) client(h2 bool) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(r.ca.root)
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool},
		ForceAttemptHTTP2: h2,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, _ := net.SplitHostPort(addr)
			a := r.addrOf(host)
			pair, ok := r.fronts[a]
			if !ok {
				return nil, fmt.Errorf("no front for %s", host)
			}
			to := pair[0]
			if port == "443" {
				to = pair[1]
			}
			var d net.Dialer
			return d.DialContext(ctx, network, to)
		},
	}
	if !h2 {
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	r.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func (r *rig) addrOf(host string) string {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	if host == "fev.local" {
		return r.p.set.Index
	}
	for _, t := range r.p.set.Targets {
		for _, n := range t.Names {
			if n == host || strings.HasPrefix(n, "*.") && strings.HasSuffix(host, n[1:]) {
				return t.Address
			}
		}
	}
	return host
}

func page(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "%s host=%s xff=%q xfp=%q xfh=%q ua=%q", body, r.Host, r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Proto"), r.Header.Get("X-Forwarded-Host"), r.Header.Get("User-Agent"))
	})
}

func plainText(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, body)
	})
}

func get(t *testing.T, c *http.Client, url string) (int, string, *http.Response) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp
}

func container(id, name, addr, target string, extra ...string) api.DomainTarget {
	n := strings.ToLower(name) + ".fev.local"
	names := append([]string{"*." + n, n}, extra...)
	return api.DomainTarget{Kind: api.TargetContainer, ID: id, Name: name, Address: addr, Names: names, Target: target, NetNS: "/var/run/docker/netns/" + id}
}

func TestTheRule(t *testing.T) {
	for _, c := range []struct {
		name string
		ps   []probed
		want WebPort
		ok   bool
	}{
		{"one port", []probed{{port: 3000, http: true}}, WebPort{Port: 3000}, true},
		{"HTML over a lower metrics port", []probed{{port: 8000, http: true}, {port: 9000, http: true, html: true}}, WebPort{Port: 9000}, true},
		{"EXPOSEd over a lower API", []probed{{port: 4318, http: true}, {port: 16686, http: true, expose: true}, {port: 14269, http: true}}, WebPort{Port: 16686}, true},
		{"plain before TLS", []probed{{port: 9443, http: true, html: true, tls: true}, {port: 9000, http: true, html: true}}, WebPort{Port: 9000}, true},
		{"TLS alone", []probed{{port: 8384, http: true, html: true, tls: true}, {port: 22000}}, WebPort{Port: 8384, TLS: true}, true},
		{"no HTTP", []probed{{port: 5432}, {port: 6379}}, WebPort{}, false},
	} {
		got, ok := decide(c.ps)
		if ok != c.ok || got.Port != c.want.Port || got.TLS != c.want.TLS {
			t.Errorf("%s: %+v %v, want %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
	if got := candidates([]Listener{{netip.MustParseAddr("127.0.0.1"), 2019}, {netip.IPv4Unspecified(), 80}, {netip.IPv6Unspecified(), 80}, {netip.MustParseAddr("::1"), 9}}); len(got) != 1 || got[0] != 80 {
		t.Errorf("candidates %v", got)
	}
}

func TestDetectedPortOverHTTPAndHTTPS(t *testing.T) {
	r := newRig(t)
	var probes []string
	var pmu sync.Mutex
	r.backend("app", "198.19.64.2:3000", http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("User-Agent") == UserAgent {
			pmu.Lock()
			probes = append(probes, q.Method+" "+q.URL.Path)
			pmu.Unlock()
		}
		page("app").ServeHTTP(w, q)
	}), false, false)
	r.backend("app", "198.19.64.2:9100", plainText("metrics"), false, false)
	r.backend("app", "198.19.64.2:2019", page("admin"), false, true)
	r.p.Set(api.DomainsParams{Seq: 1, HTTPS: true, Index: "198.19.4.1", Targets: []api.DomainTarget{container("app", "app", "198.19.4.2", "198.19.64.2")}})
	r.front("198.19.4.2")
	c := r.client(false)
	code, body, _ := get(t, c, "http://app.fev.local/")
	if code != 200 || !strings.HasPrefix(body, "app host=app.fev.local xff=\"\"") {
		t.Errorf("http: %d %q", code, body)
	}
	code, body, resp := get(t, c, "https://x.app.fev.local/")
	if code != 200 || !strings.HasPrefix(body, "app host=x.app.fev.local") || !strings.Contains(body, `xfp="https"`) || !strings.Contains(body, `xfh="x.app.fev.local"`) || strings.Contains(body, `xff=""`) {
		t.Errorf("https: %d %q", code, body)
	}
	if got := resp.TLS.PeerCertificates[0].DNSNames; len(got) != 1 || got[0] != "x.app.fev.local" {
		t.Errorf("the leaf names %q", got)
	}
	pmu.Lock()
	defer pmu.Unlock()
	if len(probes) == 0 || probes[0] != "HEAD /favicon.ico" {
		t.Errorf("probes seen %q", probes)
	}
}

func TestPort80AndLabels(t *testing.T) {
	r := newRig(t)
	r.backend("web", "198.19.2.2:80", page("nginx"), false, false)
	r.backend("web", "198.19.2.2:8080", page("other"), false, false)
	r.backend("lab", "198.19.2.3:5000", page("labelled"), false, false)
	r.backend("lab", "198.19.2.3:80", page("not this"), false, false)
	r.backend("sec", "198.19.2.4:8443", page("own tls"), true, false)
	lab := container("lab", "lab", "198.19.4.3", "198.19.2.3")
	lab.HTTPPort = 5000
	sec := container("sec", "sec", "198.19.4.4", "198.19.2.4")
	sec.HTTPSPort = 8443
	r.p.Set(api.DomainsParams{Seq: 1, HTTPS: true, Index: "198.19.4.1", Targets: []api.DomainTarget{container("web", "web", "198.19.4.2", "198.19.2.2"), lab, sec}})
	for _, a := range []string{"198.19.4.2", "198.19.4.3", "198.19.4.4"} {
		r.front(a)
	}
	c := r.client(false)
	for url, want := range map[string]string{
		"https://web.fev.local/": "nginx",
		"http://lab.fev.local/":  "labelled",
		"https://sec.fev.local/": "own tls",
		"http://sec.fev.local/":  "own tls",
	} {
		if code, body, _ := get(t, c, url); code != 200 || !strings.HasPrefix(body, want) {
			t.Errorf("%s: %d %q", url, code, body)
		}
	}
}

func TestAContainersOwnTLSIsReTerminated(t *testing.T) {
	r := newRig(t)
	r.backend("tls", "198.19.2.5:443", page("its own"), true, false)
	r.p.Set(api.DomainsParams{Seq: 1, HTTPS: true, Index: "198.19.4.1", Targets: []api.DomainTarget{container("tls", "tls", "198.19.4.5", "198.19.2.5")}})
	r.front("198.19.4.5")
	code, body, resp := get(t, r.client(false), "https://tls.fev.local/")
	if code != 200 || !strings.HasPrefix(body, "its own") {
		t.Fatalf("%d %q", code, body)
	}
	if resp.TLS.PeerCertificates[0].Issuer.CommonName != "test root" {
		t.Errorf("the client saw %v's certificate", resp.TLS.PeerCertificates[0].Issuer)
	}
}

func TestNoWebPortIsRefusedAtOnce(t *testing.T) {
	r := newRig(t)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("-ERR not http\r\n"))
			c.Close()
		}
	}()
	r.raw("db", "198.19.2.6:6379", l.Addr().String(), false)
	r.p.Set(api.DomainsParams{Seq: 1, HTTPS: true, Index: "198.19.4.1", Targets: []api.DomainTarget{container("db", "db", "198.19.4.6", "198.19.2.6")}})
	r.front("198.19.4.6")
	c := r.client(false)
	start := time.Now()
	if _, err := c.Get("https://db.fev.local/"); err == nil || !strings.Contains(err.Error(), "tls") {
		t.Errorf("https: %v", err)
	}
	code, body, _ := get(t, c, "http://db.fev.local/")
	if code != 502 || !strings.Contains(body, "container db has no web port") {
		t.Errorf("http: %d %q", code, body)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("refusing took %v", d)
	}
}

func TestAMachinesOwnPortIsSpliced(t *testing.T) {
	r := newRig(t)
	r.backend("u24", "198.19.0.11:80", page("machine's own"), false, false)
	r.backend("alp", "198.19.0.12:8000", page("alpine app"), false, false)
	m := func(name, addr string) api.DomainTarget {
		return api.DomainTarget{Kind: api.TargetMachine, ID: name, Name: name, Address: addr, Names: []string{"*." + name + ".fev.local", name + ".fev.local"}}
	}
	r.p.Set(api.DomainsParams{Seq: 1, HTTPS: true, Index: "198.19.4.1", Targets: []api.DomainTarget{m("u24", "198.19.0.11"), m("alp", "198.19.0.12")}})
	r.front("198.19.0.11")
	r.front("198.19.0.12")
	c := r.client(false)
	if code, body, _ := get(t, c, "http://u24.fev.local/"); code != 200 || !strings.HasPrefix(body, "machine's own") {
		t.Errorf("u24: %d %q", code, body)
	}
	if code, body, _ := get(t, c, "https://alp.fev.local/"); code != 200 || !strings.HasPrefix(body, "alpine app") {
		t.Errorf("alp: %d %q", code, body)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.unredir) != 1 || r.unredir[0] != "198.19.0.11:80" {
		t.Errorf("unredirect %q", r.unredir)
	}
}

func TestTheIndexPage(t *testing.T) {
	r := newRig(t)
	r.p.Set(api.DomainsParams{Seq: 1, HTTPS: true, Index: "198.19.4.1", Targets: []api.DomainTarget{
		container("w", "web", "198.19.4.2", "198.19.2.2"),
		{Kind: api.TargetMachine, ID: "u24", Name: "u24", Address: "198.19.0.11", Names: []string{"*.u24.fev.local", "u24.fev.local"}},
	}})
	r.front("198.19.4.1")
	for _, url := range []string{"http://fev.local/", "https://fev.local/"} {
		code, body, resp := get(t, r.client(false), url)
		if code != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d", url, code)
		}
		scheme := strings.SplitN(url, ":", 2)[0]
		for _, want := range []string{"<b>web</b>", scheme + "://web.fev.local/", "<b>u24</b>", scheme + "://u24.fev.local/"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", url, want)
			}
		}
	}
}

func TestH2AndWebSocket(t *testing.T) {
	r := newRig(t)
	ws := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("Upgrade") != "websocket" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, "proto %s", q.Proto)
			return
		}
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		line, _ := brw.ReadString('\n')
		brw.WriteString("echo " + line)
		brw.Flush()
	})
	r.backend("ws", "198.19.2.7:5173", ws, false, false)
	r.p.Set(api.DomainsParams{Seq: 1, HTTPS: true, Index: "198.19.4.1", Targets: []api.DomainTarget{container("ws", "vite", "198.19.4.7", "198.19.2.7")}})
	r.front("198.19.4.7")
	code, body, resp := get(t, r.client(true), "https://vite.fev.local/")
	if code != 200 || resp.Proto != "HTTP/2.0" || body != "proto HTTP/1.1" {
		t.Errorf("h2: %d %s %q", code, resp.Proto, body)
	}
	pool := x509.NewCertPool()
	pool.AddCert(r.ca.root)
	conn, err := tls.Dial("tcp", r.fronts["198.19.4.7"][1], &tls.Config{RootCAs: pool, ServerName: "vite.fev.local", NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET /hmr HTTP/1.1\r\nHost: vite.fev.local\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(conn)
	status, _ := br.ReadString('\n')
	if !strings.HasPrefix(status, "HTTP/1.1 101") {
		t.Fatalf("upgrade: %q", status)
	}
	for {
		h, _ := br.ReadString('\n')
		if h == "\r\n" || h == "" {
			break
		}
	}
	io.WriteString(conn, "hello\n")
	if got, _ := br.ReadString('\n'); got != "echo hello\n" {
		t.Errorf("through the upgrade: %q", got)
	}
}

func TestLeavesAreCachedAndRenewed(t *testing.T) {
	r := newRig(t)
	now := time.Now()
	var clock atomic.Pointer[time.Time]
	clock.Store(&now)
	r.p.leaves.now = func() time.Time { return *clock.Load() }
	r.ca.now = func() time.Time { return *clock.Load() }
	r.backend("w", "198.19.2.2:80", page("web"), false, false)
	r.p.Set(api.DomainsParams{Seq: 1, HTTPS: true, Index: "198.19.4.1", Targets: []api.DomainTarget{container("w", "web", "198.19.4.2", "198.19.2.2")}})
	r.front("198.19.4.2")
	pool := x509.NewCertPool()
	pool.AddCert(r.ca.root)
	hs := func(name string) (time.Duration, error) {
		start := time.Now()
		c, err := tls.Dial("tcp", r.fronts["198.19.4.2"][1], &tls.Config{RootCAs: pool, ServerName: name, Time: func() time.Time { return *clock.Load() }})
		if err == nil {
			c.Close()
		}
		return time.Since(start), err
	}
	first, err := hs("web.fev.local")
	if err != nil {
		t.Fatal(err)
	}
	cached, _ := hs("web.fev.local")
	t.Logf("first handshake %v, cached %v (the bar: 50 ms more)", first, cached)
	if first-cached > 50*time.Millisecond {
		t.Errorf("a first connection costs %v more than a cached one", first-cached)
	}
	if n := r.ca.issued.Load(); n != 1 {
		t.Errorf("%d leaves issued for one name", n)
	}
	if _, err := hs("other.fev.local"); err == nil {
		t.Error("a name that is not the target's was served")
	}
	later := now.Add(api.LeafLifetime * 2 / 3).Add(time.Minute)
	clock.Store(&later)
	if _, err := hs("web.fev.local"); err != nil {
		t.Fatal(err)
	}
	if n := r.ca.issued.Load(); n != 2 {
		t.Errorf("a leaf past two thirds of its life was not renewed (%d issued)", n)
	}
	for i := 0; i < MaxLeaves+5; i++ {
		if _, err := r.p.leaves.get(context.Background(), fmt.Sprintf("n%d.web.fev.local", i)); err != nil {
			t.Fatal(err)
		}
	}
	if n := r.p.leaves.size(); n != MaxLeaves {
		t.Errorf("%d leaves cached", n)
	}
}

func TestSetsAndRelearning(t *testing.T) {
	r := newRig(t)
	r.backend("a", "198.19.2.2:3000", page("three thousand"), false, false)
	r.p.Set(api.DomainsParams{Seq: 2, HTTPS: true, Index: "198.19.4.1", Targets: []api.DomainTarget{container("a", "a", "198.19.4.2", "198.19.2.2")}})
	r.front("198.19.4.2")
	c := r.client(false)
	if _, body, _ := get(t, c, "http://a.fev.local/"); !strings.HasPrefix(body, "three thousand") {
		t.Fatalf("%q", body)
	}
	r.p.Set(api.DomainsParams{Seq: 1, HTTPS: true, Index: "198.19.4.1"})
	if code, _, _ := get(t, c, "http://a.fev.local/"); code != 200 {
		t.Error("an older set was applied")
	}
	r.backend("a2", "198.19.2.9:4000", page("four thousand"), false, false)
	tg := container("a2", "a", "198.19.4.2", "198.19.2.9")
	r.p.Set(api.DomainsParams{Seq: 3, HTTPS: true, Index: "198.19.4.1", Targets: []api.DomainTarget{tg}})
	if _, body, _ := get(t, c, "http://a.fev.local/"); !strings.HasPrefix(body, "four thousand") {
		t.Errorf("after a recreation: %q", body)
	}
}

type flakyListener struct {
	net.Listener
	failed atomic.Bool
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if !l.failed.Swap(true) {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: fmt.Errorf("too many open files")}
	}
	return l.Listener.Accept()
}

func TestAnAcceptErrorDoesNotStopTheFront(t *testing.T) {
	r := newRig(t)
	r.backend("w", "198.19.2.2:80", page("web"), false, false)
	r.p.Set(api.DomainsParams{Seq: 1, HTTPS: true, Index: "198.19.4.1", Targets: []api.DomainTarget{container("w", "web", "198.19.4.2", "198.19.2.2")}})
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { l.Close() })
	r.mu.Lock()
	r.origin[l.Addr().String()] = netip.MustParseAddrPort("198.19.4.2:80")
	r.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- r.p.ServeHTTP80(&flakyListener{Listener: l}) }()
	r.fronts["198.19.4.2"] = [2]string{l.Addr().String(), ""}
	if code, body, _ := get(t, r.client(false), "http://web.fev.local/"); code != 200 || !strings.HasPrefix(body, "web") {
		t.Errorf("after an accept error: %d %q", code, body)
	}
	l.Close()
	if err := <-done; !errors.Is(err, net.ErrClosed) {
		t.Errorf("the front ended with %v", err)
	}
}

func csrPEM(t *testing.T, names ...string) string {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: names}, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func TestAContainerOrAMachineAsksForALeafOfItsOwn(t *testing.T) {
	r := newRig(t)
	r.front("198.19.4.1")
	c := r.client(false)
	post := func(body string) (int, string) {
		t.Helper()
		resp, err := c.Post("http://fev.local/csr", "application/pkcs10", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	set := func(https bool, ts ...api.DomainTarget) {
		r.p.Set(api.DomainsParams{HTTPS: https, Index: "198.19.4.1", CA: "ROOT PEM\n", Targets: ts})
	}
	set(true, container("c0ffee", "web", "198.19.4.2", "127.0.0.1"), container("other", "other", "198.19.4.3", "198.19.64.9"))
	code, body := post(csrPEM(t, "web.fev.local"))
	if code != 200 || !strings.HasPrefix(body, "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("a container's request: %d %s", code, body)
	}
	set(true, api.DomainTarget{Kind: api.TargetMachine, ID: "u24", Name: "u24", Address: "127.0.0.1", Names: []string{"*.u24.fev.local", "u24.fev.local"}})
	if code, body := post(csrPEM(t, "u24.fev.local")); code != 200 {
		t.Fatalf("a machine's request: %d %s", code, body)
	}
	r.ca.mu.Lock()
	asked := append([]string(nil), r.ca.asked...)
	r.ca.mu.Unlock()
	if len(asked) != 2 || asked[0] != "c0ffee" || asked[1] != "machine:u24" {
		t.Fatalf("requesters %q", asked)
	}
	set(true, container("other", "other", "198.19.4.3", "198.19.64.9"))
	if code, body := post(csrPEM(t, "other.fev.local")); code != 403 || !strings.Contains(body, "127.0.0.1 is not") {
		t.Fatalf("a stranger's request: %d %s", code, body)
	}
	set(true, container("c0ffee", "web", "198.19.4.2", "127.0.0.1"))
	if code, _ := post("not a request"); code != 400 {
		t.Fatalf("no CSR: %d", code)
	}
	if code, _, _ := get(t, c, "http://fev.local/csr"); code != 405 {
		t.Fatalf("GET: %d", code)
	}
	if code, body, _ := get(t, c, "http://fev.local/ca.pem"); code != 200 || body != "ROOT PEM\n" {
		t.Fatalf("the root: %d %q", code, body)
	}
	set(false, container("c0ffee", "web", "198.19.4.2", "127.0.0.1"))
	if code, _ := post(csrPEM(t, "web.fev.local")); code != 503 {
		t.Fatalf("HTTPS off: %d", code)
	}
	r.ca.mu.Lock()
	n := len(r.ca.asked)
	r.ca.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d requests reached the daemon, want 2", n)
	}
}

func TestANewRootDropsTheCachedLeaves(t *testing.T) {
	r := newRig(t)
	r.backend("w", "198.19.64.2:80", page("hello"), false, false)
	set := func(ca string) {
		r.p.Set(api.DomainsParams{HTTPS: true, Index: "198.19.4.1", CA: ca, Targets: []api.DomainTarget{container("w", "web", "198.19.4.2", "198.19.64.2")}})
	}
	set("ROOT A")
	r.front("198.19.4.2")
	fetch := func() {
		t.Helper()
		c := r.client(false)
		if code, _, _ := get(t, c, "https://web.fev.local/"); code != 200 {
			t.Fatalf("GET: %d", code)
		}
	}
	fetch()
	set("ROOT A")
	fetch()
	if n := r.ca.issued.Load(); n != 1 {
		t.Fatalf("%d leaves for one root", n)
	}
	set("ROOT B")
	fetch()
	if n := r.ca.issued.Load(); n != 2 {
		t.Fatalf("%d leaves after a new root, want 2", n)
	}
	r.p.Set(api.DomainsParams{HTTPS: false, Index: "198.19.4.1", Targets: []api.DomainTarget{container("w", "web", "198.19.4.2", "198.19.64.2")}})
	set("ROOT C")
	fetch()
	if n := r.ca.issued.Load(); n != 3 {
		t.Fatalf("%d leaves after a root changed while HTTPS was off, want 3", n)
	}
}

func TestAMachinesServerStartedLaterIsSplicedTo(t *testing.T) {
	r := newRig(t)
	r.backend("alp", "198.19.0.12:8000", page("alpine app"), false, false)
	m := api.DomainTarget{Kind: api.TargetMachine, ID: "alp", Name: "alp", Address: "198.19.0.12", Names: []string{"alp.fev.local"}}
	r.p.Set(api.DomainsParams{Seq: 1, HTTPS: true, Index: "198.19.4.1", Targets: []api.DomainTarget{m}})
	r.front("198.19.0.12")
	c := r.client(false)
	if code, body, _ := get(t, c, "http://alp.fev.local/"); code != 200 || !strings.HasPrefix(body, "alpine app") {
		t.Fatalf("before: %d %q", code, body)
	}
	r.backend("alp", "198.19.0.12:80", page("its own 80"), false, false)
	c.CloseIdleConnections()
	if code, body, _ := get(t, c, "http://alp.fev.local/"); code != 200 || !strings.HasPrefix(body, "its own 80") {
		t.Errorf("after: %d %q", code, body)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.unredir) != 1 || r.unredir[0] != "198.19.0.12:80" {
		t.Errorf("unredirect %q", r.unredir)
	}
}

func TestTheFrontTakesInitsListeners(t *testing.T) {
	var files []*os.File
	var addrs []string
	for i := 0; i < 2; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		f, err := l.(*net.TCPListener).File()
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
		t.Cleanup(func() { f.Close() })
		files = append(files, f)
		addrs = append(addrs, l.Addr().String())
	}
	early, err := net.Dial("tcp", addrs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer early.Close()
	l80, l443, err := Listeners(fmt.Sprintf("%d,%d", files[0].Fd(), files[1].Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer l80.Close()
	defer l443.Close()
	if l80.Addr().String() != addrs[0] || l443.Addr().String() != addrs[1] {
		t.Fatalf("listeners %s %s, want %s %s", l80.Addr(), l443.Addr(), addrs[0], addrs[1])
	}
	c, err := l80.Accept()
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	for _, bad := range []string{"4", "4,4", "x,5", "1,2", "4;5"} {
		if _, _, err := Listeners(bad); err == nil {
			t.Errorf("%q taken", bad)
		}
	}
}

func TestARequestForAnotherHostIsRefused(t *testing.T) {
	r := newRig(t)
	var served atomic.Int32
	r.backend("app", "198.19.64.2:80", http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		served.Add(1)
		page("app").ServeHTTP(w, q)
	}), false, false)
	r.p.Set(api.DomainsParams{Seq: 1, HTTPS: true, Index: "198.19.4.1", Targets: []api.DomainTarget{container("app", "app", "198.19.4.2", "198.19.64.2")}})
	r.front("198.19.4.2")
	r.front("198.19.4.1")
	c := r.client(false)
	do := func(url, host string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", url, nil)
		req.Host = host
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%s Host %s: %v", url, host, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, tc := range []struct {
		url, host string
		want      int
	}{
		{"http://app.fev.local/", "rebound.attacker.example", http.StatusMisdirectedRequest},
		{"https://app.fev.local/", "rebound.attacker.example:443", http.StatusMisdirectedRequest},
		{"http://app.fev.local/", "198.19.4.9", http.StatusMisdirectedRequest},
		{"http://fev.local/", "rebound.attacker.example", http.StatusMisdirectedRequest},
		{"http://app.fev.local/", "app.fev.local", 200},
		{"http://app.fev.local/", "APP.fev.local.:80", 200},
		{"https://app.fev.local/", "x.app.fev.local", 200},
		{"http://app.fev.local/", "198.19.4.2", 200},
		{"http://fev.local/", "fev.local", 200},
		{"http://fev.local/", "198.19.4.1:80", 200},
	} {
		if got := do(tc.url, tc.host); got != tc.want {
			t.Errorf("%s with Host %s: %d, want %d", tc.url, tc.host, got, tc.want)
		}
	}
	if n := served.Load(); n != 4 {
		t.Errorf("the backend served %d requests, want 4", n)
	}
}

func (r *rig) clientAt(at map[string]string) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(r.ca.root)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, _ := net.SplitHostPort(addr)
			pair, ok := r.fronts[at[host]]
			if !ok {
				return nil, fmt.Errorf("no front for %s", host)
			}
			to := pair[0]
			if port == "443" {
				to = pair[1]
			}
			var d net.Dialer
			return d.DialContext(ctx, network, to)
		},
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	r.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func TestANamesIPv6AddressIsTheNamesOwn(t *testing.T) {
	r := newRig(t)
	r.backend("w", "198.19.2.2:80", page("web's"), false, false)
	r.backend("u24", "[fd5b:6e2a:91c0:1::b]:80", page("u24's own over IPv6"), false, false)
	r.backend("u24", "198.19.0.11:80", page("u24's own"), false, false)
	m := api.DomainTarget{Kind: api.TargetMachine, ID: "u24", Name: "u24", Address: "198.19.0.11", Address6: "fd5b:6e2a:91c0:1::b", Names: []string{"*.u24.fev.local", "u24.fev.local"}}
	r.p.Set(api.DomainsParams{Seq: 1, HTTPS: true, Index: "198.19.4.1", Targets: []api.DomainTarget{container("w", "web", "198.19.4.2", "198.19.2.2"), m}})
	r.p.SetNames6(netip.MustParsePrefix("fd5b:6e2a:91c0:3::/64"))
	at := map[string]string{
		"web.fev.local":   "fd5b:6e2a:91c0:3::c613:402",
		"fev.local":       "fd5b:6e2a:91c0:3::c613:401",
		"u24.fev.local":   "fd5b:6e2a:91c0:1::b",
		"gone.fev.local":  "fd5b:6e2a:91c0:3::c613:4ff",
		"other.fev.local": "fd5b:6e2a:91c0:4::c613:402",
	}
	for _, a := range at {
		r.front(a)
	}
	c := r.clientAt(at)
	for _, url := range []string{"http://web.fev.local/", "https://web.fev.local/"} {
		if code, body, _ := get(t, c, url); code != 200 || !strings.HasPrefix(body, "web's") {
			t.Errorf("%s: %d %q", url, code, body)
		}
	}
	if code, body, _ := get(t, c, "https://fev.local/"); code != 200 || !strings.Contains(body, "<b>web</b>") {
		t.Errorf("the index: %d %q", code, body)
	}
	if code, body, _ := get(t, c, "http://u24.fev.local/"); code != 200 || !strings.HasPrefix(body, "u24's own over IPv6") {
		t.Errorf("u24: %d %q", code, body)
	}
	for _, url := range []string{"http://gone.fev.local/", "http://other.fev.local/"} {
		if _, err := c.Get(url); err == nil {
			t.Errorf("%s answered", url)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.unredir) != 1 || r.unredir[0] != "198.19.0.11:80" {
		t.Errorf("unredirect %q: a machine's port is its IPv4 address's", r.unredir)
	}
}

func TestTheFrontTakesInitsIPv6PairToo(t *testing.T) {
	h, err := webctl.Hold(netip.MustParseAddr("127.0.0.1"), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := h.Also(netip.IPv6Loopback(), 0, 0); err != nil {
		t.Skip("no IPv6 loopback:", err)
	}
	files := h.Files()
	if len(files) != 4 || h.FDs(4) != "4,5,6,7" {
		t.Fatalf("%d files, FDs %q", len(files), h.FDs(4))
	}
	var fds []string
	for _, f := range files {
		fds = append(fds, strconv.Itoa(int(f.Fd())))
	}
	pairs, err := ListenerPairs(strings.Join(fds, ","))
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 {
		t.Fatalf("%d pairs", len(pairs))
	}
	for i, family := range []string{"127.0.0.1:", "[::1]:"} {
		for j := range 2 {
			if a := pairs[i][j].Addr().String(); !strings.HasPrefix(a, family) {
				t.Errorf("pair %d listener %d at %s", i, j, a)
			}
			defer pairs[i][j].Close()
		}
	}
	c, err := net.Dial("tcp", pairs[1][0].Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	for _, bad := range []string{"", "4,5,6", "4,5,6,7,8,9"} {
		if _, err := ListenerPairs(bad); err == nil {
			t.Errorf("%q taken", bad)
		}
	}
}
