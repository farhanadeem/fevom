package machnet

import (
	"net/netip"

	"fev/guest/internal/listen"
	api "fev/proto/agent"
)

func (n *Network) ListenEvent(ev listen.Event) { n.ports.event(ev) }

func (n *Network) Ports() api.MachinePortsList { return n.ports.list() }

func (n *Network) FollowPorts(fn func(api.MachinePorts)) { go n.ports.run(fn) }

func (n *Network) SetListeners(f func(netns uint64) []listen.Listener) {
	n.mu.Lock()
	n.listeners = f
	n.mu.Unlock()
}

func (n *Network) MachineAddress(addr netip.Addr) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, m := range n.byName {
		if m.addr == addr {
			return true
		}
	}
	return false
}
