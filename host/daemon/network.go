package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"sort"
	"sync"
	"syscall"
	"time"

	"fev/host/daemon/config"
	api "fev/proto/daemon"
	"fev/proto/netplan"
	priv "fev/proto/privhelper"
)

const (
	WarnHelper   = "network.helper"
	warnConflict = "network.conflict "
)

const tunnelMTU = 16384

type HelperClient interface {
	Call(ctx context.Context, method string, params, result any) (*os.File, error)
}

type SocketHelper struct {
	Path string
}

func (s SocketHelper) Call(ctx context.Context, method string, params, result any) (*os.File, error) {
	path := s.Path
	if path == "" {
		path = priv.SocketPath
	}
	return priv.Call(ctx, path, method, params, result)
}

type MacNetwork interface {
	Check(ours []netip.Prefix, tunnel string) (keep []netip.Prefix, conflicts []api.RouteConflict, warnings []string)
	Taken(tunnel string) []netip.Prefix
	Subscribe(fn func()) (cancel func())
	Refresh(reason string)
	NAT64() []netip.Prefix
}

type Tunnel interface {
	Attach(f *os.File, e TunnelEnds)
	SetPrefixes(ps []netip.Prefix)
	SetNAT64(ps []netip.Prefix)
	Detach()
}

type TunnelEnds struct {
	Interface          string
	Mac, Peer          netip.Addr
	Mac6, Peer6, Mac46 netip.Addr
	MTU                int
}

type Network struct {
	Helper     HelperClient
	Mac        MacNetwork
	StartMac   func(log *slog.Logger) MacNetwork
	Tunnel     Tunnel
	MTU        int
	NAT64      func() []netip.Prefix
	Tunnel6    func(on bool)
	Hello      func(h *priv.HelloResult)
	MacChanged func()
	Extra      func() []netip.Prefix
	NoAdmin    NoAdminPath

	host Host
	cfg  *config.Store

	rerouting sync.Mutex
	mu        sync.Mutex
	opening   bool
	tun       *os.File
	iface     string
	via       string
	noHelper  string
	base      string
	mac6      netip.Addr
	routes    []netip.Prefix
	nat64     []netip.Prefix
	hello     *priv.HelloResult
	reason    string
	failure   string
	warned    map[string]bool
	cancel    func()
	closed    bool
	starting  bool
}

func (n *Network) Name() string    { return "direct access" }
func (n *Network) Marks() []string { return nil }

func (n *Network) Start(h Host) error {
	n.host, n.cfg = h, h.Config()
	if n.Helper == nil {
		n.Helper = SocketHelper{}
	}
	n.warned = map[string]bool{}
	if n.Mac == nil && n.StartMac != nil {
		n.Mac = n.StartMac(h.Log())
	}
	if n.Mac != nil {
		n.cancel = n.Mac.Subscribe(func() {
			go func() {
				n.reroute("the Mac's network changed")
				if n.MacChanged != nil {
					n.MacChanged()
				}
			}()
		})
	}
	if reason := n.offReason(); reason != "" {
		n.mu.Lock()
		n.reason = reason
		n.mu.Unlock()
		n.publish()
	}
	return nil
}

func (n *Network) offReason() string {
	switch {
	case !n.cfg.Bool(api.KeyAdminEnabled):
		return api.DirectAdminOff
	case !n.cfg.Bool(api.KeyNetworkDirectAccess):
		return api.DirectSetting
	}
	return ""
}

func (n *Network) StateChanged(st api.Status) {
	if st.State != api.StateStarting {
		n.mu.Lock()
		n.starting = false
		n.mu.Unlock()
		return
	}
	n.mu.Lock()
	stale := n.tun != nil && n.base != planInEffect(n.cfg).Base().String()
	first := !n.starting
	n.starting = true
	n.mu.Unlock()
	if stale {
		n.closeTunnel(api.DirectFailed)
	}
	n.mu.Lock()
	reason := n.offReason()
	need := n.tun == nil && !n.opening && !n.closed && (reason == "" || n.noAdminFor(reason))
	need = need || (first && n.tun != nil && n.via == api.ViaVmnet && reason == "" && !n.opening && !n.closed)
	link := first && !need && !n.opening && !n.closed && reason != api.DirectAdminOff
	if need {
		n.opening = true
	}
	n.mu.Unlock()
	if need {
		go n.open()
	} else if link {
		go n.linkAlone()
	}
}

