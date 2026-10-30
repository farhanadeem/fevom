package docker

import (
	"context"
	"net/netip"
	"sort"
	"sync"

	"fev/host/daemon"
	"fev/host/docker/forward"
	"fev/proto/agent"
	api "fev/proto/daemon"
)

const HostNetworkOwner = "host-network"

type enginePorts struct {
	m *Module

	mu   sync.Mutex
	boot string
	set  *agent.EnginePorts
}

func EngineWants(p agent.EnginePorts) []forward.Want {
	at, err := netip.ParseAddr(p.Address)
	if err != nil || !at.Is4() {
		at = netip.AddrFrom4([4]byte{127, 0, 0, 1})
	}
	seen := map[forward.Want]bool{}
	var wants []forward.Want
	for _, ep := range p.Ports {
		var proto string
		switch ep.Proto {
		case agent.PortTCP:
			proto = forward.TCP
		case agent.PortUDP:
			proto = forward.UDP
		default:
			continue
		}
		if ep.Port == 0 {
			continue
		}
		a, err := netip.ParseAddr(ep.Address)
		if err != nil {
			continue
		}
		if a = a.Unmap(); !a.IsUnspecified() && !(a.Is4() && a.IsLoopback()) {
			continue
		}
		w := forward.Want{Proto: proto, Port: ep.Port, Guest: netip.AddrPortFrom(at, ep.Port)}
		if !seen[w] {
			seen[w] = true
			wants = append(wants, w)
		}
	}
	sort.Slice(wants, func(i, j int) bool {
		if wants[i].Port != wants[j].Port {
			return wants[i].Port < wants[j].Port
		}
		return wants[i].Proto < wants[j].Proto
	})
	return wants
}

func (p *enginePorts) follow(vm daemon.VM) {
	boot := vm.BootID()
	p.mu.Lock()
	if p.boot == boot {
		p.mu.Unlock()
		return
	}
	p.boot, p.set = boot, nil
	p.mu.Unlock()
	p.m.table.Set(api.ForwardDocker, HostNetworkOwner, nil)
	a := vm.Agent()
	if a == nil {
		return
	}
	a.OnEnginePorts(func(ep agent.EnginePorts) { p.apply(boot, ep) })
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), machinePortsBound)
		l, err := a.EnginePorts(ctx)
		cancel()
		if err != nil {
			p.m.log.Warn("docker: the guest did not say what host-network containers listen on; their ports are forwarded as they change", "boot", boot, "err", err)
			return
		}
		p.apply(boot, *l)
	}()
}

func (p *enginePorts) apply(boot string, ep agent.EnginePorts) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.boot != boot || (p.set != nil && p.set.Seq >= ep.Seq) {
		return
	}
	p.set = &ep
	p.m.table.Set(api.ForwardDocker, HostNetworkOwner, EngineWants(ep))
}

func (p *enginePorts) again() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.set != nil {
		p.m.table.Set(api.ForwardDocker, HostNetworkOwner, EngineWants(*p.set))
	}
}

func (p *enginePorts) gone() {
	p.mu.Lock()
	p.boot, p.set = "", nil
	p.mu.Unlock()
	p.m.table.Set(api.ForwardDocker, HostNetworkOwner, nil)
}
