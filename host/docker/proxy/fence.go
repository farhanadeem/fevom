package proxy

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

type Fenced struct {
	Waited time.Duration
	Left   []string
}

const (
	fenceOff = iota
	fenceWork
	fenceAll
)

var removalBound = 5 * time.Second

var headerEnded func()

func (p *Proxy) Fence(ctx context.Context, bound time.Duration, waiting func(left []string)) (Fenced, func(), error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return Fenced{}, func() {}, fmt.Errorf("the Docker socket is closed")
	}
	if p.fence != fenceOff {
		p.mu.Unlock()
		return Fenced{}, func() {}, fmt.Errorf("a fence is already up")
	}
	p.fence = fenceWork
	p.fenceGate = make(chan struct{})
	p.mu.Unlock()
	release := func() {
		p.mu.Lock()
		if p.fence == fenceOff {
			p.mu.Unlock()
			return
		}
		p.fence = fenceOff
		close(p.fenceGate)
		held := p.fenceHeld
		p.fenceHeld = nil
		dial := p.dial
		passing := p.mode == passing
		for i, isGone := range gone(held) {
			switch {
			case isGone:
				held[i].Close()
			case passing:
				p.wg.Add(1)
				go p.join(held[i], dial)
			default:
				p.held = append(p.held, held[i])
			}
		}
		p.mu.Unlock()
	}
	began := time.Now()
	var timeout <-chan time.Time
	if bound > 0 {
		t := time.NewTimer(bound)
		defer t.Stop()
		timeout = t.C
	}
	removals := time.NewTimer(removalBound)
	defer removals.Stop()
	waitRemovals := true
	var told string
	var left []string
	for {
		p.mu.Lock()
		left = p.inFlightLocked(waitRemovals)
		if len(left) == 0 {
			p.fence = fenceAll
			p.mu.Unlock()
			break
		}
		changed := p.flightChanged
		p.mu.Unlock()
		if s := strings.Join(left, ", "); s != told && waiting != nil {
			told = s
			waiting(left)
		}
		select {
		case <-changed:
			continue
		case <-removals.C:
			waitRemovals = false
			continue
		case <-timeout:
		case <-ctx.Done():
			release()
			return Fenced{}, func() {}, ctx.Err()
		}
		p.mu.Lock()
		p.fence = fenceAll
		left = p.inFlightLocked(false)
		p.mu.Unlock()
		break
	}
	return Fenced{Waited: time.Since(began), Left: left}, release, nil
}

func (p *Proxy) inFlightLocked(removals bool) []string {
	var out []string
	for t := range p.tracked {
		for _, f := range t.flight {
			if f.cls != removal || removals {
				out = append(out, f.kind)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (p *Proxy) flightLocked() {
	close(p.flightChanged)
	p.flightChanged = make(chan struct{})
}

func (p *Proxy) heldLocked(c class) bool {
	return p.fence == fenceAll || (p.fence == fenceWork && (c == work || c == upgraded))
}

type tracked struct {
	p         *Proxy
	req, resp framer
	pending   []pendingReq
	flight    []flying
	final     bool
}

type pendingReq struct {
	method  string
	kind    string
	cls     class
	raw     bool
	counted bool
}

type flying struct {
	kind string
	cls  class
}

func (p *Proxy) track() *tracked {
	t := &tracked{p: p}
	t.resp.response = true
	p.mu.Lock()
	p.tracked[t] = struct{}{}
	p.mu.Unlock()
	return t
}

func (t *tracked) untrack() {
	p := t.p
	p.mu.Lock()
	delete(p.tracked, t)
	if len(t.flight) > 0 {
		t.flight = nil
		p.flightLocked()
	}
	p.mu.Unlock()
}

func (t *tracked) fromClient(b []byte) {
	var reqs []pendingReq
	t.req.feed(b, func(m message, h []byte) (bool, bool) {
		kind, cls := classify(m.method, m.target)
		raw := m.upgrade || hijacks(m.method, m.target)
		reqs = append(reqs, pendingReq{method: m.method, kind: kind, cls: cls, raw: raw})
		return !raw, raw
	}, func() {})
	if len(reqs) == 0 {
		return
	}
	if headerEnded != nil {
		headerEnded()
	}
	p := t.p
	for _, r := range reqs {
		for {
			p.mu.Lock()
			if p.heldLocked(r.cls) {
				gate := p.fenceGate
				p.mu.Unlock()
				<-gate
				continue
			}
			if r.cls == removal && p.fence != fenceOff {
				r.cls = stream
			}
			r.counted = r.cls != stream
			t.pending = append(t.pending, r)
			if r.counted {
				t.flight = append(t.flight, flying{r.kind, r.cls})
				p.flightLocked()
			}
			p.mu.Unlock()
			break
		}
	}
}

func (t *tracked) fromEngine(b []byte) {
	p := t.p
	t.resp.feed(b, func(m message, h []byte) (bool, bool) {
		p.mu.Lock()
		var first pendingReq
		if len(t.pending) > 0 {
			first = t.pending[0]
		}
		p.mu.Unlock()
		switch {
		case m.status == 101:
			t.end(true)
			return false, true
		case m.status >= 100 && m.status < 200:
			t.final = false
			return false, false
		case first.raw:
			t.end(true)
			return false, true
		case first.method == "HEAD" || m.status == 204 || m.status == 304:
			t.final = true
			return false, false
		}
		t.final = true
		return true, false
	}, func() {
		if t.final {
			t.final = false
			t.end(false)
		}
	})
}

func (t *tracked) end(upgradedNow bool) {
	p := t.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(t.pending) == 0 {
		return
	}
	first := t.pending[0]
	t.pending = t.pending[1:]
	if !first.counted || (upgradedNow && first.cls == upgraded) {
		return
	}
	for i, f := range t.flight {
		if f.kind == first.kind && f.cls == first.cls {
			t.flight = append(t.flight[:i], t.flight[i+1:]...)
			break
		}
	}
	p.flightLocked()
}

type trackedConn struct {
	net.Conn
	read func([]byte)
}

func (c *trackedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.read(b[:n])
	}
	return n, err
}

func (c *trackedConn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return c.Conn.Close()
}