func (n *Network) ConfigChanged(ch api.ConfigChange) {
	switch ch.Key {
	case api.KeyAdminEnabled, api.KeyNetworkDirectAccess:
		if reason := n.offReason(); reason != "" {
			n.mu.Lock()
			keep := n.noAdminFor(reason) && n.via == api.ViaVmnet && n.tun != nil
			n.mu.Unlock()
			if !keep {
				n.closeTunnel(reason)
			}
			if n.Hello != nil {
				n.Hello(nil)
			}
			if ch.Key == api.KeyAdminEnabled && reason == api.DirectSetting {
				go n.linkAlone()
			}
			if n.noAdminFor(reason) && !keep {
				n.startOpen()
			}
			return
		}
		n.mu.Lock()
		need := (n.tun == nil || (ch.Key == api.KeyAdminEnabled && n.via == api.ViaVmnet)) && !n.opening && !n.closed
		if need {
			n.opening = true
		}
		n.mu.Unlock()
		if need {
			go n.open()
		}
	case api.KeyNetworkSubnet:
	}
}

func (n *Network) Close() {
	n.mu.Lock()
	n.closed = true
	f, via := n.tun, n.via
	n.tun, n.via = nil, ""
	n.mu.Unlock()
	if n.cancel != nil {
		n.cancel()
	}
	if f != nil {
		n.release(f, via)
	}
	if c, ok := n.Mac.(io.Closer); ok {
		c.Close()
	}
}

func (n *Network) Taken() []netip.Prefix {
	if n.Mac == nil {
		return nil
	}
	n.mu.Lock()
	iface := n.iface
	n.mu.Unlock()
	return n.Mac.Taken(iface)
}

func (n *Network) call(method string, params, result any) (*os.File, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return n.Helper.Call(ctx, method, params, result)
}

func helperReason(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOENT):
		return api.DirectNoHelper
	case errors.Is(err, syscall.ECONNREFUSED):
		return api.DirectHelperOff
	case errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return api.DirectOtherUser
	}
	return api.DirectFailed
}

