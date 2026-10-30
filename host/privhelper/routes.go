package privhelper

import (
	"fmt"
	"net/netip"
	"regexp"
	"slices"

	api "fev/proto/privhelper"
)

var utunName = regexp.MustCompile(`^utun[0-9]{1,5}$`)

func (s *Server) tunnelInterface(ifaces []Interface, name string) (Interface, error) {
	if !utunName.MatchString(name) {
		return Interface{}, api.Refusal{Arg: "interface", Value: name, Reason: "not a utun's name"}
	}
	for _, i := range ifaces {
		if i.Name == name {
			if !i.Has(s.plan.TunnelMac()) {
				return Interface{}, api.Refusal{Arg: "interface", Value: name, Reason: fmt.Sprintf("does not carry the tunnel address %s", s.plan.TunnelMac())}
			}
			return i, nil
		}
	}
	return Interface{}, api.Refusal{Arg: "interface", Value: name, Reason: "no such interface"}
}

func PlanRoutes(bases api.Bases, tun Interface, want []netip.Prefix, table []Route, ifaces []Interface) (add, del []netip.Prefix, err error) {
	for i, p := range want {
		arg := fmt.Sprintf("prefixes[%d]", i)
		for _, ifc := range ifaces {
			if ifc.Index == tun.Index {
				continue
			}
			for _, a := range ifc.Addrs {
				if a.Addr().Is4() != p.Addr().Is4() || a.Addr().IsLinkLocalUnicast() {
					continue
				}
				if a.Masked().Overlaps(p) {
					return nil, nil, api.Refusal{Arg: arg, Value: p.String(), Reason: fmt.Sprintf("meets %s's connected network %s", ifc.Name, a.Masked())}
				}
			}
		}
		for _, r := range table {
			if r.Index == tun.Index || r.Prefix.Addr().Is4() != p.Addr().Is4() || r.Flags&(RTFWasCloned|RTFDynamic) != 0 {
				continue
			}
			if p.Contains(r.Prefix.Addr()) && r.Prefix.Bits() >= p.Bits() {
				return nil, nil, api.Refusal{Arg: arg, Value: p.String(), Reason: fmt.Sprintf("a route to %s on %s is as specific or more", r.Prefix, nameOf(ifaces, r.Index))}
			}
		}
	}
	var have []netip.Prefix
	for _, r := range table {
		if r.Index != tun.Index || r.Flags&RTFStatic == 0 || (!inside(bases, r.Prefix) && !api.NAT64Twin(bases, r.Prefix)) || slices.Contains(have, r.Prefix) {
			continue
		}
		have = append(have, r.Prefix)
	}
	for _, p := range have {
		if !slices.Contains(want, p) {
			del = append(del, p)
		}
	}
	for _, p := range want {
		if !slices.Contains(have, p) {
			add = append(add, p)
		}
	}
	return add, del, nil
}

func inside(bases api.Bases, p netip.Prefix) bool {
	for _, b := range bases {
		if b.Addr().Is4() == p.Addr().Is4() && b.Contains(p.Addr()) && p.Bits() >= b.Bits() {
			return true
		}
	}
	return false
}

func nameOf(ifaces []Interface, index int) string {
	for _, i := range ifaces {
		if i.Index == index {
			return i.Name
		}
	}
	return fmt.Sprintf("interface %d", index)
}

func prefixStrings(ps []netip.Prefix) []string {
	if len(ps) == 0 {
		return nil
	}
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}

func (s *Server) routesSet(p api.RoutesParams) (api.Routes, error) {
	nat64, err := s.sys.NAT64Prefixes()
	if err != nil {
		s.logf("routes.set: the Mac's NAT64 prefixes: %v", err)
		nat64 = nil
	}
	want, err := api.CheckRoutesNAT64(s.bases, nat64, p.Prefixes)
	if err != nil {
		return api.Routes{}, err
	}
	ifaces, err := s.sys.Interfaces()
	if err != nil {
		return api.Routes{}, err
	}
	tun, err := s.tunnelInterface(ifaces, p.Interface)
	if err != nil {
		return api.Routes{}, err
	}
	if err := holdBases(s.sys, recordedHeld(s.rec)); err != nil {
		s.logf("routes.set: %v", err)
	}
	table, err := s.sys.Routes()
	if err != nil {
		return api.Routes{}, err
	}
	add, del, err := PlanRoutes(s.bases, tun, want, table, ifaces)
	if err != nil {
		return api.Routes{}, err
	}
	for _, d := range del {
		if err := s.sys.DeleteRoute(tun.Name, d); err != nil {
			return api.Routes{}, fmt.Errorf("deleting the route to %s on %s: %w", d, tun.Name, err)
		}
	}
	for _, a := range add {
		if err := s.sys.AddRoute(tun.Name, a); err != nil {
			return api.Routes{}, fmt.Errorf("adding the route to %s on %s: %w", a, tun.Name, err)
		}
	}
	prefixes := prefixStrings(want)
	if prefixes == nil {
		prefixes = []string{}
	}
	return api.Routes{Interface: tun.Name, Prefixes: prefixes, Added: prefixStrings(add), Removed: prefixStrings(del)}, nil
}
