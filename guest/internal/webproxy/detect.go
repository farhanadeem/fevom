package webproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	api "fev/proto/agent"
)

const UserAgent = "fev-web-detect/1 (+http://fev.local/)"

const probeWait = 1500 * time.Millisecond

type Listener struct {
	Addr netip.Addr
	Port int
}

type WebPort struct {
	Port int
	TLS  bool
	Why  string
}

type probed struct {
	port   int
	http   bool
	html   bool
	tls    bool
	expose bool
}

func decide(ps []probed) (WebPort, bool) {
	tiers := []struct {
		why string
		ok  func(p probed) bool
	}{
		{"answers GET / with HTML", func(p probed) bool { return p.html }},
		{"EXPOSEd and answers HTTP", func(p probed) bool { return p.http && p.expose }},
		{"answers HTTP", func(p probed) bool { return p.http }},
	}
	for _, tier := range tiers {
		var in []probed
		for _, p := range ps {
			if tier.ok(p) {
				in = append(in, p)
			}
		}
		if len(in) == 0 {
			continue
		}
		sort.Slice(in, func(i, j int) bool {
			if in[i].tls != in[j].tls {
				return !in[i].tls
			}
			return in[i].port < in[j].port
		})
		return WebPort{Port: in[0].port, TLS: in[0].tls, Why: tier.why}, true
	}
	return WebPort{}, false
}

func candidates(ls []Listener) []int {
	seen := map[int]bool{}
	var out []int
	for _, l := range ls {
		if l.Addr.IsLoopback() || l.Port <= 0 || l.Port > 65535 || seen[l.Port] {
			continue
		}
		seen[l.Port] = true
		out = append(out, l.Port)
	}
	sort.Ints(out)
	return out
}

func (p *Proxy) detect(ctx context.Context, t api.DomainTarget, host netip.Addr, sni string) (WebPort, error) {
	switch {
	case t.HTTPPort > 0:
		return WebPort{Port: t.HTTPPort, Why: "the label's or the setting's"}, nil
	case t.HTTPSPort > 0:
		return WebPort{Port: t.HTTPSPort, TLS: true, Why: "the label's or the setting's"}, nil
	}
	ls, err := p.cfg.Lister.Listening(t)
	if err != nil {
		return WebPort{}, fmt.Errorf("the listening ports of %s: %w", t.Name, err)
	}
	ports := candidates(ls)
	for _, port := range ports {
		if port == 80 {
			return WebPort{Port: 80, Why: "listens on 80"}, nil
		}
	}
	exposed := map[int]bool{}
	for _, e := range t.Exposed {
		if n, proto, _ := strings.Cut(e, "/"); proto == "tcp" || proto == "" {
			if v, err := strconv.Atoi(n); err == nil {
				exposed[v] = true
			}
		}
	}
	results := make([]probed, len(ports))
	var wg sync.WaitGroup
	for i, port := range ports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = p.probe(ctx, netip.AddrPortFrom(host, uint16(port)), sni)
			results[i].expose = exposed[port]
		}()
	}
	wg.Wait()
	if w, ok := decide(results); ok {
		return w, nil
	}
	return WebPort{}, fmt.Errorf("%s has no web port (%d listening ports probed)", t.Name, len(ports))
}

func (p *Proxy) probe(ctx context.Context, at netip.AddrPort, sni string) probed {
	r := probed{port: int(at.Port())}
	conf := &tls.Config{InsecureSkipVerify: true, ServerName: sni, NextProtos: []string{"http/1.1"}}
	if p.speaksTLS(ctx, at, conf) {
		r.tls = true
		r.http, r.html = p.ask(ctx, at, conf, sni)
		return r
	}
	r.http, r.html = p.ask(ctx, at, nil, sni)
	return r
}

func (p *Proxy) speaksTLS(ctx context.Context, at netip.AddrPort, conf *tls.Config) bool {
	ctx, cancel := context.WithTimeout(ctx, probeWait)
	defer cancel()
	c, err := p.cfg.Dial(ctx, "tcp", at.String())
	if err != nil {
		return false
	}
	defer c.Close()
	deadline, _ := ctx.Deadline()
	c.SetDeadline(deadline)
	return tls.Client(c, conf).HandshakeContext(ctx) == nil
}

func (p *Proxy) ask(ctx context.Context, at netip.AddrPort, conf *tls.Config, host string) (isHTTP, isHTML bool) {
	if host == "" {
		host = at.String()
	}
	for _, req := range []string{"HEAD /favicon.ico", "GET /"} {
		status, ctype, ok := p.request(ctx, at, conf, req, host)
		if !ok {
			if req == "GET /" {
				return isHTTP, false
			}
			continue
		}
		isHTTP = true
		if req == "GET /" {
			return true, status < 500 && strings.HasPrefix(strings.ToLower(ctype), "text/html")
		}
	}
	return isHTTP, false
}

func (p *Proxy) request(ctx context.Context, at netip.AddrPort, conf *tls.Config, req, host string) (status int, ctype string, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, probeWait)
	defer cancel()
	c, err := p.cfg.Dial(ctx, "tcp", at.String())
	if err != nil {
		return 0, "", false
	}
	defer c.Close()
	deadline, _ := ctx.Deadline()
	c.SetDeadline(deadline)
	if conf != nil {
		tc := tls.Client(c, conf)
		if err := tc.HandshakeContext(ctx); err != nil {
			return 0, "", false
		}
		c = tc
	}
	fmt.Fprintf(c, "%s HTTP/1.1\r\nHost: %s\r\nUser-Agent: %s\r\nAccept: text/html,*/*\r\nConnection: close\r\n\r\n", req, host, UserAgent)
	br := bufio.NewReaderSize(c, 4096)
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, "", false
	}
	f := strings.Fields(line)
	if len(f) < 2 || !strings.HasPrefix(f[0], "HTTP/1.") {
		return 0, "", false
	}
	status, err = strconv.Atoi(f[1])
	if err != nil || status < 100 || status > 599 {
		return 0, "", false
	}
	for i := 0; i < 100; i++ {
		h, err := br.ReadString('\n')
		h = strings.TrimRight(h, "\r\n")
		if err != nil || h == "" {
			break
		}
		if k, v, ok := strings.Cut(h, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Type") {
			ctype = strings.TrimSpace(v)
		}
	}
	return status, ctype, true
}
