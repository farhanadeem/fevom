package privhelper

import (
	"fmt"
	"net/netip"

	api "fev/proto/privhelper"
)

func (s *Server) tunnelOpen(p api.TunnelParams) (api.Tunnel, int, error) {
	if err := api.CheckMTU(p.MTU); err != nil {
		return api.Tunnel{}, -1, err
	}
	var mac6 netip.Addr
	if p.IPv6 {
		if s.plan6 == nil {
			return api.Tunnel{}, -1, api.Refusal{Arg: "ipv6", Value: "true", Reason: "the install recorded no IPv6 base; run `sudo fev setup --admin` again"}
		}
		mac6 = s.plan6.TunnelMac6()
	}
	mac, peer := s.plan.TunnelMac(), s.plan.TunnelPeer()
	ifaces, err := s.sys.Interfaces()
	if err != nil {
		return api.Tunnel{}, -1, err
	}
	if other := carrying(ifaces, mac, ""); other != "" {
		return api.Tunnel{}, -1, api.Refusal{Arg: "tunnel", Value: other, Reason: fmt.Sprintf("%s already carries the tunnel address %s: a daemon holds the tunnel", other, mac)}
	}
	fd, name, err := s.sys.OpenTunnel(p.MTU, mac, peer, mac6)
	if err != nil {
		return api.Tunnel{}, -1, err
	}
	ifaces, err = s.sys.Interfaces()
	if err != nil {
		s.sys.CloseFD(fd)
		return api.Tunnel{}, -1, err
	}
	if other := carrying(ifaces, mac, name); other != "" {
		s.sys.CloseFD(fd)
		return api.Tunnel{}, -1, api.Refusal{Arg: "tunnel", Value: other, Reason: fmt.Sprintf("%s took the tunnel address %s at the same time", other, mac)}
	}
	s.unisolate(ifaces, name)
	t := api.Tunnel{Interface: name, Mac: mac.String(), Peer: peer.String(), MTU: p.MTU}
	if mac6.IsValid() {
		t.Mac6 = mac6.String()
	}
	return t, fd, nil
}

func (s *Server) unisolate(ifaces []Interface, tunnel string) {
	base := s.plan.Base()
	for _, i := range ifaces {
		if i.Name == tunnel {
			continue
		}
		for _, a := range i.Addrs {
			if base.Contains(a.Addr()) {
				return
			}
		}
	}
	gone, err := s.sys.Unisolate(base)
	switch {
	case err != nil:
		s.logf("tunnel.open: the isolation table: %v", err)
	case gone:
		s.logf("tunnel.open: %s taken out of macOS's isolation table for vmnet networks (left by a network gone)", base)
	}
}

func carrying(ifaces []Interface, a netip.Addr, except string) string {
	for _, i := range ifaces {
		if i.Name != except && i.Has(a) {
			return i.Name
		}
	}
	return ""
}

func (s *Server) portBind(p api.PortParams) (api.Bound, int, error) {
	if err := api.CheckPort(p); err != nil {
		return api.Bound{}, -1, err
	}
	addr := netip.MustParseAddr("127.0.0.1")
	if p.Family == 6 {
		addr = netip.IPv6Loopback()
	}
	ap := netip.AddrPortFrom(addr, uint16(p.Port))
	fd, err := s.sys.BindPort(p.Proto, ap)
	if err != nil {
		return api.Bound{}, -1, fmt.Errorf("binding %s %s: %w", p.Proto, ap, err)
	}
	return api.Bound{Address: ap.String()}, fd, nil
}
