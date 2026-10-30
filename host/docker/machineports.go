package docker

import (
	"context"
	"net/netip"
	"sort"
	"sync"
	"time"

	"fev/host/daemon"
	"fev/host/docker/forward"
	"fev/proto/agent"
	api "fev/proto/daemon"
)

type machinePorts struct {
	m *Module

	mu      sync.Mutex
	on, lan bool
	boot    string
	sets    map[string]agent.MachinePorts
}

func MachineWants(p agent.MachinePorts, lan bool) []forward.Want {
	machine, err := netip.ParseAddr(p.Address)
	if err != nil || !machine.Is4() {
		return nil
	}
	type key struct {
		proto string
		port  uint16
	}
	byPort := map[key]forward.Want{}
	for _, mp := range p.Ports {
		var proto string
		switch mp.Proto {
		case agent.PortTCP:
			proto = forward.TCP
		case agent.PortUDP:
			proto = forward.UDP
		default:
			continue
		}
		if mp.Port == 0 || (proto == forward.UDP && machineOwnUDP(mp.Port)) {
			continue
		}
		a, err := netip.ParseAddr(mp.Address)
		if err != nil {
			continue
		}
		a = a.Unmap()
		var wide bool
		switch {
		case a.IsUnspecified():
			wide = lan
		case a == machine:
		default:
			continue
		}
		k := key{proto, mp.Port}
		w := byPort[k]
		w.Proto, w.Port, w.Guest = proto, mp.Port, netip.AddrPortFrom(machine, mp.Port)
		w.LAN = w.LAN || wide
		byPort[k] = w
	}
	wants := make([]forward.Want, 0, len(byPort))
	for _, w := range byPort {
		wants = append(wants, w)
	}
	sort.Slice(wants, func(i, j int) bool {
		if wants[i].Port != wants[j].Port {
			return wants[i].Port < wants[j].Port
		}
		return wants[i].Proto < wants[j].Proto
	})
	return wants
}

func machineOwnUDP(port uint16) bool { return port == 68 || port == 546 || port == 5353 }

func (p *machinePorts) follow(vm daemon.VM) {
	boot := vm.BootID()
	p.mu.Lock()
	if p.boot == boot {
		p.mu.Unlock()
		return
	}
	p.boot = boot
	p.sets = map[string]agent.MachinePorts{}
	p.mu.Unlock()
	p.m.table.Drop(api.ForwardMachine)
	a := vm.Agent()
	if a == nil {
		return
	}
	a.OnMachinePorts(func(mp agent.MachinePorts) { p.apply(boot, mp) })
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), machinePortsBound)
		l, err := a.MachinePorts(ctx)
		cancel()
		if err != nil {
			p.m.log.Warn("docker: the guest did not say what its machines listen on; their ports are forwarded as they change", "boot", boot, "err", err)
			return
		}
		for _, mp := range l.Machines {
			p.apply(boot, mp)
		}
	}()
}

const machinePortsBound = 30 * time.Second

func (p *machinePorts) apply(boot string, mp agent.MachinePorts) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.boot != boot {
		return
	}
	if old, ok := p.sets[mp.Name]; ok && old.Seq >= mp.Seq {
		return
	}
	p.sets[mp.Name] = mp
	if p.on {
		p.m.table.Set(api.ForwardMachine, mp.Name, MachineWants(mp, p.lan))
	}
}

func (p *machinePorts) gone() {
	p.mu.Lock()
	p.boot, p.sets = "", nil
	p.mu.Unlock()
	p.m.table.Drop(api.ForwardMachine)
}

func (p *machinePorts) setOn(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.on = on
	p.reapplyLocked()
}

func (p *machinePorts) setLAN(lan bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lan = lan
	p.reapplyLocked()
}

func (p *machinePorts) reapplyLocked() {
	if !p.on {
		p.m.table.Drop(api.ForwardMachine)
		return
	}
	names := make([]string, 0, len(p.sets))
	for name := range p.sets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p.m.table.Set(api.ForwardMachine, name, MachineWants(p.sets[name], p.lan))
	}
}
