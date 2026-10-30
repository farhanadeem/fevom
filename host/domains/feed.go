package domains

import (
	"net/netip"

	"fev/host/docker/engine"
	"fev/proto/agent"
)

func (r *Registry) Containers() *Feed { return &Feed{r: r} }

type Feed struct{ r *Registry }

func (f *Feed) Container(id string, c *engine.Container) {
	if c == nil || !c.Running {
		f.r.SetContainer(id, nil)
		return
	}
	nets := make([]Network, 0, len(c.Networks))
	for _, n := range c.Networks {
		nets = append(nets, Network{Name: n.Name, Address: n.Address, Address6: n.Address6})
	}
	f.r.SetContainer(id, &Container{ID: c.ID, Name: c.Name, Labels: c.Labels, NetworkMode: c.NetworkMode, Networks: nets, SandboxKey: c.SandboxKey, Exposed: c.Exposed})
}

func (f *Feed) Synced(running map[string]bool) { f.r.Synced(running) }

func (f *Feed) Clear() { f.r.ClearContainers() }

func (r *Registry) Machine(st agent.MachineState, httpPort, httpsPort int) {
	ip, err := netip.ParseAddr(st.Address)
	if st.Removed || st.State != agent.MachineRunning || err != nil || !ip.Is4() {
		r.SetMachine(st.Name, nil)
		return
	}
	m := &Machine{Name: st.Name, Address: ip, HTTPPort: httpPort, HTTPSPort: httpsPort}
	if ip6, err := netip.ParseAddr(st.Address6); err == nil && ip6.Is6() && !ip6.Is4In6() {
		m.Address6 = ip6
	}
	r.SetMachine(st.Name, m)
}
