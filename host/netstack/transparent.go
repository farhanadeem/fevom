package netstack

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const sniffWait = 250 * time.Millisecond

func (s *Stack) transparentProxy(asked, dst netip.AddrPort) *proxyEndpoint {
	s.mu.Lock()
	p := s.proxy
	s.mu.Unlock()
	if p == nil || asked != dst || p.opts.Transparent == nil || !p.opts.Transparent() {
		return nil
	}
	if p.alwaysDirect(dst.Addr()) || !p.anyProxy() {
		return nil
	}
	return p
}

func (p *proxyEndpoint) alwaysDirect(a netip.Addr) bool {
	a = a.Unmap()
	if p.refused(a) || p.s.cfg.isHost(a) {
		return true
	}
	if p.opts.LANs != nil {
		for _, pf := range p.opts.LANs() {
			if pf.Contains(a) {
				return true
			}
		}
	}
	return false
}

func (p *proxyEndpoint) anyProxy() bool {
	setting := ""
	if p.opts.Setting != nil {
		setting = p.opts.Setting()
	}
	up, fixed, err := ParseProxySetting(setting)
	switch {
	case err != nil:
		return false
	case fixed:
		return up.Kind != Direct
	}
	pp, ok := p.opts.System.(ProxyPresence)
	if !ok {
		return true
	}
	p.mu.Lock()
	known := p.presence
	p.mu.Unlock()
	if known != 0 {
		return known == 2
	}
	some := pp.Configured()
	p.mu.Lock()
	p.presence = 1
	if some {
		p.presence = 2
	}
	p.mu.Unlock()
	return some
}

func (p *proxyEndpoint) transparent(r *tcp.ForwarderRequest, dst netip.AddrPort) {
	s := p.s
	ctx, cancel := context.WithTimeout(s.ctx, proxyDialTimeout)
	defer cancel()
	port := dst.Port()
	name, sure := s.names.lookup(dst.Addr())
	if name != "" && underLocalZone(name) || excluded(dst.Addr().String(), p.exclude()) {
		s.forwardDirect(r, dst)
		return
	}

	if name != "" && sure && port != 80 {
		ups := p.decideTransparent(ctx, "https", name, port)
		if onlyDirect(ups) {
			s.forwardDirect(r, dst)
			return
		}
		s.syns.take(r.ID())
		up, err := p.openStream(ctx, ups, name, dst)
		if err != nil {
			s.log.Debug("transparent proxy: refused", "dst", dst, "name", name, "err", err)
			r.Complete(true)
			return
		}
		guest, _, ok := accept(r)
		if !ok {
			up.Close()
			return
		}
		p.carry(guest, up, nil)
		return
	}

	s.syns.take(r.ID())
	guest, ep, ok := accept(r)
	if !ok {
		return
	}
	head, sniffed, kind := sniffFlow(guest)
	fromBytes := false
	if sniffed != "" && !sure {
		name, fromBytes = sniffed, true
	}
	if name != "" && underLocalZone(name) {
		name = ""
	}
	target := name
	if target == "" {
		target = dst.Addr().String()
	}
	scheme := "https"
	if kind == "http" || (kind == "" && port == 80) {
		scheme = "http"
	}
	ups := p.decideTransparent(ctx, scheme, target, port)
	if fromBytes && name != "" && anyDirect(ups) && !p.namesAddress(ctx, name, dst.Addr()) {
		ups = p.decideTransparent(ctx, scheme, dst.Addr().String(), port)
	}
	if kind == "http" {
		if up, conn, err := p.openHTTPProxy(ctx, ups); err == nil && conn != nil {
			br := bufio.NewReader(io.MultiReader(bytes.NewReader(head), guest))
			p.relayHTTP(guest, br, up, conn, name, dst)
			return
		} else if err != nil {
			s.log.Debug("transparent proxy: the proxy cannot be reached", "dst", dst, "name", name, "err", err)
			ep.Abort()
			guest.Close()
			return
		}
	}
	var up net.Conn
	var err error
	if onlyDirect(ups) {
		up, err = s.host.DialTCP(ctx, dst)
	} else {
		up, err = p.openStream(ctx, ups, name, dst)
	}
	if err != nil {
		s.log.Debug("transparent proxy: refused", "dst", dst, "name", name, "err", err)
		ep.Abort()
		guest.Close()
		return
	}
	p.carry(guest, up, head)
}

func (p *proxyEndpoint) decideTransparent(ctx context.Context, scheme, host string, port uint16) []Upstream {
	ups, err := p.decide(ctx, scheme, host, port)
	if err != nil {
		p.s.log.Debug("transparent proxy: no decision, direct", "target", host, "err", err)
		ups = []Upstream{{Kind: Direct}}
		p.remember(decisionKey(scheme, host, port), ups)
	}
	return ups
}

func anyDirect(ups []Upstream) bool {
	if len(ups) == 0 {
		return true
	}
	for _, up := range ups {
		if up.Kind == Direct {
			return true
		}
	}
	return false
}

func (p *proxyEndpoint) namesAddress(ctx context.Context, name string, a netip.Addr) bool {
	addrs, err := p.lookup(ctx, name)
	if err != nil {
		return false
	}
	for _, b := range addrs {
		if b.Unmap() == a.Unmap() {
			return true
		}
	}
	return false
}

