package webproxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strings"
	"sync"
	"time"

	api "fev/proto/agent"
)

type Lister interface {
	Listening(t api.DomainTarget) ([]Listener, error)
}

type Config struct {
	OriginalDst func(net.Conn) (netip.AddrPort, error)
	Lister      Lister
	Dial        func(ctx context.Context, network, addr string) (net.Conn, error)
	Issuer      Issuer
	Unredirect  func(netip.AddrPort)
	Logf        func(format string, args ...any)
	Now         func() time.Time
}

type Proxy struct {
	cfg    Config
	leaves *leaves

	mu       sync.Mutex
	set      api.DomainsParams
	root     string
	targets  map[netip.Addr]*entry
	targets6 map[netip.Addr]*entry
	names6   netip.Prefix
	conns    sync.Map

	front, plain *http.Server
	frontL       *chanListener
	plainL       *chanListener
	transport    *http.Transport
}

type entry struct {
	t     api.DomainTarget
	once  sync.Mutex
	web   *WebPort
	why   error
	spare map[int]bool
}

type connInfo struct {
	dst netip.AddrPort
	e   *entry
	tls bool
}

type ctxKey struct{}

func New(cfg Config) *Proxy {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Dial == nil {
		d := &net.Dialer{}
		cfg.Dial = d.DialContext
	}
	p := &Proxy{cfg: cfg, targets: map[netip.Addr]*entry{}}
	p.leaves = newLeaves(cfg.Issuer, cfg.Now)
	p.transport = &http.Transport{
		DialContext:           func(ctx context.Context, network, addr string) (net.Conn, error) { return cfg.Dial(ctx, network, addr) },
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		ForceAttemptHTTP2:     false,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 0,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	handler := http.HandlerFunc(p.serveHTTP)
	ctxOf := func(ctx context.Context, c net.Conn) context.Context {
		if v, ok := p.conns.LoadAndDelete(c); ok {
			return context.WithValue(ctx, ctxKey{}, v.(connInfo))
		}
		return ctx
	}
	p.frontL, p.plainL = newChanListener(), newChanListener()
	quiet := log.New(io.Discard, "", 0)
	p.front = &http.Server{Handler: handler, ConnContext: ctxOf, IdleTimeout: 2 * time.Minute, ReadHeaderTimeout: 30 * time.Second, ErrorLog: quiet}
	p.plain = &http.Server{Handler: handler, ConnContext: ctxOf, IdleTimeout: 2 * time.Minute, ReadHeaderTimeout: 30 * time.Second, ErrorLog: quiet}
	p.front.Protocols = new(http.Protocols)
	p.front.Protocols.SetHTTP1(true)
	p.front.Protocols.SetHTTP2(true)
	go p.front.Serve(p.frontL)
	go p.plain.Serve(p.plainL)
	return p
}

func (p *Proxy) Set(set api.DomainsParams) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if set.Seq != 0 && set.Seq < p.set.Seq {
		return
	}
	next := make(map[netip.Addr]*entry, len(set.Targets))
	next6 := map[netip.Addr]*entry{}
	for _, t := range set.Targets {
		a, err := netip.ParseAddr(t.Address)
		if err != nil {
			continue
		}
		e := &entry{t: t}
		if old, ok := p.targets[a]; ok && sameTarget(old.t, t) {
			e = old
		}
		next[a] = e
		if a6, err := netip.ParseAddr(t.Address6); err == nil && a6.Is6() {
			next6[a6] = e
		}
	}
	if set.CA != "" {
		if p.root != "" && set.CA != p.root {
			p.leaves.flush()
		}
		p.root = set.CA
	}
	p.set, p.targets, p.targets6 = set, next, next6
}

func (p *Proxy) SetNames6(n netip.Prefix) {
	p.mu.Lock()
	p.names6 = n.Masked()
	p.mu.Unlock()
}

func sameTarget(a, b api.DomainTarget) bool {
	return a.Kind == b.Kind && a.ID == b.ID && a.Address == b.Address && a.Target == b.Target && a.NetNS == b.NetNS &&
		a.Address6 == b.Address6 && a.Target6 == b.Target6 &&
		a.HTTPPort == b.HTTPPort && a.HTTPSPort == b.HTTPSPort && strings.Join(a.Names, ",") == strings.Join(b.Names, ",") &&
		strings.Join(a.Exposed, ",") == strings.Join(b.Exposed, ",") &&
		a.Redirect80 == b.Redirect80 && a.Own443 == b.Own443 && a.Pass443 == b.Pass443 && strings.Join(a.Hosts, ",") == strings.Join(b.Hosts, ",")
}

func (p *Proxy) lookup(a netip.Addr) (e *entry, index bool, https bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a.Is6() && !a.Is4In6() {
		if e, ok := p.targets6[a]; ok {
			return e, false, p.set.HTTPS
		}
		b := a.As16()
		if !p.names6.IsValid() || !p.names6.Contains(a) || b[8]|b[9]|b[10]|b[11] != 0 {
			return nil, false, p.set.HTTPS
		}
		a = netip.AddrFrom4([4]byte(b[12:16]))
	}
	a = a.Unmap()
	if p.set.Index != "" && a.String() == p.set.Index {
		return nil, true, p.set.HTTPS
	}
	return p.targets[a], false, p.set.HTTPS
}

