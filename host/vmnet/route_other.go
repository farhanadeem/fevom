//go:build !darwin

package vmnet

import "net/netip"

type Route struct {
	Interface string
	Reject    bool
	Prefix    netip.Prefix
}

func RouteOf(netip.Addr) (Route, error) { return Route{}, ErrUnsupported }
