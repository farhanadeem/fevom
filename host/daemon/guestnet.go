package daemon

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"

	"fev/host/daemon/config"
	"fev/host/netstack"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/netplan"
)

type GuestNet struct {
	Mac interface {
		GlobalIPv6() bool
		Addresses() netstack.MacAddresses
		Own() ([]MacAddress, bool)
	}
	Zone netstack.Zone
	Log  *slog.Logger

	cfg  *config.Store
	host Host

	macSet     func(ctx context.Context, c *agent.Client, addresses []string) error
	macRetry   time.Duration
	interfaces func() ([]MacAddress, error)

	mu            sync.Mutex
	stack         *netstack.Stack
	tunnel        *netstack.Tunnel
	iface         string
	prefixes      []netip.Prefix
	nat64         []netip.Prefix
	boot          string
	client        *agent.Client
	macSending    bool
	macAgain      bool
	toldBoot      string
	told          chan struct{}
	macTimer      *time.Timer
	macTimerSpent bool
}

const macRetryAfter = time.Second

type MacAddress struct {
	Addr      netip.Addr
	Interface string
}

func (g *GuestNet) Name() string    { return "guest network" }
func (g *GuestNet) Marks() []string { return nil }
func (g *GuestNet) Close()          {}

func (g *GuestNet) StateChanged(st api.Status) {
	g.mu.Lock()
	if st.State == api.StateStopped || (g.boot != "" && st.BootID != g.boot) {
		g.boot, g.client = "", nil
		if g.macTimer != nil {
			g.macTimer.Stop()
			g.macTimer = nil
		}
	}
	first := g.boot == "" && st.BootID != ""
	host := g.host
	g.mu.Unlock()
	ready := st.State == api.StateRunning || (st.State == api.StateStarting && st.Phase == api.PhaseEngine)
	if !ready || !first || host == nil {
		return
	}
	vm, ok := host.VM()
	if !ok || vm.BootID() != st.BootID || vm.Agent() == nil {
		return
	}
	g.mu.Lock()
	g.boot, g.client = st.BootID, vm.Agent()
	g.toldLocked(st.BootID)
	g.mu.Unlock()
	g.sendMac(false)
}

func (g *GuestNet) MacTold(boot string) <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.toldLocked(boot)
}

const MacToldWait = 5 * time.Second

func (g *GuestNet) WaitMacTold(boot string, bound time.Duration) bool {
	told := g.MacTold(boot)
	select {
	case <-told:
		return true
	default:
	}
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case <-told:
		return true
	case <-t.C:
		g.log().Warn("machines: the machines that ran at the last stop are started before the guest was told the Mac's addresses; an isolated one waits for them in init", "waited", bound)
		return false
	}
}

func (g *GuestNet) toldLocked(boot string) chan struct{} {
	if g.toldBoot != boot || g.told == nil {
		g.toldBoot, g.told = boot, make(chan struct{})
	}
	return g.told
}

func (g *GuestNet) toldDone(boot string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.toldBoot != boot || g.told == nil {
		return
	}
	select {
	case <-g.told:
	default:
		close(g.told)
	}
}

func (g *GuestNet) Start(h Host) error {
	g.mu.Lock()
	g.host = h
	g.cfg = h.Config()
	if g.Log == nil {
		g.Log = h.Log()
	}
	g.mu.Unlock()
	return nil
}

func (g *GuestNet) ConfigChanged(ch api.ConfigChange) {
	if ch.Key != api.KeyNetworkProxy && ch.Key != api.KeyNetworkProxyExclude {
		return
	}
	g.mu.Lock()
	s := g.stack
	g.mu.Unlock()
	if s != nil {
		s.InvalidateProxyDecisions()
	}
}

func (g *GuestNet) Options(o *netstack.Options) {
	g.mu.Lock()
	cfg := g.cfg
	g.mu.Unlock()
	if cfg == nil {
		return
	}
	c := netstack.ConfigOf(planInEffect(cfg))
	if p6, ok := plan6InEffect(cfg); ok {
		c = c.WithIPv6(p6)
	}
	o.Config = c
	o.Proxy = &netstack.ProxyOptions{
		Setting:     func() string { return cfg.String(api.KeyNetworkProxy) },
		Exclude:     func() []string { return cfg.List(api.KeyNetworkProxyExclude) },
		Transparent: func() bool { return cfg.Bool(api.KeyNetworkProxyTransparent) },
	}
	if g.Mac != nil {
		mac := g.Mac
		o.IPv6Usable = mac.GlobalIPv6
		if l, ok := mac.(interface{ LANs() []netip.Prefix }); ok {
			o.Proxy.LANs = l.LANs
		}
	}
	if g.Zone != nil {
		o.Zone = g.Zone
	}
}