func (p *Proxy) ServeHTTP80(l net.Listener) error { return p.accept(l, false) }

func (p *Proxy) ServeHTTPS(l net.Listener) error { return p.accept(l, true) }

func (p *Proxy) accept(l net.Listener, secure bool) error {
	var delay time.Duration
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else if delay *= 2; delay > time.Second {
				delay = time.Second
			}
			p.cfg.Logf("webproxy: accept: %v; again in %v", err, delay)
			time.Sleep(delay)
			continue
		}
		delay = 0
		go p.handle(c, secure)
	}
}

func (p *Proxy) handle(c net.Conn, secure bool) {
	dst, err := p.cfg.OriginalDst(c)
	if err != nil {
		p.cfg.Logf("webproxy: a connection with no original destination: %v", err)
		c.Close()
		return
	}
	e, index, https := p.lookup(dst.Addr())
	if !index && e == nil {
		c.Close()
		return
	}
	if secure && !https {
		c.Close()
		return
	}
	if e != nil && e.t.Kind == api.TargetMachine && p.machineListens(e, int(dst.Port())) {
		p.splice(c, dst)
		return
	}
	if secure && e != nil && e.t.Kind == api.TargetK8s && e.t.Pass443 {
		var spliced bool
		if c, spliced = p.passOrKeep(c, e, dst); spliced {
			return
		}
	}
	info := connInfo{dst: dst, e: e, tls: secure}
	if !secure {
		p.conns.Store(c, info)
		p.plainL.put(c)
		return
	}
	tc := tls.Server(c, &tls.Config{
		NextProtos: []string{"h2", "http/1.1"},
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return p.certificate(hello, info)
		},
	})
	p.conns.Store(net.Conn(tc), info)
	p.frontL.put(tc)
}

func (p *Proxy) machineListens(e *entry, port int) bool {
	e.once.Lock()
	defer e.once.Unlock()
	if e.spare == nil {
		e.spare = map[int]bool{}
	}
	if e.spare[port] {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeWait)
	defer cancel()
	c, err := p.cfg.Dial(ctx, "tcp", netip.AddrPortFrom(netip.MustParseAddr(e.t.Address), uint16(port)).String())
	listens := err == nil
	if c != nil {
		c.Close()
	}
	e.spare[port] = listens
	if listens && p.cfg.Unredirect != nil {
		p.cfg.Unredirect(netip.AddrPortFrom(netip.MustParseAddr(e.t.Address), uint16(port)))
	}
	return listens
}

func (p *Proxy) passOrKeep(c net.Conn, e *entry, dst netip.AddrPort) (net.Conn, bool) {
	rec := &recorder{Conn: c}
	var sni string
	seen := false
	c.SetReadDeadline(time.Now().Add(probeWait * 4))
	tls.Server(rec, &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		sni, seen = h.ServerName, true
		return nil, errPeeked
	}}).Handshake()
	c.SetReadDeadline(time.Time{})
	kept := &prefixed{Conn: c, head: rec.buf}
	if !seen {
		return kept, false
	}
	if sni == "" {
		return kept, false
	}
	if _, ok := nameOf(e, sni); ok {
		return kept, false
	}
	p.spliceWith(kept, dst)
	return nil, true
}

var errPeeked = errors.New("the hello was read")

type recorder struct {
	net.Conn
	buf []byte
}

func (r *recorder) Read(b []byte) (int, error) {
	n, err := r.Conn.Read(b)
	r.buf = append(r.buf, b[:n]...)
	return n, err
}

func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }

type prefixed struct {
	net.Conn
	head []byte
}

