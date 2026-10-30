package main

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"fev/guest/internal/fw"
	"fev/guest/internal/logbuf"
	api "fev/proto/agent"
	"fev/proto/netplan"
)

type direct struct {
	s          *system
	web        atomic.Bool
	setting    atomic.Int32
	webPending atomic.Bool

	mu           sync.Mutex
	dev, uplink  string
	domains      *api.DomainsParams
	targets      []fw.NameTarget
	targets6     []fw.NameTarget
	machines6    []fw.MachineTarget6
	machines     []netip.Addr
	unredirected map[netip.AddrPort]bool
	services     []fw.ServiceTarget
	k8sweb       string
}

func newDirect(s *system) *direct {
	d := &direct{s: s, unredirected: map[netip.AddrPort]bool{}}
	d.web.Store(webProxyOn(s.cmdline))
	var mu sync.Mutex
	was := ""
	s.engine.sup.Subscribe(func(st api.EngineStatus) {
		mu.Lock()
		became := st.State == api.EngineReady && was != api.EngineReady
		was = st.State
		mu.Unlock()
		if became {
			go d.apply("the engine is ready")
		}
	})
	return d
}

func (d *direct) uplinkUp(dev string) error {
	d.mu.Lock()
	d.uplink = dev
	d.mu.Unlock()
	if t := tunnelDevice(d.s.cmdline); t != "" {
		dev = t
	}
	d.mu.Lock()
	d.dev = dev
	d.mu.Unlock()
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return err
	}
	names := d.s.plan.Names()
	route := &netlink.Route{
		LinkIndex: lo.Attrs().Index, Dst: &net.IPNet{IP: names.Addr().AsSlice(), Mask: net.CIDRMask(names.Bits(), 32)},
		Table: unix.RT_TABLE_LOCAL, Type: unix.RTN_LOCAL, Scope: netlink.SCOPE_HOST, Protocol: unix.RTPROT_KERNEL,
	}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("the names' range %s as a local route: %w", names, err)
	}
	if d.s.ok6 {
		n6 := d.s.plan6.Names6()
		route6 := &netlink.Route{
			LinkIndex: lo.Attrs().Index, Dst: &net.IPNet{IP: n6.Addr().AsSlice(), Mask: net.CIDRMask(n6.Bits(), 128)},
			Table: unix.RT_TABLE_LOCAL, Type: unix.RTN_LOCAL, Scope: netlink.SCOPE_HOST, Protocol: unix.RTPROT_KERNEL,
		}
		if err := netlink.RouteReplace(route6); err != nil {
			return fmt.Errorf("the names' IPv6 range %s as a local route: %w", n6, err)
		}
	}
	return nil
}

func (d *direct) uplinkName() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.uplink
}

func (d *direct) setWeb(on bool) {
	if d.web.Swap(on) == on {
		return
	}
	if d.setting.Load() > 0 {
		d.webPending.Store(true)
		return
	}
	go d.apply("the web ports' listeners are " + map[bool]string{true: "open", false: "closed"}[on])
}

var errNoUplink = errors.New("there is no uplink: no direct path")

func (d *direct) apply(why string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.applyLocked(why)
}

func (d *direct) applyLocked(why string) error {
	if d.dev == "" {
		return errNoUplink
	}
	plan := d.s.plan
	https := true
	if d.domains != nil {
		https = d.domains.HTTPS
	}
	rules := fw.Direct{
		Uplink: d.dev, TunnelMac: plan.TunnelMac(), TunnelMac46: plan.TunnelMac46(), Names: plan.Names(), Index: plan.Index(),
		WebHTTP: netplan.WebHTTPPort, WebHTTPS: netplan.WebHTTPSPort, Web: d.web.Load(), HTTPS: https,
		Targets:  d.targets,
		Machines: plan.Machines(), MachineTargets: d.machines, Unredirected: d.unredirected,
	}
	if d.s.ok6 {
		p6 := d.s.plan6
		rules.TunnelMac6, rules.Names6, rules.Index6 = p6.TunnelMac6(), p6.Names6(), p6.Name6(plan.Index())
		rules.Targets6, rules.Machines6, rules.MachineTargets6 = d.targets6, p6.Subnet(netplan.Subnet6Machines), d.machines6
	}
	tool := fw.Tool{Root: engineImage, Reaper: d.s.reaper}
	if err := tool.Apply(rules.Script); err != nil {
		d.s.log.Logf(logbuf.Error, "init: the direct path's rules (%s): %v", why, err)
		return err
	}
	if k8sweb := fw.K8sWeb(d.services, netplan.WebHTTPPort, netplan.WebHTTPSPort, d.web.Load(), https); k8sweb != d.k8sweb && (d.k8sweb != "" || k8sweb != fw.K8sWebNone) {
		if err := tool.Nft(k8sweb); err != nil {
			d.s.log.Logf(logbuf.Error, "init: the cluster's LoadBalancers' web ports (%s): %v", why, err)
		} else {
			d.k8sweb = k8sweb
		}
	}
	if rules.TunnelMac6.IsValid() {
		if err := tool.Apply6(rules.Script6); err != nil {
			d.s.log.Logf(logbuf.Error, "init: the direct path's IPv6 rules (%s): %v", why, err)
		}
	}
	d.s.log.Logf(logbuf.Info, "init: the direct path's rules made (%s): %d names", why, len(d.targets))
	return nil
}

