package docker

import (
	"net/netip"
	"slices"
	"sort"

	"fev/host/docker/engine"
	"fev/host/docker/forward"
	"fev/proto/agent"
)

var (
	loopback4 = netip.AddrFrom4([4]byte{127, 0, 0, 1})
	loopback6 = netip.IPv6Loopback()
)

func Wants(bindings []engine.Binding, uplink netip.Addr) []forward.Want {
	type port struct {
		proto string
		port  uint16
	}
	addrs := map[port][]netip.Addr{}
	for _, b := range bindings {
		if (b.Proto != forward.TCP && b.Proto != forward.UDP) || b.HostPort == 0 {
			continue
		}
		p := port{b.Proto, b.HostPort}
		ip := b.HostIP
		if !ip.IsValid() {
			ip = netip.IPv4Unspecified()
		}
		addrs[p] = append(addrs[p], ip.Unmap())
	}
	wants := make([]forward.Want, 0, len(addrs))
	for p, ips := range addrs {
		wants = append(wants, forward.Want{Proto: p.proto, Port: p.port, Guest: netip.AddrPortFrom(guestAddr(ips, uplink), p.port), LAN: slices.ContainsFunc(ips, netip.Addr.IsUnspecified)})
	}
	sort.Slice(wants, func(i, j int) bool {
		return wants[i].Port < wants[j].Port || (wants[i].Port == wants[j].Port && wants[i].Proto < wants[j].Proto)
	})
	return wants
}

func guestAddr(ips []netip.Addr, uplink netip.Addr) netip.Addr {
	var four, six bool
	var specific []netip.Addr
	for _, ip := range ips {
		switch {
		case ip == netip.IPv4Unspecified():
			if uplink.IsValid() {
				return uplink
			}
			four = true
		case ip == loopback4:
			four = true
		case ip == netip.IPv6Unspecified() || ip == loopback6:
			six = true
		default:
			specific = append(specific, ip)
		}
	}
	if four {
		return loopback4
	}
	if six {
		return loopback6
	}
	sort.Slice(specific, func(i, j int) bool {
		if specific[i].Is4() != specific[j].Is4() {
			return specific[i].Is4()
		}
		return specific[i].Less(specific[j])
	})
	return specific[0]
}

func uplinkOf(interfaces []agent.Interface) netip.Addr {
	for _, i := range interfaces {
		if !i.Up {
			continue
		}
		for _, a := range i.Addresses {
			prefix, err := netip.ParsePrefix(a)
			if err != nil {
				continue
			}
			ip := prefix.Addr().Unmap()
			if ip.IsLoopback() {
				break
			}
			if ip.Is4() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() && !ip.IsMulticast() && ip != netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
				return ip
			}
		}
	}
	return netip.Addr{}
}
