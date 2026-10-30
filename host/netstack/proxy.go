package netstack

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"fev/proto/netplan"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

type UpstreamKind int

const (
	Direct UpstreamKind = iota
	ViaHTTP
	ViaHTTPS
	ViaSOCKS5
)

type Upstream struct {
	Kind        UpstreamKind
	Addr        string
	User        *url.Userinfo
	RemoteNames bool
}

func (u Upstream) String() string {
	switch u.Kind {
	case ViaHTTP:
		return "http proxy " + u.Addr
	case ViaHTTPS:
		return "https proxy " + u.Addr
	case ViaSOCKS5:
		return "socks5 proxy " + u.Addr
	}
	return "direct"
}

type ProxyDecider interface {
	Decide(ctx context.Context, url string) ([]Upstream, error)
}

type ProxyOptions struct {
	Setting     func() string
	Exclude     func() []string
	System      ProxyDecider
	Transparent func() bool
	LANs        func() []netip.Prefix
}

type ProxyPresence interface {
	Configured() bool
}

func ParseProxySetting(s string) (Upstream, bool, error) {
	switch strings.TrimSpace(s) {
	case "", "auto":
		return Upstream{}, false, nil
	case "none":
		return Upstream{Kind: Direct}, true, nil
	}
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" {
		return Upstream{}, false, errors.New("network.proxy is auto, none, or a URL such as http://proxy.example:3128")
	}
	up := Upstream{User: u.User}
	defaults := map[string]int{"http": 80, "https": 443, "socks5": 1080, "socks5h": 1080}
	port, ok := defaults[u.Scheme]
	if !ok {
		return Upstream{}, false, fmt.Errorf("network.proxy: %s is not one of http, https, socks5, socks5h", u.Scheme)
	}
	switch u.Scheme {
	case "http":
		up.Kind = ViaHTTP
	case "https":
		up.Kind = ViaHTTPS
	case "socks5":
		up.Kind = ViaSOCKS5
	case "socks5h":
		up.Kind, up.RemoteNames = ViaSOCKS5, true
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return Upstream{}, false, fmt.Errorf("network.proxy: port %q", p)
		}
		port = n
	}
	if u.Path != "" && u.Path != "/" {
		return Upstream{}, false, errors.New("network.proxy: a proxy URL has no path")
	}
	up.Addr = hostPort(u.Hostname(), port)
	return up, true, nil
}

func MaskProxySetting(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.User == nil {
		return s
	}
	return u.Redacted()
}

func hostPort(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }

const proxyCacheMax = 1024

type proxyEndpoint struct {
	s      *Stack
	opts   ProxyOptions
	server *http.Server

	mu       sync.Mutex
	cache    map[string][]Upstream
	presence int
}

var proxyDialTimeout = 30 * time.Second

func newProxyEndpoint(s *Stack, opts *ProxyOptions) (*proxyEndpoint, error) {
	p := &proxyEndpoint{s: s, cache: map[string][]Upstream{}}
	if opts != nil {
		p.opts = *opts
	}
	if p.opts.System == nil {
		p.opts.System = SystemProxies()
	}
	l, err := gonet.ListenTCP(s.stack, tcpip.FullAddress{NIC: nicID, Addr: toAddr(s.cfg.Gateway), Port: netplan.ProxyPort}, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("netstack: the proxy endpoint: %w", err)
	}
	p.server = &http.Server{Handler: p, ErrorLog: log.New(io.Discard, "", 0)}
	go p.server.Serve(l)
	return p, nil
}

func (p *proxyEndpoint) close() { p.server.Close() }

func (p *proxyEndpoint) invalidate() {
	p.mu.Lock()
	p.cache = map[string][]Upstream{}
	p.presence = 0
	p.mu.Unlock()
}

func (s *Stack) InvalidateProxyDecisions() {
	s.mu.Lock()
	p := s.proxy
	s.mu.Unlock()
	if p != nil {
		p.invalidate()
	}
}

func (p *proxyEndpoint) decide(ctx context.Context, scheme, host string, port uint16) ([]Upstream, error) {
	if excluded(host, p.exclude()) {
		return []Upstream{{Kind: Direct}}, nil
	}
	setting := ""
	if p.opts.Setting != nil {
		setting = p.opts.Setting()
	}
	if up, fixed, err := ParseProxySetting(setting); err != nil {
		return nil, err
	} else if fixed {
		return []Upstream{up}, nil
	}
	target := decisionKey(scheme, host, port)
	p.mu.Lock()
	ups, ok := p.cache[target]
	p.mu.Unlock()
	if ok {
		return ups, nil
	}
	ups, err := p.opts.System.Decide(ctx, target)
	if err != nil {
		return nil, err
	}
	p.remember(target, ups)
	return ups, nil
}