func (s *system) DomainsSet(p api.DomainsParams) (api.DomainsApplied, error) {
	s.direct.setting.Add(1)
	r, kept, err := s.direct.take(p)
	if kept {
		s.web.domains(p)
	}
	s.direct.setting.Add(-1)
	pending := s.direct.webPending.Swap(false)
	if !kept {
		if pending {
			go s.direct.apply("the web ports' listeners changed")
		}
		return r, err
	}
	if err := s.direct.apply(fmt.Sprintf("domains.set %d", p.Seq)); err != nil {
		return api.DomainsApplied{}, err
	}
	return r, nil
}

func (d *direct) take(p api.DomainsParams) (_ api.DomainsApplied, kept bool, _ error) {
	if err := api.CheckDomains(p); err != nil {
		return api.DomainsApplied{}, false, err
	}
	plan := d.s.plan
	if p.Index != plan.Index().String() {
		return api.DomainsApplied{}, false, fmt.Errorf("index %s is not the plan's %s", p.Index, plan.Index())
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.domains != nil && p.Seq < d.domains.Seq {
		return api.DomainsApplied{Seq: d.domains.Seq, Targets: len(d.domains.Targets)}, false, nil
	}
	var out api.DomainsApplied
	var targets, targets6 []fw.NameTarget
	var machines []netip.Addr
	var machines6 []fw.MachineTarget6
	var services []fw.ServiceTarget
	inForce := 0
	for _, t := range p.Targets {
		a := netip.MustParseAddr(t.Address)
		switch t.Kind {
		case api.TargetContainer:
			if !plan.NameAddress(a) {
				out.Errors = append(out.Errors, fmt.Sprintf("%s: not one of the names' range %s", t.Address, plan.Names()))
				continue
			}
			targets = append(targets, fw.NameTarget{Address: a, Target: netip.MustParseAddr(t.Target)})
			if d.s.ok6 {
				t6 := fw.NameTarget{Address: d.s.plan6.Name6(a)}
				if a6, err := netip.ParseAddr(t.Target6); err == nil {
					t6.Target = a6
				}
				targets6 = append(targets6, t6)
			}
		case api.TargetMachine:
			if !plan.Machines().Contains(a) {
				out.Errors = append(out.Errors, fmt.Sprintf("%s: not one of the machines' %s", t.Address, plan.Machines()))
				continue
			}
			machines = append(machines, a)
			if a6, err := netip.ParseAddr(t.Address6); err == nil && d.s.ok6 && d.s.plan6.Subnet(netplan.Subnet6Machines).Contains(a6) {
				machines6 = append(machines6, fw.MachineTarget6{Address6: a6, Address: a})
			}
		case api.TargetK8s:
			if !plan.KubeLoadBalancers().Contains(a) {
				out.Errors = append(out.Errors, fmt.Sprintf("%s: not one of the cluster's load balancers' %s", t.Address, plan.KubeLoadBalancers()))
				continue
			}
			services = append(services, fw.ServiceTarget{Address: a, Redirect80: t.Redirect80, Own443: t.Own443})
		}
		inForce++
	}
	d.domains, d.targets, d.machines, d.services = &p, targets, machines, services
	d.targets6, d.machines6 = targets6, machines6
	out.Seq, out.Targets = p.Seq, inForce
	return out, true, nil
}

func (d *direct) unredirect(ap netip.AddrPort) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !slices.Contains(d.machines, ap.Addr()) || d.unredirected[ap] {
		return
	}
	d.unredirected[ap] = true
	d.applyLocked(fmt.Sprintf("%s is served by its machine", ap))
}

func (d *direct) machineGone(addr netip.Addr) {
	d.mu.Lock()
	defer d.mu.Unlock()
	changed := false
	for ap := range d.unredirected {
		if ap.Addr() == addr {
			delete(d.unredirected, ap)
			changed = true
		}
	}
	if changed {
		d.applyLocked(fmt.Sprintf("the machine at %s stopped", addr))
	}
}
