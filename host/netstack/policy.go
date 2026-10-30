package netstack

import (
	"net/netip"

	"fev/proto/netplan"
)

type Config struct {
	Subnet  netip.Prefix
	Gateway netip.Addr
	Host    netip.Addr

	Subnet6  netip.Prefix
	Gateway6 netip.Addr
	Host6    netip.Addr
	Own6     netip.Prefix

	Own netip.Prefix
}

func ConfigOf(p netplan.Plan) Config {
	return Config{Subnet: p.Uplink(), Gateway: p.UplinkGateway(), Host: p.Host(), Own: p.Base()}
}

var DefaultConfig = ConfigOf(netplan.Default())

var macLoopback = netip.AddrFrom4([4]byte{127, 0, 0, 1})

func (c Config) route(dst netip.AddrPort) (netip.AddrPort, bool) {
	if c.Host.IsValid() && dst.Addr().Unmap() == c.Host {
		return netip.AddrPortFrom(macLoopback, dst.Port()), true
	}
	if c.Host6.IsValid() && dst.Addr() == c.Host6 {
		return netip.AddrPortFrom(macLoopback6, dst.Port()), true
	}
	return dst, c.forwardable(dst.Addr())
}

func (s *Stack) route(dst netip.AddrPort) (netip.AddrPort, bool) {
	if s.noHostLoopback != nil && s.noHostLoopback.Load() && s.cfg.isHost(dst.Addr()) {
		return dst, false
	}
	if t := s.tunnelRef.Load(); t != nil && t.routed(dst.Addr().Unmap()) {
		return dst, false
	}
	return s.cfg.route(dst)
}

func (c Config) forwardable(dst netip.Addr) bool {
	dst = dst.Unmap()
	if dst.Is6() {
		return c.forwardable6(dst)
	}
	switch {
	case !dst.IsValid(), !dst.Is4():
		return false
	case dst.IsLoopback(), dst.IsUnspecified(), dst.IsMulticast(), dst.IsLinkLocalUnicast():
		return false
	case dst == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return false
	case c.Subnet.Contains(dst):
		return false
	}
	return true
}
