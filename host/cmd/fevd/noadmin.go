package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"strconv"
	"sync"

	"fev/host/daemon"
	"fev/host/domains"
	"fev/host/macnet"
	"fev/host/vmnet"
	api "fev/proto/daemon"
	"fev/proto/netplan"
	priv "fev/proto/privhelper"
)

type vmnetPath struct {
	names *daemon.Names
	log   *slog.Logger
	mtu   int
	tso   bool

	mu     sync.Mutex
	i      *vmnet.Interface
	cancel func()
	done   chan struct{}
}

func newVmnetPath(names *daemon.Names) *vmnetPath {
	if !vmnet.Supported {
		return nil
	}
	p := &vmnetPath{names: names, log: slog.Default()}
	if v := os.Getenv("FEV_VMNET_MTU"); v != "" {
		m, err := strconv.Atoi(v)
		if err != nil || m < vmnet.MinMTU || m > vmnet.MaxMTU {
			fail("FEV_VMNET_MTU=%q: from %d to %d", v, vmnet.MinMTU, vmnet.MaxMTU)
		}
		p.mtu = m
	}
	p.tso = os.Getenv("FEV_VMNET_TSO") == "1"
	return p
}

func (p *vmnetPath) Open(plan netplan.Plan, log *slog.Logger, stopped func()) (*os.File, string, int, error) {
	p.mu.Lock()
	if log != nil {
		p.log = log
	}
	defer p.mu.Unlock()
	if p.i != nil {
		return nil, "", 0, fmt.Errorf("vmnet: the interface is open already")
	}
	cfg := vmnet.Config{Base: plan.Base(), Mac: plan.TunnelMac(), Peer: plan.TunnelPeer(), Prefixes: plan.Routed(), MTU: p.mtu, TSO: p.tso}
	if r, err := vmnet.RouteOf(plan.Index()); err == nil && r.Prefix == plan.Base() {
		for _, h := range macnet.HeldIn(priv.RecordPath, 0) {
			if r.Reject && h.Prefix.Masked() == plan.Base() {
				return nil, "", 0, fmt.Errorf("%w (%s on %s)", daemon.ErrHelperHeld, h.Prefix, r.Interface)
			}
		}
		return nil, "", 0, fmt.Errorf("the Mac has a route of fev's base %s already, on %s: fev's bridge is not started over it", plan.Base(), r.Interface)
	}
	var i *vmnet.Interface
	for try := 1; ; try++ {
		var err error
		if i, err = vmnet.Start(cfg); err != nil {
			if errors.Is(err, vmnet.ErrNotPrivate) {
				err = fmt.Errorf(`%w: "fev config set %s 10.198.0.0/16" (or another private /16 this Mac does not use), then "fev reset"; or "sudo fev setup --admin" for the admin helper`, err, api.KeyNetworkSubnet)
			}
			return nil, "", 0, err
		}
		if i.Name() != "" {
			break
		}
		i.Close()
		if try == 2 {
			return nil, "", 0, fmt.Errorf("vmnet started the interface twice and no bridge of the Mac's took %s in 10 s (bridges: %s)", plan.TunnelMac(), vmnet.Bridges())
		}
		p.log.Warn("vmnet: the interface's bridge did not appear in 10 s: asked again", "address", plan.TunnelMac(), "bridges", vmnet.Bridges())
	}
	if r, err := vmnet.RouteOf(plan.Index()); err == nil && r.Interface != i.Name() {
		name := i.Name()
		i.Close()
		for _, h := range macnet.HeldIn(priv.RecordPath, 0) {
			if r.Reject && h.Prefix.Masked() == plan.Base() {
				return nil, "", 0, fmt.Errorf("%w (%s on %s)", daemon.ErrHelperHeld, h.Prefix, r.Interface)
			}
		}
		return nil, "", 0, fmt.Errorf("the Mac sends %s by %s, not by fev's bridge %s", plan.Index(), r.Interface, name)
	}
	i.ServeMDNS(&vmnet.Answerer{Lookup: p.lookup, TTL: domains.PositiveTTL})
	p.zones(i)
	cancel := p.names.Subscribe(func() { p.zones(i) })
	p.names.SetMDNS(i.Name())
	done := make(chan struct{})
	go func() {
		select {
		case <-i.Stopped():
			stopped()
		case <-done:
		}
	}()
	p.i, p.cancel, p.done = i, cancel, done
	p.log.Info("vmnet: the no-admin path's interface", "bridge", i.Name(), "mtu", i.MTU(), "tso", p.tso, "link", i.MAC().String())
	return i.Tunnel(), i.Name(), i.PacketMTU(), nil
}

func (p *vmnetPath) lookup(name string) (netip.Addr, bool) {
	reg := p.names.Registry()
	if reg == nil {
		return netip.Addr{}, false
	}
	if a, found, fwd := reg.ForwardA(name); fwd {
		return a, found
	}
	if !reg.Owns(name) {
		return netip.Addr{}, false
	}
	a := reg.Lookup(name)
	if !a.Found {
		return netip.Addr{}, false
	}
	return a.Address, true
}

func (p *vmnetPath) zones(i *vmnet.Interface) {
	zs := []string{domains.Zone}
	if reg := p.names.Registry(); reg != nil {
		for _, l := range reg.LocalNames() {
			zs = append(zs, l.Name)
		}
	}
	i.SetZones(zs)
}

func (p *vmnetPath) SetPrefixes(ps []netip.Prefix) {
	p.mu.Lock()
	i := p.i
	p.mu.Unlock()
	if i != nil {
		i.SetPrefixes(ps)
	}
}

func (p *vmnetPath) Close() {
	p.mu.Lock()
	i, cancel, done, log := p.i, p.cancel, p.done, p.log
	p.i, p.cancel, p.done = nil, nil, nil
	p.mu.Unlock()
	if i == nil {
		return
	}
	cancel()
	close(done)
	p.names.SetMDNS("")
	i.Close()
	log.Info("vmnet: the no-admin path's interface stopped", "count", i.Stats().String())
}
