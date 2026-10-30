package macnet

import (
	"fmt"
	"net/netip"
)

type Kind string

const (
	KindRoute   Kind = "route"
	KindScoped  Kind = "scoped route"
	KindNetwork Kind = "network"
	KindAddress Kind = "address"
)

type Conflict struct {
	Prefix    netip.Prefix
	With      netip.Prefix
	Kind      Kind
	Interface string
	Withdrawn bool
}

func (c Conflict) Warning() string {
	on := ""
	if c.Interface != "" {
		on = " on " + c.Interface
	}
	if c.Withdrawn {
		return fmt.Sprintf("the Mac's %s %s%s overlaps fev's %s: direct access to %s is off until it goes", c.Kind, c.With, on, c.Prefix, c.Prefix)
	}
	if c.Kind == KindScoped {
		return fmt.Sprintf("the Mac's %s %s%s overlaps fev's %s: programs bound to %s reach it there, every other program reaches fev", c.Kind, c.With, on, c.Prefix, c.Interface)
	}
	return fmt.Sprintf("the Mac's %s %s%s covers fev's %s: its addresses inside %s reach fev, not %s", c.Kind, c.With, on, c.Prefix, c.Prefix, ifName(c.Interface))
}

func ifName(s string) string {
	if s == "" {
		return "that route"
	}
	return s
}

type Result struct {
	Keep      []netip.Prefix
	Conflicts []Conflict
}

func (r Result) Withdrawn() []netip.Prefix {
	var out []netip.Prefix
	for _, c := range r.Conflicts {
		if c.Withdrawn && (len(out) == 0 || out[len(out)-1] != c.Prefix) {
			out = append(out, c.Prefix)
		}
	}
	return out
}

func Check(ours []netip.Prefix, tunnel string, s Snapshot) Result {
	var r Result
	for _, p := range ours {
		p = p.Masked()
		withdrawn := false
		seen := map[macPrefix]bool{}
		for _, m := range mine(s, tunnel) {
			if m.with.Addr().Is4() != p.Addr().Is4() || !m.with.Overlaps(p) {
				continue
			}
			key := macPrefix{m.with, "", m.iface}
			if seen[key] {
				continue
			}
			seen[key] = true
			c := Conflict{Prefix: p, With: m.with, Kind: m.kind, Interface: m.iface}
			inside := m.with.Bits() >= p.Bits()
			c.Withdrawn = inside && m.kind != KindScoped
			withdrawn = withdrawn || c.Withdrawn
			r.Conflicts = append(r.Conflicts, c)
		}
		if !withdrawn {
			r.Keep = append(r.Keep, p)
		}
	}
	return r
}

func Taken(s Snapshot, tunnel string) []netip.Prefix {
	var out []netip.Prefix
	for _, m := range mine(s, tunnel) {
		out = append(out, m.with)
	}
	return out
}

type macPrefix struct {
	with  netip.Prefix
	kind  Kind
	iface string
}

func mine(s Snapshot, tunnel string) []macPrefix {
	var out []macPrefix
	for _, rt := range s.Routes {
		if rt.Prefix.Bits() == 0 || rt.Prefix.Addr().IsLoopback() || (tunnel != "" && rt.Interface == tunnel) {
			continue
		}
		k := KindRoute
		if rt.Scoped {
			k = KindScoped
		}
		out = append(out, macPrefix{rt.Prefix.Masked(), k, rt.Interface})
	}
	for _, n := range s.Networks {
		if n.Prefix.Bits() == 0 || n.Prefix.Addr().IsLoopback() || (tunnel != "" && n.Interface == tunnel) {
			continue
		}
		out = append(out, macPrefix{n.Prefix.Masked(), KindNetwork, n.Interface})
	}
	for _, a := range s.Addresses {
		if a.Addr.IsLoopback() || a.Addr.IsLinkLocalUnicast() || (tunnel != "" && a.Interface == tunnel) {
			continue
		}
		out = append(out, macPrefix{netip.PrefixFrom(a.Addr, a.Addr.BitLen()), KindAddress, a.Interface})
	}
	return out
}