func onlyDirect(ups []Upstream) bool {
	return len(ups) == 0 || (len(ups) == 1 && ups[0].Kind == Direct)
}

func accept(r *tcp.ForwarderRequest) (*gonet.TCPConn, tcpip.Endpoint, bool) {
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		r.Complete(true)
		return nil, nil, false
	}
	r.Complete(false)
	return gonet.NewTCPConn(&wq, ep), ep, true
}

func sniffFlow(c *gonet.TCPConn) (head []byte, name, kind string) {
	c.SetReadDeadline(time.Now().Add(sniffWait))
	defer c.SetReadDeadline(time.Time{})
	buf := make([]byte, 0, 2048)
	chunk := make([]byte, 4096)
	for len(buf) < sniffMax+5 {
		n, err := c.Read(chunk)
		buf = append(buf, chunk[:n]...)
		nm, k, res := sniffName(buf)
		if res == sniffDone || err != nil {
			return buf, nm, k
		}
	}
	nm, k, _ := sniffName(buf)
	return buf, nm, k
}

func (p *proxyEndpoint) openStream(ctx context.Context, ups []Upstream, name string, dst netip.AddrPort) (net.Conn, error) {
	target := name
	if target == "" {
		target = dst.Addr().String()
	}
	var lastErr error = errors.New("no way out")
	for _, up := range ups {
		var c net.Conn
		var err error
		switch up.Kind {
		case Direct:
			c, err = p.s.host.DialTCP(ctx, dst)
		case ViaSOCKS5:
			t := target
			if !up.RemoteNames {
				t = dst.Addr().String()
			}
			c, err = p.dialVia(ctx, up, t, dst.Port())
		default:
			c, err = p.dialVia(ctx, up, target, dst.Port())
		}
		if err == nil {
			return c, nil
		}
		lastErr = fmt.Errorf("%v: %w", up, err)
		if errors.Is(err, errProxyRefused) {
			break
		}
	}
	return nil, lastErr
}

func (p *proxyEndpoint) openHTTPProxy(ctx context.Context, ups []Upstream) (Upstream, net.Conn, error) {
	var lastErr error
	for _, up := range ups {
		if up.Kind != ViaHTTP && up.Kind != ViaHTTPS {
			return up, nil, nil
		}
		c, err := p.dialProxy(ctx, up)
		if err == nil {
			return up, c, nil
		}
		lastErr = fmt.Errorf("%v: %w", up, err)
	}
	return Upstream{}, nil, lastErr
}

func (p *proxyEndpoint) carry(guest *gonet.TCPConn, up net.Conn, head []byte) {
	defer guest.Close()
	defer up.Close()
	if len(head) > 0 {
		if _, err := up.Write(head); err != nil {
			return
		}
	}
	stop := context.AfterFunc(p.s.ctx, func() {
		guest.Close()
		up.Close()
	})
	defer stop()
	splice(guest, up)
}

func (p *proxyEndpoint) relayHTTP(guest *gonet.TCPConn, br *bufio.Reader, up Upstream, conn net.Conn, name string, dst netip.AddrPort) {
	defer guest.Close()
	stop := context.AfterFunc(p.s.ctx, func() { guest.Close() })
	defer stop()
	closeUp := func() {
		if conn != nil {
			conn.Close()
			conn = nil
		}
	}
	defer closeUp()
	var upr *bufio.Reader
	if conn != nil {
		upr = bufio.NewReader(conn)
	}
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		host := name
		if h := hostOf(req.Host); h != "" {
			host = h
		}
		if host == "" {
			host = dst.Addr().String()
		}
		req.URL.Scheme = "http"
		req.URL.Host = host
		if dst.Port() != 80 {
			req.URL.Host = hostPort(host, int(dst.Port()))
		}
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("Proxy-Connection")
		if up.User != nil {
			req.Header.Set("Proxy-Authorization", basicAuth(up.User))
		}
		if conn == nil {
			dctx, cancel := context.WithTimeout(p.s.ctx, proxyDialTimeout)
			conn, err = p.dialProxy(dctx, up)
			cancel()
			if err != nil {
				p.s.log.Debug("transparent proxy: the proxy cannot be reached", "url", req.URL.String(), "err", err)
				return
			}
			upr = bufio.NewReader(conn)
		}
		c := conn
		upStop := context.AfterFunc(p.s.ctx, func() { c.Close() })
		if err := req.WriteProxy(conn); err != nil {
			upStop()
			return
		}
		var resp *http.Response
		for {
			resp, err = http.ReadResponse(upr, req)
			if err != nil {
				upStop()
				return
			}
			if resp.StatusCode >= 200 || resp.StatusCode == http.StatusSwitchingProtocols {
				break
			}
			if err := resp.Write(guest); err != nil {
				upStop()
				return
			}
		}
		if err := resp.Write(guest); err != nil {
			resp.Body.Close()
			upStop()
			return
		}
		resp.Body.Close()
		upStop()
		if resp.StatusCode == http.StatusSwitchingProtocols {
			splice(&bufferedConn{Conn: guest, r: br}, &bufferedConn{Conn: conn, r: upr})
			return
		}
		if resp.Close {
			closeUp()
		}
		if req.Close {
			return
		}
	}
}

func hostOf(h string) string {
	h = strings.TrimSpace(h)
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return validName(h)
}