func (n *Network) open() {
	defer func() {
		n.mu.Lock()
		n.opening = false
		n.mu.Unlock()
	}()
	log := n.host.Log()
	if n.offReason() == api.DirectAdminOff {
		n.openNoAdmin(api.DirectAdminOff)
		return
	}
	var hello priv.HelloResult
	if _, err := n.call(priv.MethodHello, priv.Empty{}, &hello); err != nil {
		reason := helperReason(err)
		n.mu.Lock()
		n.hello, n.reason = nil, reason
		n.failure = ""
		if reason == api.DirectFailed && n.NoAdmin == nil {
			n.failure = "fev's admin helper did not answer: " + err.Error()
		}
		n.mu.Unlock()
		log.Info("direct access: no helper to use", "reason", reason, "err", err)
		if n.Hello != nil {
			n.Hello(nil)
		}
		if n.NoAdmin != nil {
			n.openNoAdmin(reason)
			return
		}
		n.publish()
		return
	}
	n.mu.Lock()
	n.hello = &hello
	n.mu.Unlock()
	if n.Hello != nil {
		if hello.ProtocolVersion == priv.ProtocolVersion {
			h := hello
			n.Hello(&h)
		} else {
			n.Hello(nil)
		}
	}
	if hello.ProtocolVersion != priv.ProtocolVersion {
		n.mu.Lock()
		n.reason, n.failure = api.DirectOutdated, ""
		n.mu.Unlock()
		log.Warn("direct access: the admin helper speaks another protocol", "helper", hello.ProtocolVersion, "ours", priv.ProtocolVersion)
		if n.NoAdmin != nil {
			n.openNoAdmin(api.DirectOutdated)
			return
		}
		n.publish()
		return
	}
	n.link(&hello)
	n.mu.Lock()
	onVmnet := n.tun != nil && n.via == api.ViaVmnet
	n.mu.Unlock()
	fallBack := func() {
		if !onVmnet && n.NoAdmin != nil {
			n.openNoAdmin(api.DirectFailed)
		} else {
			n.publish()
		}
	}
	plan := planInEffect(n.cfg)
	if hello.Base != plan.Base().String() {
		n.mu.Lock()
		n.reason = api.DirectFailed
		n.failure = fmt.Sprintf("fev's admin helper was installed for the addresses %s and fev now uses %s: run \"sudo fev setup --admin\" again", hello.Base, plan.Base())
		n.mu.Unlock()
		fallBack()
		return
	}
	var tunnel priv.Tunnel
	mtu := n.MTU
	if mtu == 0 {
		mtu = tunnelMTU
	}
	p6, has6 := plan6InEffect(n.cfg)
	ipv6 := has6 && hello.Base6 == p6.Prefix().String()
	if has6 && !ipv6 {
		log.Info("direct access: IPv4 alone: the admin helper recorded another IPv6 base", "helper", hello.Base6, "plan", p6.Prefix())
	}
	f, err := n.call(priv.MethodTunnelOpen, priv.TunnelParams{MTU: mtu, IPv6: ipv6}, &tunnel)
	if r, ok := priv.AsRefusal(err); ok && ipv6 && r.Arg == "ipv6" {
		log.Warn("direct access: IPv4 alone: the admin helper refused the IPv6 /128", "err", err)
		ipv6 = false
		f, err = n.call(priv.MethodTunnelOpen, priv.TunnelParams{MTU: mtu}, &tunnel)
	}
	if err != nil {
		n.mu.Lock()
		n.reason, n.failure = api.DirectFailed, "the admin helper did not open the tunnel: "+err.Error()
		n.mu.Unlock()
		log.Warn("direct access: tunnel.open failed", "err", err)
		fallBack()
		return
	}
	n.mu.Lock()
	if n.closed || n.offReason() != "" {
		n.mu.Unlock()
		f.Close()
		return
	}
	mac, _ := netip.ParseAddr(tunnel.Mac)
	peer, _ := netip.ParseAddr(tunnel.Peer)
	ends := TunnelEnds{Interface: tunnel.Interface, Mac: mac, Peer: peer, MTU: tunnel.MTU}
	if m6, err := netip.ParseAddr(tunnel.Mac6); err == nil && ipv6 && m6 == p6.TunnelMac6() {
		ends.Mac6, ends.Peer6, ends.Mac46 = m6, p6.TunnelPeer6(), plan.TunnelMac46()
	}
	old, oldVia := n.tun, n.via
	n.tun, n.iface, n.reason, n.failure, n.base, n.mac6 = f, tunnel.Interface, "", "", plan.Base().String(), ends.Mac6
	if oldVia == api.ViaVmnet {
		n.via, n.noHelper, n.routes = "", "", nil
	}
	n.mu.Unlock()
	if oldVia == api.ViaVmnet {
		log.Info("direct access: the admin helper takes over from the no-admin path")
		if n.Tunnel == nil {
			old.Close()
		}
		defer n.NoAdmin.Close()
	}
	if n.Tunnel != nil {
		if ends.MTU < priv.MinMTU || ends.MTU > priv.MaxMTU {
			ends.MTU = mtu
		}
		n.Tunnel.Attach(f, ends)
	}
	log.Info("direct access: tunnel open", "interface", tunnel.Interface, "mac", tunnel.Mac, "peer", tunnel.Peer, "mac6", tunnel.Mac6, "mtu", tunnel.MTU)
	if n.Tunnel6 != nil {
		n.Tunnel6(ends.Mac6.IsValid())
	}
	n.reroute("the tunnel opened")
}

func (n *Network) link(hello *priv.HelloResult) {
	if !hello.DockerSockLink || hello.DockerSock == priv.StateFev {
		return
	}
	log := n.host.Log()
	var st priv.DockerSock
	if _, err := n.call(priv.MethodDockerSockLink, priv.Empty{}, &st); err != nil {
		log.Info("docker.sock: the admin helper did not make the link", "err", err)
	} else {
		log.Info("docker.sock: linked by the admin helper", "state", st.State, "target", st.Target)
	}
}

func (n *Network) linkAlone() {
	n.mu.Lock()
	closed := n.closed
	n.mu.Unlock()
	if closed {
		return
	}
	var hello priv.HelloResult
	if _, err := n.call(priv.MethodHello, priv.Empty{}, &hello); err != nil {
		n.host.Log().Info("docker.sock: no admin helper to make the link", "err", err)
		return
	}
	if hello.ProtocolVersion != priv.ProtocolVersion {
		return
	}
	n.link(&hello)
}

