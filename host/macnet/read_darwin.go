//go:build darwin

package macnet

import (
	"fmt"
	"math/bits"
	"net"
	"net/netip"
	"slices"
	"syscall"

	"golang.org/x/net/route"

	"fev/host/nat64"
	"fev/proto/netplan"
)

const (
	rtfHost      = 0x4
	rtfReject    = 0x8
	rtfBlackhole = 0x1000
	rtfLLInfo    = 0x400
	rtfWasCloned = 0x20000
	rtfLocal     = 0x200000
	rtfBroadcast = 0x400000
	rtfMulticast = 0x800000
	rtfIfScope   = 0x1000000
)

func readMac() (Snapshot, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return Snapshot{}, fmt.Errorf("interfaces: %w", err)
	}
	names := map[int]string{}
	var s Snapshot
	globalOn := map[string]bool{}
	for _, ifc := range ifs {
		names[ifc.Index] = ifc.Name
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			ones, _ := ipn.Mask.Size()
			s.Addresses = append(s.Addresses, Address{Addr: ip, Interface: ifc.Name})
			if ip.Is6() && globalUnicast6.Contains(ip) {
				globalOn[ifc.Name] = true
			}
			if ifc.Flags&net.FlagLoopback != 0 || ip.IsLinkLocalUnicast() {
				continue
			}
			s.Networks = append(s.Networks, Network{Prefix: netip.PrefixFrom(ip, ones).Masked(), Interface: ifc.Name})
		}
		if ifc.Flags&net.FlagLoopback == 0 {
			if ps, err := nat64.Of(ifc.Name); err == nil {
				for _, p := range ps {
					if netplan.CheckNAT64(p) == nil {
						s.NAT64 = append(s.NAT64, p)
					}
				}
			}
		}
	}

	rib, err := route.FetchRIB(syscall.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return Snapshot{}, fmt.Errorf("routing table: %w", err)
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return Snapshot{}, fmt.Errorf("routing table: %w", err)
	}
	for _, m := range msgs {
		rm, ok := m.(*route.RouteMessage)
		if !ok {
			continue
		}
		rt, ok := parseRoute(rm.Flags, rm.Addrs, names[rm.Index])
		if !ok {
			continue
		}
		s.Routes = append(s.Routes, rt)
		if rt.Prefix.Bits() == 0 && rt.Prefix.Addr().Is6() && !rt.Scoped && !rt.Reject && !rt.Blackhole && globalOn[rt.Interface] {
			s.GlobalIPv6 = true
		}
	}
	sortSnapshot(&s)
	return s, nil
}

var globalUnicast6 = netip.MustParsePrefix("2000::/3")

func parseRoute(flags int, addrs []route.Addr, iface string) (Route, bool) {
	if flags&(rtfWasCloned|rtfLLInfo|rtfLocal|rtfBroadcast|rtfMulticast) != 0 {
		return Route{}, false
	}
	if len(addrs) <= syscall.RTAX_DST {
		return Route{}, false
	}
	dst, ok := inetAddr(addrs[syscall.RTAX_DST])
	if !ok {
		return Route{}, false
	}
	bitsLen := dst.BitLen()
	if flags&rtfHost == 0 {
		if len(addrs) > syscall.RTAX_NETMASK && addrs[syscall.RTAX_NETMASK] != nil {
			bitsLen = maskBits(addrs[syscall.RTAX_NETMASK], dst.BitLen())
		} else if dst.IsUnspecified() {
			bitsLen = 0
		}
	}
	rt := Route{Prefix: netip.PrefixFrom(dst, bitsLen).Masked(), Interface: iface, Scoped: flags&rtfIfScope != 0,
		Reject: flags&rtfReject != 0, Blackhole: flags&rtfBlackhole != 0}
	if len(addrs) > syscall.RTAX_GATEWAY {
		if gw, ok := inetAddr(addrs[syscall.RTAX_GATEWAY]); ok {
			rt.Gateway = gw
		}
	}
	return rt, true
}

func inetAddr(a route.Addr) (netip.Addr, bool) {
	switch a := a.(type) {
	case *route.Inet4Addr:
		return netip.AddrFrom4(a.IP), true
	case *route.Inet6Addr:
		ip := netip.AddrFrom16(a.IP)
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			b := ip.As16()
			b[2], b[3] = 0, 0
			ip = netip.AddrFrom16(b)
		}
		return ip, true
	}
	return netip.Addr{}, false
}

func maskBits(a route.Addr, full int) int {
	var b []byte
	switch a := a.(type) {
	case *route.Inet4Addr:
		b = a.IP[:]
	case *route.Inet6Addr:
		b = a.IP[:]
	default:
		return full
	}
	n := 0
	for _, x := range b {
		n += bits.LeadingZeros8(^x)
		if x != 0xff {
			break
		}
	}
	for i, x := range b {
		want := byte(0)
		switch {
		case n >= (i+1)*8:
			want = 0xff
		case n > i*8:
			want = 0xff << (8 - (n - i*8))
		}
		if x != want {
			return full
		}
	}
	return min(n, full)
}

func sortSnapshot(s *Snapshot) {
	slices.SortFunc(s.Routes, func(a, b Route) int {
		if c := comparePrefix(a.Prefix, b.Prefix); c != 0 {
			return c
		}
		if a.Interface != b.Interface {
			if a.Interface < b.Interface {
				return -1
			}
			return 1
		}
		if a.Scoped != b.Scoped {
			if a.Scoped {
				return 1
			}
			return -1
		}
		if c := a.Gateway.Compare(b.Gateway); c != 0 {
			return c
		}
		return cmpBool(a.Reject, b.Reject)*2 + cmpBool(a.Blackhole, b.Blackhole)
	})
	s.Routes = slices.Compact(s.Routes)
	slices.SortFunc(s.NAT64, comparePrefix)
	s.NAT64 = slices.Compact(s.NAT64)
	slices.SortFunc(s.Networks, func(a, b Network) int {
		if c := comparePrefix(a.Prefix, b.Prefix); c != 0 {
			return c
		}
		return cmpString(a.Interface, b.Interface)
	})
	s.Networks = slices.Compact(s.Networks)
	slices.SortFunc(s.Addresses, func(a, b Address) int {
		if c := a.Addr.Compare(b.Addr); c != 0 {
			return c
		}
		return cmpString(a.Interface, b.Interface)
	})
	s.Addresses = slices.Compact(s.Addresses)
}

func comparePrefix(a, b netip.Prefix) int {
	if c := a.Addr().Compare(b.Addr()); c != 0 {
		return c
	}
	return a.Bits() - b.Bits()
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