func decisionKey(scheme, host string, port uint16) string {
	return scheme + "://" + hostPort(host, int(port)) + "/"
}

func (p *proxyEndpoint) remember(target string, ups []Upstream) {
	p.mu.Lock()
	if len(p.cache) >= proxyCacheMax {
		p.cache = map[string][]Upstream{}
	}
	p.cache[target] = ups
	p.mu.Unlock()
}

func (p *proxyEndpoint) exclude() []string {
	if p.opts.Exclude == nil {
		return nil
	}
	return p.opts.Exclude()
}

func excluded(host string, list []string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	addr, err := netip.ParseAddr(host)
	isAddr := err == nil
	for _, e := range list {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if isAddr {
			if pf, err := netip.ParsePrefix(e); err == nil && pf.Contains(addr.Unmap()) {
				return true
			}
			if a, err := netip.ParseAddr(e); err == nil && a.Unmap() == addr.Unmap() {
				return true
			}
			continue
		}
		domain := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(e, "*"), "."), ".")
		if domain != "" && (host == domain || strings.HasSuffix(host, "."+domain)) {
			return true
		}
	}
	return false
}

func (p *proxyEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "this is fev's proxy endpoint: send requests through it, in absolute form, or CONNECT", http.StatusBadRequest)
		return
	}
	p.forward(w, r)
}

func (p *proxyEndpoint) fail(w http.ResponseWriter, code int, target string, err error) {
	p.s.log.Debug("proxy endpoint: request failed", "target", target, "err", err)
	http.Error(w, fmt.Sprintf("fev's proxy endpoint could not reach %s: %v", target, err), code)
}

var errRefused = errors.New("refused: fev's own addresses and the Mac's loopback are not reached through the proxy endpoint")

var errProxyRefused = errors.New("the proxy refused")

func (p *proxyEndpoint) connect(w http.ResponseWriter, r *http.Request) {
	host, portText, err := net.SplitHostPort(r.Host)
	port, perr := strconv.Atoi(portText)
	if err != nil || perr != nil || port <= 0 || port > 65535 {
		http.Error(w, "CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(p.s.ctx, proxyDialTimeout)
	defer cancel()
	scheme := "https"
	if port == 80 {
		scheme = "http"
	}
	conn, err := p.dial(ctx, scheme, host, uint16(port))
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, errRefused) {
			code = http.StatusForbidden
		}
		p.fail(w, code, r.Host, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		conn.Close()
		http.Error(w, "cannot take the connection over", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		conn.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		client.Close()
		conn.Close()
		return
	}
	if n := buf.Reader.Buffered(); n > 0 {
		head, _ := buf.Reader.Peek(n)
		if _, err := conn.Write(head); err != nil {
			client.Close()
			conn.Close()
			return
		}
	}
	stop := context.AfterFunc(p.s.ctx, func() {
		client.Close()
		conn.Close()
	})
	defer stop()
	splice(client, conn)
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *proxyEndpoint) forward(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Hostname()
	port := 80
	if r.URL.Scheme == "https" {
		port = 443
	}
	if ps := r.URL.Port(); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n <= 0 || n > 65535 {
			http.Error(w, "bad port", http.StatusBadRequest)
			return
		}
		port = n
	}
	if r.URL.Scheme != "http" {
		http.Error(w, "fev's proxy endpoint carries https by CONNECT", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), proxyDialTimeout)
	defer cancel()
	ups, err := p.decide(ctx, "http", host, uint16(port))
	if err != nil {
		p.fail(w, http.StatusBadGateway, r.URL.Host, err)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	if c := r.Header.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			out.Header.Del(strings.TrimSpace(f))
		}
	}
	out.Close = true
	var conn net.Conn
	var lastErr error
	for _, up := range ups {
		switch up.Kind {
		case ViaHTTP, ViaHTTPS:
			conn, lastErr = p.dialProxy(ctx, up)
			if lastErr == nil {
				if up.User != nil {
					out.Header.Set("Proxy-Authorization", basicAuth(up.User))
				}
				lastErr = out.WriteProxy(conn)
			}
		default:
			conn, lastErr = p.dialVia(ctx, up, host, uint16(port))
			if lastErr == nil {
				lastErr = out.Write(conn)
			}
		}
		if lastErr == nil {
			break
		}
		if conn != nil {
			conn.Close()
			conn = nil
		}
		if errors.Is(lastErr, errRefused) {
			break
		}
	}
	if conn == nil {
		code := http.StatusBadGateway
		if errors.Is(lastErr, errRefused) {
			code = http.StatusForbidden
		}
		p.fail(w, code, r.URL.Host, lastErr)
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(r.Context(), func() { conn.Close() })
	defer stop()
	resp, err := http.ReadResponse(bufio.NewReader(conn), out)
	if err != nil {
		p.fail(w, http.StatusBadGateway, r.URL.Host, err)
		return
	}
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func basicAuth(u *url.Userinfo) string {
	pw, _ := u.Password()
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+pw))
}