func (n *Network) reroute(why string) {
	n.rerouting.Lock()
	defer n.rerouting.Unlock()
	n.mu.Lock()
	iface, open, base, mac6, via := n.iface, n.tun != nil, n.base, n.mac6, n.via
	nat64OK := n.hello != nil && n.hello.RoutesNAT64
	n.mu.Unlock()
	plan := planInEffect(n.cfg)
	if open && base != plan.Base().String() {
		n.publish()
		return
	}
	ours := plan.Routed()
	if n.Extra != nil {
		ours = withExtra(plan, ours, n.Extra())
	}
	if p6, ok := plan6InEffect(n.cfg); ok && mac6.IsValid() {
		ours = append(ours, p6.Routed6()...)
	}
	keep, conflicts, warnings := ours, []api.RouteConflict(nil), []string(nil)
	if n.Mac != nil {
		keep, conflicts, warnings = n.Mac.Check(ours, iface)
	}
	var mapped, nat []netip.Prefix
	if mac6.IsValid() && nat64OK {
		for _, pref := range n.macNAT64() {
			nat = append(nat, pref)
			for _, p := range keep {
				if p.Addr().Is4() {
					mapped = append(mapped, netplan.NAT64Prefix(pref, p))
				}
			}
		}
		if len(mapped) > 0 && n.Mac != nil {
			var c2 []api.RouteConflict
			var w2 []string
			mapped, c2, w2 = n.Mac.Check(mapped, iface)
			conflicts, warnings = append(conflicts, c2...), append(warnings, w2...)
		}
	}
	n.warnConflicts(conflicts, warnings)
	if !open {
		n.publishWith(conflicts)
		return
	}
	n.mu.Lock()
	same := slices.Equal(n.routes, append(slices.Clone(keep), mapped...))
	n.mu.Unlock()
	if !same && via == api.ViaVmnet {
		n.NoAdmin.SetPrefixes(keep)
		n.mu.Lock()
		n.routes = keep
		n.mu.Unlock()
		if n.Tunnel != nil {
			n.Tunnel.SetPrefixes(keep)
		}
		n.host.Log().Info("direct access: the no-admin path's addresses set", "why", why, "routes", keep)
		n.publishWith(conflicts)
		return
	}
	if !same {
		r, err := n.routesSet(iface, append(slices.Clone(keep), mapped...))
		if err != nil && len(mapped) > 0 {
			n.host.Log().Warn("direct access: routes.set refused the NAT64 routes; set without them", "why", why, "err", err, "nat64", nat)
			mapped, nat = nil, nil
			r, err = n.routesSet(iface, keep)
		}
		if err != nil {
			n.mu.Lock()
			n.failure = "the admin helper did not set the tunnel's routes: " + err.Error()
			n.mu.Unlock()
			n.host.Log().Warn("direct access: routes.set failed", "why", why, "err", err)
			n.publishWith(conflicts)
			return
		}
		if len(mapped) == 0 {
			nat = nil
		}
		n.mu.Lock()
		n.routes, n.nat64, n.failure = append(slices.Clone(keep), mapped...), nat, ""
		n.mu.Unlock()
		if n.Tunnel != nil {
			n.Tunnel.SetPrefixes(keep)
			n.Tunnel.SetNAT64(nat)
		}
		n.host.Log().Info("direct access: routes set", "why", why, "routes", r.Prefixes, "added", r.Added, "removed", r.Removed, "nat64", nat)
		if n.Mac != nil {
			go n.Mac.Refresh("routes.set")
		}
	}
	n.publishWith(conflicts)
}

func (n *Network) routesSet(iface string, ps []netip.Prefix) (priv.Routes, error) {
	prefixes := make([]string, len(ps))
	for i, p := range ps {
		prefixes[i] = p.String()
	}
	var r priv.Routes
	_, err := n.call(priv.MethodRoutesSet, priv.RoutesParams{Interface: iface, Prefixes: prefixes}, &r)
	return r, err
}

func (n *Network) macNAT64() []netip.Prefix {
	if n.NAT64 != nil {
		return n.NAT64()
	}
	if n.Mac != nil {
		return n.Mac.NAT64()
	}
	return nil
}

func (n *Network) closeTunnel(reason string) {
	n.mu.Lock()
	f, via := n.tun, n.via
	n.tun, n.iface, n.routes, n.reason, n.base, n.mac6, n.nat64, n.via, n.noHelper = nil, "", nil, reason, "", netip.Addr{}, nil, "", ""
	n.mu.Unlock()
	if f != nil {
		n.release(f, via)
		n.host.Log().Info("direct access: tunnel closed", "reason", reason)
		if n.Tunnel6 != nil {
			n.Tunnel6(false)
		}
	}
	n.publish()
}

func (n *Network) release(f *os.File, via string) {
	if n.Tunnel != nil {
		n.Tunnel.Detach()
	} else {
		f.Close()
	}
	if via == api.ViaVmnet {
		n.NoAdmin.Close()
	}
}

