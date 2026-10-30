package netstack

import (
	"net"
	"net/netip"
	"sync"
)

type MacAddresses interface {
	HasAddress(netip.Addr) bool
}

type udpFlowSet struct {
	mu    sync.Mutex
	flows map[*liveUDPFlow]struct{}
}

type liveUDPFlow struct {
	local netip.Addr
	end   func()
}

func (u *udpFlowSet) add(conn net.Conn, end func()) (remove func()) {
	ua, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return func() {}
	}
	local, ok := netip.AddrFromSlice(ua.IP)
	if !ok {
		return func() {}
	}
	f := &liveUDPFlow{local: local.Unmap(), end: end}
	u.mu.Lock()
	if u.flows == nil {
		u.flows = map[*liveUDPFlow]struct{}{}
	}
	u.flows[f] = struct{}{}
	u.mu.Unlock()
	return func() {
		u.mu.Lock()
		delete(u.flows, f)
		u.mu.Unlock()
	}
}

func (s *Stack) MacAddressesChanged(mac MacAddresses) int {
	var gone []*liveUDPFlow
	s.udpFlows.mu.Lock()
	for f := range s.udpFlows.flows {
		if !f.local.IsUnspecified() && !mac.HasAddress(f.local) {
			gone = append(gone, f)
		}
	}
	s.udpFlows.mu.Unlock()
	for _, f := range gone {
		f.end()
	}
	if len(gone) > 0 {
		s.log.Debug("udp: flows ended, their local address gone", "flows", len(gone))
	}
	return len(gone)
}
