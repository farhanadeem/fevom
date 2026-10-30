package machnet

import (
	"net/netip"
	"sort"
	"sync"

	"fev/guest/internal/listen"
	api "fev/proto/agent"
)

type portSet struct {
	name    string
	address netip.Addr
	netns   uint64
	socks   map[uint64]listen.Listener
	seq     uint64
}

type ports struct {
	mu     sync.Mutex
	seq    uint64
	byNS   map[uint64]*portSet
	byName map[string]*portSet
	gone   map[string]uint64
	dirty  map[string]bool
	wake   chan struct{}
	follow func(api.MachinePorts)
}

func newPorts() *ports {
	return &ports{
		byNS: map[uint64]*portSet{}, byName: map[string]*portSet{},
		gone: map[string]uint64{}, dirty: map[string]bool{}, wake: make(chan struct{}, 1),
	}
}

func (p *ports) add(name string, addr netip.Addr, netns uint64, seed func(netns uint64) []listen.Listener) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if old := p.byName[name]; old != nil {
		delete(p.byNS, old.netns)
	}
	s := &portSet{name: name, address: addr, netns: netns, socks: map[uint64]listen.Listener{}}
	if seed != nil {
		for _, l := range seed(netns) {
			s.socks[l.Cookie] = l
		}
	}
	p.byNS[netns], p.byName[name] = s, s
	delete(p.gone, name)
	p.changed(s)
}

func (p *ports) remove(_ string, netns uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.byNS[netns]
	if s == nil {
		return
	}
	delete(p.byNS, netns)
	if p.byName[s.name] == s {
		delete(p.byName, s.name)
	}
	p.seq++
	p.gone[s.name] = p.seq
	p.dirty[s.name] = true
	p.poke()
}

func (p *ports) rename(old, new string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.byName[old]
	if s == nil || old == new {
		return
	}
	if other := p.byName[new]; other != nil {
		delete(p.byNS, other.netns)
	}
	delete(p.byName, old)
	p.seq++
	p.gone[old] = p.seq
	p.dirty[old] = true
	s.name = new
	p.byName[new] = s
	delete(p.gone, new)
	p.changed(s)
}

func (p *ports) event(ev listen.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.byNS[ev.NetNS]
	if s == nil {
		return
	}
	_, had := s.socks[ev.Cookie]
	if ev.Added {
		s.socks[ev.Cookie] = ev.Listener
	} else if had {
		delete(s.socks, ev.Cookie)
	} else {
		return
	}
	p.changed(s)
}

func (p *ports) changed(s *portSet) {
	p.seq++
	s.seq = p.seq
	p.dirty[s.name] = true
	p.poke()
}

func (p *ports) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (s *portSet) message() api.MachinePorts {
	m := api.MachinePorts{Name: s.name, Seq: s.seq, Ports: []api.MachinePort{}}
	if s.address.IsValid() {
		m.Address = s.address.String()
	}
	seen := map[api.MachinePort]bool{}
	for _, l := range s.socks {
		mp := api.MachinePort{Proto: l.Proto, Address: l.Addr.String(), Port: l.Port}
		if !seen[mp] {
			seen[mp] = true
			m.Ports = append(m.Ports, mp)
		}
	}
	sortPorts(m.Ports)
	return m
}

func sortPorts(ps []api.MachinePort) {
	sort.Slice(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if a.Proto != b.Proto {
			return a.Proto < b.Proto
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Address < b.Address
	})
}

func (p *ports) list() api.MachinePortsList {
	p.mu.Lock()
	defer p.mu.Unlock()
	l := api.MachinePortsList{Machines: []api.MachinePorts{}}
	for _, s := range p.byName {
		l.Machines = append(l.Machines, s.message())
	}
	sort.Slice(l.Machines, func(i, j int) bool { return l.Machines[i].Name < l.Machines[j].Name })
	return l
}

func (p *ports) next() []api.MachinePorts {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []api.MachinePorts
	for name := range p.dirty {
		if s := p.byName[name]; s != nil {
			out = append(out, s.message())
		} else if seq, ok := p.gone[name]; ok {
			out = append(out, api.MachinePorts{Name: name, Seq: seq, Ports: []api.MachinePort{}})
			delete(p.gone, name)
		}
	}
	clear(p.dirty)
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func (p *ports) run(fn func(api.MachinePorts)) {
	for range p.wake {
		for _, m := range p.next() {
			fn(m)
		}
	}
}