func (p *proxyEndpoint) dial(ctx context.Context, scheme, host string, port uint16) (net.Conn, error) {
	ups, err := p.decide(ctx, scheme, host, port)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, up := range ups {
		c, err := p.dialVia(ctx, up, host, port)
		if err == nil {
			return c, nil
		}
		if errors.Is(err, errRefused) {
			return nil, err
		}
		lastErr = fmt.Errorf("%v: %w", up, err)
	}
	return nil, lastErr
}

func (p *proxyEndpoint) dialVia(ctx context.Context, up Upstream, host string, port uint16) (net.Conn, error) {
	switch up.Kind {
	case Direct:
		return p.dialTarget(ctx, host, port)
	case ViaHTTP, ViaHTTPS:
		c, err := p.dialProxy(ctx, up)
		if err != nil {
			return nil, err
		}
		if deadline, ok := ctx.Deadline(); ok {
			c.SetDeadline(deadline)
		}
		target := hostPort(host, int(port))
		req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
		if up.User != nil {
			req += "Proxy-Authorization: " + basicAuth(up.User) + "\r\n"
		}
		if _, err := io.WriteString(c, req+"\r\n"); err != nil {
			c.Close()
			return nil, err
		}
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			c.Close()
			return nil, err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			c.Close()
			return nil, fmt.Errorf("%w: the proxy answered CONNECT with %s", errProxyRefused, resp.Status)
		}
		c.SetDeadline(time.Time{})
		return &bufferedConn{Conn: c, r: br}, nil
	case ViaSOCKS5:
		target := host
		if !up.RemoteNames {
			if _, err := netip.ParseAddr(host); err != nil {
				addrs, err := p.lookup(ctx, host)
				if err != nil {
					return nil, err
				}
				target = addrs[0].String()
			}
		}
		c, err := upstreamDialer{p}.DialContext(ctx, "tcp", up.Addr)
		if err != nil {
			return nil, err
		}
		if err := socks5Connect(ctx, c, up.User, target, port); err != nil {
			c.Close()
			if errors.Is(err, errSOCKSRefused) {
				err = fmt.Errorf("%w: %v", errProxyRefused, err)
			}
			return nil, err
		}
		return c, nil
	}
	return nil, errors.New("no way out")
}

