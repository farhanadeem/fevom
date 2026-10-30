package machnet

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"fev/guest/internal/fw"
	"fev/guest/internal/listen"
	"fev/guest/internal/logbuf"
	"fev/guest/internal/reaper"
	"fev/guest/internal/streams"
	"fev/guest/machines"
	"fev/proto/relay"
)

type Options struct {
	Prefix       netip.Prefix
	Prefix6      netip.Prefix
	RefreshAfter time.Duration
	Upstream     netip.AddrPort
	ForwardLocal bool
	FirewallRoot string
	Reaper       *reaper.Reaper
	DialEngine   func() (net.Conn, error)
	Log          *logbuf.Buffer
	Requests     *streams.Requests
	Links        string
	Members      func(gone netip.Addr)
	Uplink       func() string
	Base         netip.Prefix
}

type Network struct {
	o            Options
	prefix       netip.Prefix
	gateway      netip.Addr
	prefix6      netip.Prefix
	gateway6     netip.Addr
	rs           *net.IPConn
	ra           []byte
	refreshAfter time.Duration
	lan          lan
	res          resolver

	setupMu sync.Mutex
	up      atomic.Bool
	bridge  netlink.Link

	mu     sync.Mutex
	byMAC  map[string]*member
	byName map[string]*member
	last   map[string]netip.Addr

	ports     *ports
	listeners func(netns uint64) []listen.Listener

	isoMu        sync.Mutex
	mac          []netip.Addr
	isolated     map[string]netip.Addr
	macTold      chan struct{}
	macToldOnce  sync.Once
	macToldBound time.Duration
}

const MacToldBound = 30 * time.Second

type member struct {
	name, key     string
	mac           net.HardwareAddr
	addr          netip.Addr
	addr6         netip.Addr
	ll6           netip.Addr
	lastRA        time.Time
	leased        chan struct{}
	leaseOnce     sync.Once
	docker        *dockerSocket
	bridge, agent *hostSocket
	nsfd          int
}

func New(o Options) *Network {
	prefix := o.Prefix
	if !prefix.IsValid() {
		prefix = Prefix
	}
	n := &Network{
		o: o, prefix: prefix.Masked(), gateway: Gateway(prefix),
		byMAC: map[string]*member{}, byName: map[string]*member{}, last: map[string]netip.Addr{},
		ports: newPorts(), isolated: map[string]netip.Addr{}, macTold: make(chan struct{}), macToldBound: MacToldBound,
	}
	if o.Prefix6.IsValid() && o.Prefix6.Addr().Is6() && o.Prefix6.Bits() == 64 {
		n.prefix6 = o.Prefix6.Masked()
		n.gateway6 = Gateway6(n.prefix6)
		n.ra = routerAdvertisement(bridgeMAC, n.prefix6)
	}
	n.refreshAfter = o.RefreshAfter
	if n.refreshAfter <= 0 {
		n.refreshAfter = RefreshAfter
	}
	n.lan = lan{server: n.gateway, prefix: n.prefix, lease: LeaseSeconds}
	n.res = resolver{gateway: n.gateway, lookup: n.lookup, lookup6: n.lookup6, forwardLocal: o.ForwardLocal}
	return n
}

func (n *Network) logf(level int, format string, args ...any) {
	if n.o.Log != nil {
		n.o.Log.Logf(level, "machines' network: "+format, args...)
	}
}

func (n *Network) lookup(name string) (netip.Addr, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	m := n.byName[relay.MachineNameKey(name)]
	if m == nil {
		return netip.Addr{}, false
	}
	return m.addr, true
}

func (n *Network) lookup6(name string) netip.Addr {
	n.mu.Lock()
	defer n.mu.Unlock()
	if m := n.byName[relay.MachineNameKey(name)]; m != nil {
		return m.addr6
	}
	return netip.Addr{}
}

