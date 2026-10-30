package netstack

import (
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const (
	answeredKeep    = 5 * time.Minute
	answeredKeepMax = 24 * time.Hour
	answeredMax     = 4096
)

type answered struct {
	name   string
	until  time.Time
	shared bool
}

type answeredNames struct {
	mu  sync.Mutex
	m   map[netip.Addr]answered
	now func() time.Time
}

func (a *answeredNames) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *answeredNames) record(name string, rrs []dns.RR) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if name == "" || underLocalZone(name) || isHostName(dns.Fqdn(name)) {
		return
	}
	now := a.clock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m == nil {
		a.m = map[netip.Addr]answered{}
	}
	for _, rr := range rrs {
		var addr netip.Addr
		switch r := rr.(type) {
		case *dns.A:
			addr, _ = netip.AddrFromSlice(r.A.To4())
		case *dns.AAAA:
			addr, _ = netip.AddrFromSlice(r.AAAA)
		default:
			continue
		}
		if !addr.IsValid() {
			continue
		}
		keep := time.Duration(rr.Header().Ttl) * time.Second
		keep = max(keep, answeredKeep)
		keep = min(keep, answeredKeepMax)
		e := answered{name: name, until: now.Add(keep)}
		if old, ok := a.m[addr]; ok && now.Before(old.until) && (old.shared || old.name != name) {
			e.shared = true
		}
		if _, ok := a.m[addr]; !ok && len(a.m) >= answeredMax {
			a.sweep(now)
		}
		a.m[addr] = e
	}
}

func (a *answeredNames) sweep(now time.Time) {
	for k, e := range a.m {
		if !now.Before(e.until) {
			delete(a.m, k)
		}
	}
	for k := range a.m {
		if len(a.m) < answeredMax-answeredMax/8 {
			break
		}
		delete(a.m, k)
	}
}

func (a *answeredNames) lookup(addr netip.Addr) (string, bool) {
	addr = addr.Unmap()
	now := a.clock()
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.m[addr]
	if !ok {
		return "", false
	}
	if !now.Before(e.until) {
		delete(a.m, addr)
		return "", false
	}
	return e.name, !e.shared
}

func answeredName(q dns.Question, resp *dns.Msg) string {
	if singleLabel(q.Name) && len(resp.Answer) > 0 {
		if c, ok := resp.Answer[0].(*dns.CNAME); ok && strings.EqualFold(c.Hdr.Name, q.Name) {
			return c.Target
		}
	}
	return q.Name
}