func socks5Connect(ctx context.Context, c net.Conn, user *url.Userinfo, host string, port uint16) error {
	if deadline, ok := ctx.Deadline(); ok {
		c.SetDeadline(deadline)
		defer c.SetDeadline(time.Time{})
	}
	methods := []byte{5, 1, 0}
	if user != nil {
		methods = []byte{5, 2, 0, 2}
	}
	if _, err := c.Write(methods); err != nil {
		return err
	}
	var reply [2]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		return err
	}
	switch {
	case reply[0] != 5:
		return errors.New("the SOCKS proxy is not SOCKS5")
	case reply[1] == 2 && user != nil:
		pw, _ := user.Password()
		if len(user.Username()) > 255 || len(pw) > 255 {
			return errors.New("SOCKS5 credentials too long")
		}
		msg := append([]byte{1, byte(len(user.Username()))}, user.Username()...)
		msg = append(append(msg, byte(len(pw))), pw...)
		if _, err := c.Write(msg); err != nil {
			return err
		}
		if _, err := io.ReadFull(c, reply[:]); err != nil {
			return err
		}
		if reply[1] != 0 {
			return errors.New("the SOCKS proxy refused the credentials")
		}
	case reply[1] != 0:
		return errors.New("the SOCKS proxy accepts none of our ways to authenticate")
	}
	req := []byte{5, 1, 0}
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Unmap().Is4() {
			req = append(append(req, 1), a.Unmap().AsSlice()...)
		} else {
			req = append(append(req, 4), a.AsSlice()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("name too long for SOCKS5")
		}
		req = append(append(req, 3, byte(len(host))), host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return err
	}
	var head [4]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return err
	}
	if head[1] != 0 {
		return fmt.Errorf("%w (reply %d)", errSOCKSRefused, head[1])
	}
	var skip int
	switch head[3] {
	case 1:
		skip = 4 + 2
	case 4:
		skip = 16 + 2
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return err
		}
		skip = int(n[0]) + 2
	default:
		return errors.New("the SOCKS proxy sent an address of no known type")
	}
	_, err := io.ReadFull(c, make([]byte, skip))
	return err
}

var errSOCKSRefused = errors.New("the SOCKS proxy refused the connection")

func (p *proxyEndpoint) dialProxy(ctx context.Context, up Upstream) (net.Conn, error) {
	c, err := upstreamDialer{p}.DialContext(ctx, "tcp", up.Addr)
	if err != nil {
		return nil, err
	}
	if up.Kind != ViaHTTPS {
		return c, nil
	}
	host, _, _ := net.SplitHostPort(up.Addr)
	tc := tls.Client(c, &tls.Config{ServerName: host})
	if err := tc.HandshakeContext(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return tc, nil
}

type upstreamDialer struct{ p *proxyEndpoint }

func (d upstreamDialer) DialContext(ctx context.Context, _, addr string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, err
	}
	addrs, err := d.p.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, a := range addrs {
		c, err := d.p.s.host.DialTCP(ctx, netip.AddrPortFrom(a, uint16(port)))
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (p *proxyEndpoint) dialTarget(ctx context.Context, host string, port uint16) (net.Conn, error) {
	if isHostName(dns.Fqdn(host)) || (p.s.cfg.isHost(parseAddr(host))) {
		if !p.s.cfg.Host.IsValid() || (p.s.noHostLoopback != nil && p.s.noHostLoopback.Load()) {
			return nil, errRefused
		}
		return p.s.host.DialTCP(ctx, netip.AddrPortFrom(macLoopback, port))
	}
	addrs, err := p.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error = errRefused
	for _, a := range addrs {
		if p.refused(a) {
			continue
		}
		c, err := p.s.host.DialTCP(ctx, netip.AddrPortFrom(a, port))
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func parseAddr(s string) netip.Addr {
	a, _ := netip.ParseAddr(strings.Trim(s, "[]"))
	return a
}

func (p *proxyEndpoint) refused(a netip.Addr) bool {
	a = a.Unmap()
	c := p.s.cfg
	switch {
	case a.IsLoopback(), a.IsUnspecified(), a.IsMulticast(), a.IsLinkLocalUnicast():
		return true
	case c.Subnet.Contains(a), c.Own.IsValid() && c.Own.Contains(a), c.Own6.IsValid() && c.Own6.Contains(a):
		return true
	}
	if t := p.s.tunnelRef.Load(); t != nil && t.routed(a) {
		return true
	}
	return false
}

func (p *proxyEndpoint) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if a := parseAddr(host); a.IsValid() {
		return []netip.Addr{a}, nil
	}
	var out []netip.Addr
	for _, qt := range []uint16{dns.TypeA, dns.TypeAAAA} {
		rrs, err := p.s.host.Resolve(ctx, dns.Fqdn(host), qt)
		if err != nil {
			continue
		}
		for _, rr := range rrs {
			switch r := rr.(type) {
			case *dns.A:
				if a, ok := netip.AddrFromSlice(r.A.To4()); ok {
					out = append(out, a)
				}
			case *dns.AAAA:
				if a, ok := netip.AddrFromSlice(r.AAAA); ok {
					out = append(out, a)
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the Mac cannot resolve %s", host)
	}
	return out, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func (c *bufferedConn) CloseWrite() error {
	if hc, ok := c.Conn.(halfCloser); ok {
		return hc.CloseWrite()
	}
	return nil
}