func (n *Network) Plug(p machines.Plug) (machines.Plugged, error) {
	if p.IsolateNetwork {
		if err := n.waitMacTold(); err != nil {
			return machines.Plugged{}, err
		}
	}
	if err := n.setup(); err != nil {
		return machines.Plugged{}, err
	}
	id := p.UUID
	if id == "" {
		id = p.Name
	}
	mac := MACFor(id)
	if p.MAC != "" {
		hw, err := net.ParseMAC(p.MAC)
		if err != nil || len(hw) != 6 {
			return machines.Plugged{}, fmt.Errorf("the machine's MAC %q is not an ethernet address", p.MAC)
		}
		mac = hw
	}
	n.mu.Lock()
	taken := map[netip.Addr]string{}
	for _, m := range n.byName {
		taken[m.addr] = m.name
	}
	if other := n.byMAC[mac.String()]; other != nil && other.name != p.Name {
		n.mu.Unlock()
		return machines.Plugged{}, fmt.Errorf("the MAC %s is %s's", mac, other.name)
	}
	addr, err := allocate(n.prefix, taken, p.Name, id, p.Address, n.last[p.Name])
	if err != nil {
		n.mu.Unlock()
		return machines.Plugged{}, err
	}
	m := &member{name: p.Name, key: relay.MachineNameKey(p.Name), mac: mac, addr: addr, leased: make(chan struct{}), nsfd: -1}
	if fd, err := unix.FcntlInt(uintptr(p.NetNS), unix.F_DUPFD_CLOEXEC, 0); err == nil {
		m.nsfd = fd
	}
	if old := n.byName[m.key]; old != nil {
		delete(n.byMAC, old.mac.String())
	}
	n.byName[m.key], n.byMAC[mac.String()], n.last[p.Name] = m, m, addr
	n.mu.Unlock()

	unveth, err := machines.Veth(p.Name, mac.String(), p.NetNS, n.bridge)
	if err != nil {
		n.forget(m)
		m.closeNS()
		return machines.Plugged{}, err
	}
	veth := machines.HostLink(p.Name)
	if p.IsolateNetwork {
		if err := n.isolate(veth, addr, true); err != nil {
			unveth()
			n.forget(m)
			m.closeNS()
			return machines.Plugged{}, fmt.Errorf("isolate_network: %w", err)
		}
	}
	netns, err := listen.NetNSCookie(p.NetNS)
	if err != nil {
		n.logf(logbuf.Warning, "%s: its ports are not followed: %v", p.Name, err)
	} else {
		n.mu.Lock()
		seed := n.listeners
		n.mu.Unlock()
		n.ports.add(m.name, addr, netns, seed)
	}
	if p.Run != "" && n.o.DialEngine != nil {
		ds, err := listenDocker(p, n.o.DialEngine, n.logf)
		if err != nil {
			n.logf(logbuf.Warning, "%s: no engine socket: %v", p.Name, err)
		}
		n.mu.Lock()
		m.docker = ds
		n.mu.Unlock()
	}
	bridgeSocket, agentSocket := n.hostSockets(p)
	n.mu.Lock()
	m.bridge, m.agent = bridgeSocket, agentSocket
	n.mu.Unlock()
	stop6 := func() {}
	if n.prefix6.IsValid() && !p.IsolateNetwork {
		stop, err := n.followAddrs6(p.NetNS, func(a, ll netip.Addr) {
			n.mu.Lock()
			moved := m.addr6 != a
			m.addr6 = a
			if ll.IsValid() {
				m.ll6 = ll
			}
			n.mu.Unlock()
			if !moved {
				return
			}
			n.logf(logbuf.Info, "%s has %s", m.name, describe6(a))
			if p.Address6Changed != nil {
				p.Address6Changed()
			}
		})
		if err != nil {
			n.logf(logbuf.Warning, "%s: its IPv6 address is not followed: %v", p.Name, err)
		} else {
			stop6 = stop
		}
	}
	n.members(netip.Addr{})
	return machines.Plugged{
		MAC: mac.String(), Address: addr.String(), Leased: m.leased,
		Address6: func() string {
			n.mu.Lock()
			defer n.mu.Unlock()
			if !m.addr6.IsValid() {
				return ""
			}
			return m.addr6.String()
		},
		Unplug: func() {
			stop6()
			if netns != 0 {
				n.ports.remove(m.name, netns)
			}
			if m.docker != nil {
				m.docker.close()
			}
			m.bridge.close()
			m.agent.close()
			unveth()
			if p.IsolateNetwork {
				if err := n.isolate(veth, addr, false); err != nil {
					n.logf(logbuf.Warning, "%s: isolate_network's rules: %v", p.Name, err)
				}
			}
			n.forget(m)
			m.closeNS()
			n.members(addr)
		},
	}, nil
}