func (c *prefixed) Read(b []byte) (int, error) {
	if len(c.head) > 0 {
		n := copy(b, c.head)
		c.head = c.head[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

func (c *prefixed) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

func (p *Proxy) splice(c net.Conn, dst netip.AddrPort) { p.spliceWith(c, dst) }

func (p *Proxy) spliceWith(c net.Conn, dst netip.AddrPort) {
	ctx, cancel := context.WithTimeout(context.Background(), probeWait)
	b, err := p.cfg.Dial(ctx, "tcp", dst.String())
	cancel()
	if err != nil {
		c.Close()
		return
	}
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go cp(b, c)
	go cp(c, b)
	<-done
	<-done
	c.Close()
	b.Close()
}

func nameOf(e *entry, sni string) (string, bool) {
	sni = strings.ToLower(strings.TrimSuffix(sni, "."))
	if sni == "" {
		for _, n := range e.t.Names {
			if !strings.HasPrefix(n, "*.") {
				return n, true
			}
		}
		return "", false
	}
	for _, n := range e.t.Names {
		if n == sni {
			return sni, true
		}
		if parent, ok := strings.CutPrefix(n, "*."); ok && (sni == parent || strings.HasSuffix(sni, "."+parent)) {
			return sni, true
		}
	}
	return "", false
}

func (p *Proxy) certificate(hello *tls.ClientHelloInfo, info connInfo) (*tls.Certificate, error) {
	name := "fev.local"
	if info.e != nil {
		n, ok := nameOf(info.e, hello.ServerName)
		if !ok {
			return nil, fmt.Errorf("%q is not a name of %s", hello.ServerName, info.dst.Addr())
		}
		name = n
		if _, err := p.webPort(hello.Context(), info.e, name); err != nil {
			return nil, err
		}
	} else if s := strings.ToLower(strings.TrimSuffix(hello.ServerName, ".")); s != "" && s != "fev.local" {
		return nil, fmt.Errorf("%q is not fev.local", hello.ServerName)
	}
	return p.leaves.get(hello.Context(), name)
}

func (p *Proxy) webPort(ctx context.Context, e *entry, name string) (WebPort, error) {
	e.once.Lock()
	defer e.once.Unlock()
	if e.web != nil {
		return *e.web, nil
	}
	host := backendHost(e.t)
	w, err := p.detect(ctx, e.t, host, name)
	if err != nil {
		p.cfg.Logf("webproxy: %v", err)
		return WebPort{}, err
	}
	p.cfg.Logf("webproxy: %s serves the web on %d (tls %v): %s", e.t.Name, w.Port, w.TLS, w.Why)
	e.web = &w
	return w, nil
}

func (e *entry) forget() {
	e.once.Lock()
	e.web = nil
	e.once.Unlock()
}

func backendHost(t api.DomainTarget) netip.Addr {
	if t.Target != "" {
		if a, err := netip.ParseAddr(t.Target); err == nil {
			return a
		}
	}
	a, _ := netip.ParseAddr(t.Address)
	return a
}

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	info, ok := r.Context().Value(ctxKey{}).(connInfo)
	if !ok {
		http.Error(w, "fev: no route", http.StatusBadGateway)
		return
	}
	if info.e == nil {
		if !hostAllowed(info, nil, r.Host) {
			misdirected(w, r.Host)
			return
		}
		p.serveIndex(w, r)
		return
	}
	e, _, _ := p.lookup(info.dst.Addr())
	if e == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "fev: %s has no target now\n", info.dst.Addr())
		return
	}
	info.e = e
	if !hostAllowed(info, e, r.Host) {
		misdirected(w, r.Host)
		return
	}
	name, _ := nameOf(info.e, r.Host)
	if name == "" {
		name, _ = nameOf(info.e, "")
	}
	web, err := p.webPort(r.Context(), info.e, name)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "fev: %s has no web port\n", targetLabel(info.e.t))
		return
	}
	scheme := "http"
	if web.TLS {
		scheme = "https"
	}
	backend := netip.AddrPortFrom(backendHost(info.e.t), uint16(web.Port)).String()
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = scheme
			pr.Out.URL.Host = backend
			pr.Out.Host = pr.In.Host
			if info.tls {
				pr.SetXForwarded()
			}
		},
		Transport: p.transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			info.e.forget()
			if r.Context().Err() == nil {
				p.cfg.Logf("webproxy: %s on %s: %v", targetLabel(info.e.t), backend, err)
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "fev: %s did not answer on port %d\n", targetLabel(info.e.t), web.Port)
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
	rp.ServeHTTP(w, r)
}

func hostAllowed(info connInfo, e *entry, host string) bool {
	h := strings.TrimSpace(host)
	if hp, _, err := net.SplitHostPort(h); err == nil {
		h = hp
	}
	h = strings.ToLower(strings.TrimSuffix(strings.Trim(h, "[]"), "."))
	if h == "" {
		return true
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Unmap() == info.dst.Addr().Unmap()
	}
	if e == nil {
		return h == "fev.local"
	}
	if _, ok := nameOf(e, h); ok {
		return true
	}
	return e.t.Kind == api.TargetK8s && ingressHost(e.t.Hosts, h)
}

func ingressHost(hosts []string, h string) bool {
	for _, x := range hosts {
		if x == h {
			return true
		}
		if parent, ok := strings.CutPrefix(x, "*."); ok {
			if label, rest, ok := strings.Cut(h, "."); ok && label != "" && rest == parent {
				return true
			}
		}
	}
	return false
}

func misdirected(w http.ResponseWriter, host string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusMisdirectedRequest)
	fmt.Fprintf(w, "fev: %q is not a name of this address\n", host)
}

func targetLabel(t api.DomainTarget) string {
	if t.Kind == api.TargetMachine {
		return "machine " + t.Name
	}
	if t.Kind == api.TargetK8s {
		return "service " + t.Name
	}
	return "container " + t.Name
}

func (p *Proxy) Close() {
	p.frontL.Close()
	p.plainL.Close()
	p.front.Close()
	p.plain.Close()
	p.transport.CloseIdleConnections()
}

type chanListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newChanListener() *chanListener {
	return &chanListener{ch: make(chan net.Conn), done: make(chan struct{})}
}

func (l *chanListener) put(c net.Conn) {
	select {
	case l.ch <- c:
	case <-l.done:
		c.Close()
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{} }