func plan6InEffect(cfg *config.Store) (netplan.Plan6, bool) {
	s := cfg.String(api.KeyNetworkSubnet6)
	if v, ok := cfg.InEffect(api.KeyNetworkSubnet6); ok {
		if v, ok := v.(string); ok {
			s = v
		}
	}
	p6, err := netplan.Parse6(s)
	return p6, err == nil
}

func (g *GuestNet) Stack(s *netstack.Stack) {
	g.mu.Lock()
	g.stack = s
	t := g.tunnel
	g.mu.Unlock()
	if t != nil {
		t.Attach(s)
	}
	go func() {
		<-s.Done()
		g.mu.Lock()
		if g.stack == s {
			g.stack = nil
		}
		g.mu.Unlock()
	}()
}

func (g *GuestNet) Attach(f *os.File, e TunnelEnds) {
	iface := e.Interface
	fd, err := dupFile(f)
	f.Close()
	if err != nil {
		g.log().Error("direct access: the tunnel's descriptor cannot be taken over", "interface", iface, "err", err)
		return
	}
	cfg := netstack.TunnelConfig{Mac: e.Mac, Peer: e.Peer, Mac6: e.Mac6, Peer6: e.Peer6, Mac46: e.Mac46, MTU: e.MTU}
	t, err := netstack.NewTunnel(fd, cfg, g.log())
	if err != nil {
		g.log().Error("direct access: the splice did not start", "interface", iface, "err", err)
		return
	}
	g.mu.Lock()
	old := g.tunnel
	g.tunnel, g.iface = t, iface
	s := g.stack
	prefixes, nat64 := g.prefixes, g.nat64
	g.mu.Unlock()
	if old != nil {
		g.closeTunnel(old)
	}
	rcv, snd := t.Buffers()
	g.log().Info("direct access: the splice started", "interface", iface, "mtu", cfg.MTU, "rcvbuf", rcv, "sndbuf", snd, "mac6", cfg.Mac6)
	t.SetPrefixes(prefixes)
	t.SetNAT64(nat64)
	if s != nil {
		t.Attach(s)
	}
}

func (g *GuestNet) SetPrefixes(ps []netip.Prefix) {
	g.mu.Lock()
	g.prefixes = append([]netip.Prefix(nil), ps...)
	t := g.tunnel
	g.mu.Unlock()
	if t != nil {
		t.SetPrefixes(ps)
	}
}

func (g *GuestNet) SetNAT64(ps []netip.Prefix) {
	g.mu.Lock()
	g.nat64 = append([]netip.Prefix(nil), ps...)
	t := g.tunnel
	g.mu.Unlock()
	if t != nil {
		t.SetNAT64(ps)
	}
}

func (g *GuestNet) Detach() {
	g.mu.Lock()
	t := g.tunnel
	g.tunnel, g.prefixes, g.nat64, g.iface = nil, nil, nil, ""
	g.mu.Unlock()
	if t != nil {
		g.closeTunnel(t)
	}
}

func (g *GuestNet) closeTunnel(t *netstack.Tunnel) {
	t.Close()
	g.log().Info("direct access: the splice's count", "count", t.Stats().String())
}

func (g *GuestNet) TunnelStats() (netstack.TunnelStats, bool) {
	g.mu.Lock()
	t := g.tunnel
	g.mu.Unlock()
	if t == nil {
		return netstack.TunnelStats{}, false
	}
	return t.Stats(), true
}

func (g *GuestNet) MacChanged() {
	g.sendMac(false)
	g.mu.Lock()
	s := g.stack
	g.mu.Unlock()
	if s == nil {
		return
	}
	if g.Mac != nil {
		if n := s.MacAddressesChanged(g.Mac.Addresses()); n > 0 {
			g.log().Info("network: UDP flows ended whose local address the Mac no longer has", "flows", n)
		}
	}
	s.InvalidateProxyDecisions()
}