func (m *member) closeNS() {
	if m.nsfd >= 0 {
		unix.Close(m.nsfd)
		m.nsfd = -1
	}
}

func (n *Network) members(gone netip.Addr) {
	if n.o.Members != nil {
		n.o.Members(gone)
	}
}

func (n *Network) NetNSPaths() map[string]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make(map[string]string, len(n.byName))
	for _, m := range n.byName {
		if m.nsfd >= 0 {
			out[m.name] = fmt.Sprintf("/proc/1/fd/%d", m.nsfd)
		}
	}
	return out
}

func (n *Network) forget(m *member) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.byName[m.key] == m {
		delete(n.byName, m.key)
	}
	if n.byMAC[m.mac.String()] == m {
		delete(n.byMAC, m.mac.String())
	}
}

func (n *Network) Leased(name string) <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if m := n.byName[relay.MachineNameKey(name)]; m != nil {
		return m.leased
	}
	return nil
}

func (n *Network) Rename(old, new string) {
	n.mu.Lock()
	o, nw := relay.MachineNameKey(old), relay.MachineNameKey(new)
	if m := n.byName[o]; m != nil {
		delete(n.byName, o)
		m.name, m.key = new, nw
		n.byName[nw] = m
		if m.docker != nil {
			m.docker.renamed(new)
		}
		m.bridge.renamed(new)
		m.agent.renamed(new)
	}
	if a, ok := n.last[old]; ok {
		delete(n.last, old)
		n.last[new] = a
	}
	n.mu.Unlock()
	n.ports.rename(old, new)
	n.members(netip.Addr{})
}

func (n *Network) setup() error {
	if n.up.Load() {
		return nil
	}
	n.setupMu.Lock()
	defer n.setupMu.Unlock()
	if n.up.Load() {
		return nil
	}
	br, err := makeBridge(BridgeName, n.gateway, n.prefix, n.prefix6.IsValid())
	if err != nil {
		return fmt.Errorf("the machines' bridge: %w", err)
	}
	var rs *net.IPConn
	if n.prefix6.IsValid() {
		if err := address6(br, n.gateway6, n.prefix6); err != nil {
			n.logf(logbuf.Warning, "no IPv6 for machines: the bridge's %s: %v", n.gateway6, err)
		} else if rs, err = listenRS(BridgeName, br.Attrs().Index); err != nil {
			n.logf(logbuf.Warning, "no IPv6 for machines: router solicitations cannot be taken: %v", err)
			rs = nil
		}
	}
	dhcp, err := listenDHCP(BridgeName)
	if err != nil {
		return fmt.Errorf("the DHCP server: %w", err)
	}
	udp, tcp, err := listenDNS(n.gateway)
	if err != nil {
		dhcp.Close()
		return fmt.Errorf("the resolver: %w", err)
	}
	n.bridge = br
	if rs != nil {
		n.rs = rs
		go n.serveRS(rs)
	}
	go n.serveDHCP(dhcp)
	go n.serveDNS(udp)
	go n.serveDNSTCP(tcp)
	n.up.Store(true)
	n.applyRules("the bridge was made")
	return nil
}

func makeBridge(name string, gateway netip.Addr, prefix netip.Prefix, ipv6 bool) (netlink.Link, error) {
	br, err := netlink.LinkByName(name)
	if err != nil {
		attrs := netlink.NewLinkAttrs()
		attrs.Name, attrs.HardwareAddr = name, bridgeMAC
		if err := netlink.LinkAdd(&netlink.Bridge{LinkAttrs: attrs}); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, err
		}
		if br, err = netlink.LinkByName(name); err != nil {
			return nil, err
		}
	}
	addr, err := netlink.ParseAddr(netip.PrefixFrom(gateway, prefix.Bits()).String())
	if err != nil {
		return nil, err
	}
	if err := netlink.AddrReplace(br, addr); err != nil {
		return nil, err
	}
	if ipv6 {
		if err := bridgeQuiet6(name); err != nil {
			return nil, err
		}
	}
	if err := netlink.LinkSetUp(br); err != nil {
		return nil, err
	}
	return br, nil
}

