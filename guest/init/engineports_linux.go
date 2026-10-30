package main

import (
	"os"
	"path/filepath"
	"sort"
	"sync"

	"golang.org/x/sys/unix"

	"fev/guest/internal/listen"
	"fev/guest/internal/logbuf"
	api "fev/proto/agent"
)

const engineContainers = engineCgroup + "/docker"

type enginePorts struct {
	log *logbuf.Buffer

	mu      sync.Mutex
	netns   uint64
	w       *listen.Watcher
	running bool
	detach  func()
	socks   map[uint64]listen.Listener
	seq     uint64
	dirty   bool
	wake    chan struct{}
}

func newEnginePorts(log *logbuf.Buffer) *enginePorts {
	return &enginePorts{log: log, socks: map[uint64]listen.Listener{}, wake: make(chan struct{}, 1)}
}

func (p *enginePorts) watcher(w *listen.Watcher, netns uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.w, p.netns = w, netns
	if p.running {
		p.attachLocked()
	}
}

func (p *enginePorts) started() {
	if err := os.Mkdir(engineContainers, 0o755); err != nil && !os.IsExist(err) {
		p.log.Logf(logbuf.Warning, "init: host-network containers' ports are not followed: %v", err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = true
	p.attachLocked()
}

func (p *enginePorts) attachLocked() {
	if p.w == nil || p.detach != nil {
		return
	}
	detach, err := p.w.AttachAt(engineContainers)
	if err != nil {
		p.log.Logf(logbuf.Warning, "init: host-network containers' ports are not followed: %v", err)
		return
	}
	p.detach = detach
}

func (p *enginePorts) ended() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.detach != nil {
		p.detach()
		p.detach = nil
	}
	p.running = false
	if len(p.socks) > 0 {
		clear(p.socks)
		p.changedLocked()
	}
}

func (p *enginePorts) event(ev listen.Event) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.netns == 0 || ev.NetNS != p.netns {
		return false
	}
	_, had := p.socks[ev.Cookie]
	switch {
	case ev.Added:
		p.socks[ev.Cookie] = ev.Listener
	case had:
		delete(p.socks, ev.Cookie)
	default:
		return true
	}
	p.changedLocked()
	return true
}

func (p *enginePorts) changedLocked() {
	p.seq++
	p.dirty = true
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *enginePorts) messageLocked() api.EnginePorts {
	m := api.EnginePorts{Address: "127.0.0.1", Seq: p.seq, Ports: []api.MachinePort{}}
	seen := map[api.MachinePort]bool{}
	for _, l := range p.socks {
		mp := api.MachinePort{Proto: l.Proto, Address: l.Addr.String(), Port: l.Port}
		if !seen[mp] {
			seen[mp] = true
			m.Ports = append(m.Ports, mp)
		}
	}
	sort.Slice(m.Ports, func(i, j int) bool {
		a, b := m.Ports[i], m.Ports[j]
		if a.Proto != b.Proto {
			return a.Proto < b.Proto
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Address < b.Address
	})
	return m
}

func (p *enginePorts) list() api.EnginePorts {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.messageLocked()
}

func (p *enginePorts) run(fn func(api.EnginePorts)) {
	for range p.wake {
		p.mu.Lock()
		if !p.dirty {
			p.mu.Unlock()
			continue
		}
		p.dirty = false
		m := p.messageLocked()
		p.mu.Unlock()
		fn(m)
	}
}

func (s *system) EnginePorts() api.EnginePorts { return s.engPorts.list() }

func initNetNSCookie() (uint64, error) {
	fd, err := unix.Open(filepath.Join("/proc/self/ns/net"), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	return listen.NetNSCookie(fd)
}