func (g *GuestNet) macList() ([]string, bool) {
	var own []MacAddress
	if g.Mac == nil {
		list := g.interfaces
		if list == nil {
			list = macSnapshot
		}
		var err error
		if own, err = list(); err != nil {
			g.log().Warn("isolated machines: the Mac's addresses cannot be listed", "err", err)
			return nil, false
		}
	} else {
		var ok bool
		if own, ok = g.Mac.Own(); !ok {
			return nil, false
		}
	}
	g.mu.Lock()
	tunnel := g.iface
	g.mu.Unlock()
	var v4, v6 []string
	for _, a := range own {
		ip := a.Addr.Unmap()
		if !ip.IsValid() || ip.IsLoopback() || ip.IsMulticast() || ip.IsUnspecified() || a.Interface == "lo0" || (tunnel != "" && a.Interface == tunnel) {
			continue
		}
		if ip.Is4() {
			v4 = append(v4, ip.String())
		} else {
			v6 = append(v6, ip.String())
		}
	}
	list := slices.Compact(append(sortedStrings(v4), sortedStrings(v6)...))
	if len(list) > agent.MaxMacAddresses {
		g.log().Warn("isolated machines: the Mac has more addresses than the guest is told; the first are sent", "addresses", len(list), "sent", agent.MaxMacAddresses)
		list = list[:agent.MaxMacAddresses]
	}
	return list, true
}

func sortedStrings(s []string) []string {
	slices.Sort(s)
	return slices.Compact(s)
}

func (g *GuestNet) sendMac(byTimer bool) {
	g.mu.Lock()
	if !byTimer {
		g.macTimerSpent = false
	}
	if g.client == nil {
		g.mu.Unlock()
		return
	}
	if g.macSending {
		g.macAgain = true
		g.mu.Unlock()
		return
	}
	g.macSending = true
	g.mu.Unlock()
	go func() {
		for {
			g.mu.Lock()
			client, boot := g.client, g.boot
			g.macAgain = false
			g.mu.Unlock()
			if list, ok := g.macList(); ok && client != nil {
				send := g.macSet
				if send == nil {
					send = func(ctx context.Context, c *agent.Client, a []string) error { return c.MacAddressesSet(ctx, a) }
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				err := send(ctx, client, list)
				cancel()
				if err != nil {
					g.macFailed(boot, err)
				} else {
					g.log().Debug("isolated machines: the guest was told the Mac's addresses", "addresses", len(list))
					g.mu.Lock()
					if g.macTimer != nil {
						g.macTimer.Stop()
						g.macTimer = nil
					}
					g.mu.Unlock()
					g.toldDone(boot)
				}
			}
			g.mu.Lock()
			if !g.macAgain || g.client == nil {
				g.macSending = false
				g.mu.Unlock()
				return
			}
			g.mu.Unlock()
		}
	}()
}

func (g *GuestNet) macFailed(boot string, err error) {
	g.mu.Lock()
	retry := !g.macTimerSpent && g.boot == boot
	if retry {
		g.macTimerSpent = true
		after := g.macRetry
		if after == 0 {
			after = macRetryAfter
		}
		if g.macTimer != nil {
			g.macTimer.Stop()
		}
		g.macTimer = time.AfterFunc(after, func() { g.sendMac(true) })
	}
	g.mu.Unlock()
	if retry {
		g.log().Warn("isolated machines: the guest was not told the Mac's addresses; sent again in a second", "err", err)
		return
	}
	g.log().Warn("isolated machines: the guest was not told the Mac's addresses; sent again at the next change of the Mac's network or the next boot", "err", err)
	g.toldDone(boot)
}

func macSnapshot() ([]MacAddress, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []MacAddress
	for _, ifc := range ifs {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			out = append(out, MacAddress{Addr: ip.Unmap(), Interface: ifc.Name})
		}
	}
	return out, nil
}

func (g *GuestNet) log() *slog.Logger {
	if g.Log != nil {
		return g.Log
	}
	return slog.Default()
}

func dupFile(f *os.File) (int, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return -1, err
	}
	fd, derr := -1, error(nil)
	if err := raw.Control(func(d uintptr) {
		syscall.ForkLock.RLock()
		fd, derr = syscall.Dup(int(d))
		if derr == nil {
			syscall.CloseOnExec(fd)
		}
		syscall.ForkLock.RUnlock()
	}); err != nil {
		return -1, err
	}
	return fd, derr
}