func (n *Network) EngineReady() {
	if !n.up.Load() {
		return
	}
	go n.applyRules("the engine is ready")
}

func (n *Network) applyRules(why string) {
	if n.o.FirewallRoot == "" || n.o.Reaper == nil {
		n.logf(logbuf.Warning, "no firewall tool: the machines' traffic is not masqueraded")
		return
	}
	uplink := uplinkName()
	if uplink == "" {
		n.logf(logbuf.Warning, "no default route: the machines' traffic is not masqueraded")
	}
	tool := fw.Tool{Root: n.o.FirewallRoot, Reaper: n.o.Reaper}
	if err := tool.Apply(func(now fw.Tables) string {
		return restoreScript(now, BridgeName, n.prefix, uplink)
	}); err != nil {
		n.logf(logbuf.Error, "make the rules (%s): %v", why, err)
		return
	}
	if n.prefix6.IsValid() {
		if err := tool.Apply6(func(now fw.Tables) string {
			return restoreScript(now, BridgeName, n.prefix6, uplink)
		}); err != nil {
			n.logf(logbuf.Error, "make the IPv6 rules (%s): %v", why, err)
			return
		}
	}
	n.logf(logbuf.Info, "rules made (%s)", why)
}

func uplinkName() string {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return ""
	}
	for _, r := range routes {
		if r.Dst == nil || r.Dst.IP.IsUnspecified() {
			if l, err := netlink.LinkByIndex(r.LinkIndex); err == nil {
				return l.Attrs().Name
			}
		}
	}
	return ""
}

func (n *Network) isolate(veth string, addr netip.Addr, on bool) error {
	n.isoMu.Lock()
	defer n.isoMu.Unlock()
	if on {
		n.isolated[veth] = addr
	} else {
		delete(n.isolated, veth)
	}
	err := n.applyIsolationLocked()
	if err != nil && on {
		delete(n.isolated, veth)
	}
	return err
}

func (n *Network) SetMacAddresses(addrs []netip.Addr) error {
	n.isoMu.Lock()
	defer n.isoMu.Unlock()
	n.mac = append([]netip.Addr{}, addrs...)
	n.macToldOnce.Do(func() { close(n.macTold) })
	if len(n.isolated) == 0 {
		return nil
	}
	return n.applyIsolationLocked()
}

func (n *Network) waitMacTold() error {
	bound := n.macToldBound
	select {
	case <-n.macTold:
		return nil
	default:
	}
	n.logf(logbuf.Info, "a machine with isolate_network waits for the Mac's addresses (mac_addresses.set)")
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case <-n.macTold:
		return nil
	case <-t.C:
		return fmt.Errorf("isolate_network: the Mac's addresses were not told within %v (mac_addresses.set): not plugged in", bound)
	}
}

func (n *Network) applyIsolationLocked() error {
	uplink := ""
	if n.o.Uplink != nil {
		uplink = n.o.Uplink()
	}
	if uplink == "" && len(n.isolated) > 0 {
		return errors.New("the uplink is not known")
	}
	if n.o.FirewallRoot == "" {
		if len(n.isolated) > 0 {
			return errors.New("no firewall tool")
		}
		return nil
	}
	base := n.o.Base
	if !base.IsValid() {
		base = n.prefix
	}
	iso := fw.Isolation{Uplink: uplink, Gateway: n.gateway, Machines: n.prefix, Base: base, Mac: n.mac}
	for veth, a := range n.isolated {
		iso.Members = append(iso.Members, fw.IsolatedMember{Veth: veth, Address: a})
	}
	tool := fw.Tool{Root: n.o.FirewallRoot, Reaper: n.o.Reaper}
	return tool.Nft(iso.Script())
}