func (n *Network) warnConflicts(conflicts []api.RouteConflict, warnings []string) {
	now := map[string]string{}
	for i, c := range conflicts {
		text := ""
		if i < len(warnings) {
			text = warnings[i]
		}
		if text == "" {
			text = fmt.Sprintf("a route of this Mac's (%s on %s) meets fev's %s", c.Route, c.Interface, c.Prefix)
		}
		now[warnConflict+c.Prefix+" "+c.Route+" "+c.Interface] = text
	}
	n.mu.Lock()
	old := n.warned
	n.warned = map[string]bool{}
	for k := range now {
		n.warned[k] = true
	}
	n.mu.Unlock()
	for k := range old {
		if _, ok := now[k]; !ok {
			n.host.Warn(k, "")
		}
	}
	keys := make([]string, 0, len(now))
	for k := range now {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n.host.Warn(k, now[k])
	}
}

func (n *Network) publish() {
	n.publishWith(nil)
}

func (n *Network) publishWith(conflicts []api.RouteConflict) {
	plan := PlanOf(n.cfg)
	n.mu.Lock()
	block := &api.NetworkStatus{Subnet: plan.Base().String(), Subnet6: n.cfg.String(api.KeyNetworkSubnet6)}
	if n.hello != nil {
		block.Helper = &api.HelperStatus{ProtocolVersion: n.hello.ProtocolVersion, SecurityLevel: n.hello.SecurityLevel, Build: n.hello.Build,
			Outdated: n.hello.SecurityLevel < priv.SecurityLevel}
	}
	switch {
	case n.tun != nil:
		block.DirectAccess, block.Interface, block.Via = api.DirectOn, n.iface, n.via
		for _, p := range n.routes {
			block.Routes = append(block.Routes, p.String())
		}
		if n.mac6.IsValid() {
			block.Address6 = n.mac6.String()
		}
		for _, p := range n.nat64 {
			block.NAT64 = append(block.NAT64, p.String())
		}
		block.Conflicts = conflicts
		for _, c := range conflicts {
			if c.Withdrawn {
				block.DirectAccess = api.DirectDegraded
			}
		}
	default:
		block.DirectAccess, block.Reason = api.DirectOff, n.reason
		if block.Reason == "" {
			block.Reason = api.DirectFailed
		}
		block.Conflicts = conflicts
	}
	if n.via == api.ViaVmnet || n.reason == api.DirectHelperHeld {
		block.NoHelper = n.noHelper
	}
	warning := ""
	switch {
	case n.failure != "":
		warning = n.failure
	case n.tun == nil && n.reason == api.DirectOutdated:
		warning = `admin helper out of date: run "sudo fev setup --admin"`
	case n.via == api.ViaVmnet && n.noHelper == api.DirectOutdated:
		warning = `admin helper out of date: direct access goes without it, and low ports and the docker.sock link wait for "sudo fev setup --admin"`
	case n.tun == nil && n.reason == api.DirectHelperOff:
		warning = "fev's admin helper is switched off (System Settings, General, Login Items): direct access and names on this Mac are off until it is on"
	case block.Helper != nil && block.Helper.Outdated:
		warning = `fev's admin helper lacks a fix this build has: run "sudo fev setup --admin"`
	}
	n.mu.Unlock()
	n.host.Warn(WarnHelper, warning)
	updateStatus(n.host, func(st *api.Status) { st.Network = block })
}

func (n *Network) Reroute(why string) {
	n.mu.Lock()
	open := n.tun != nil
	n.mu.Unlock()
	if open {
		go n.reroute(why)
	}
}

func (n *Network) HelperCall(method string, params, result any) error {
	n.mu.Lock()
	ok := n.hello != nil && n.hello.ProtocolVersion == priv.ProtocolVersion && !n.closed && n.offReason() == ""
	n.mu.Unlock()
	if !ok {
		return errNoHelper
	}
	_, err := n.call(method, params, result)
	return err
}

var errNoHelper = errors.New("no admin helper to use")

func withExtra(plan netplan.Plan, ours, extra []netip.Prefix) []netip.Prefix {
	out := slices.Clone(ours)
	var more []netip.Prefix
	for _, p := range extra {
		p = p.Masked()
		if !p.Addr().Is4() || !plan.Base().Contains(p.Addr()) || p.Bits() < plan.Base().Bits() ||
			p.Overlaps(plan.Uplink()) || p.Overlaps(plan.Tunnel()) {
			continue
		}
		covered := false
		for _, o := range append(out, more...) {
			if o.Bits() <= p.Bits() && o.Contains(p.Addr()) {
				covered = true
				break
			}
		}
		if !covered {
			more = append(more, p)
		}
	}
	slices.SortFunc(more, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
	return append(out, more...)
}

func (n *Network) directRoutes() []netip.Prefix {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.routes)
}
