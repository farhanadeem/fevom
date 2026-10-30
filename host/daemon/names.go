package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"fev/host/daemon/config"
	"fev/host/docker/engine"
	"fev/host/docker/vsock"
	"fev/host/domains"
	"fev/host/trust"
	"fev/proto/agent"
	api "fev/proto/daemon"
	priv "fev/proto/privhelper"
	"fev/proto/relay"
)

type Names struct {
	AddressFile string
	CA          CA
	Network     *Network
	Port        int
	Serve       func(addr netip.AddrPort, r *domains.Registry) (io.Closer, error)
	Offer       func()

	host Host
	cfg  *config.Store
	log  *slog.Logger

	mu          sync.Mutex
	reg         *domains.Registry
	mac6        bool
	zone        io.Closer
	zonePort    int
	resolver    string
	helper      bool
	mdns        string
	boot        string
	vm          VM
	client      *agent.Client
	seq         uint64
	sending     bool
	dirty       bool
	unsent      bool
	sent        []string
	local       map[string]api.LocalNameStatus
	localBusy   bool
	localDirty  bool
	netChanged  bool
	retry       *time.Timer
	localWarned map[string]bool
	closed      bool

	subsMu   sync.Mutex
	subs     map[int]func()
	nextID   int
	shadowed string

	domainsSet func(ctx context.Context, c *agent.Client, p agent.DomainsParams) error

	netsMu sync.Mutex
	nets   map[string][]netip.Prefix

	slots   chan struct{}
	offered bool
}

type CA interface {
	CA() (*trust.CA, error)
	RootPEM() string
}

const (
	WarnNamesZone      = "names.zone"
	warnLocalName      = "names.local "
	WarnNamesCollision = "names.collision"
)

const domainsBound = 30 * time.Second

func (n *Names) Name() string    { return "names" }
func (n *Names) Marks() []string { return nil }

func (n *Names) Start(h Host) error {
	n.host, n.cfg, n.log = h, h.Config(), h.Log()
	n.local, n.localWarned = map[string]api.LocalNameStatus{}, map[string]bool{}
	n.subs = map[int]func(){}
	n.slots = make(chan struct{}, agent.MaxStreams)
	n.mu.Lock()
	n.reg = n.newRegistry()
	n.mu.Unlock()
	if n.Network != nil {
		prev := n.Network.Hello
		n.Network.Hello = func(hr *priv.HelloResult) {
			if prev != nil {
				prev(hr)
			}
			n.hello(hr)
		}
		if n.Network.Extra == nil {
			n.Network.Extra = n.EngineNetworks
		}
		prev6 := n.Network.Tunnel6
		n.Network.Tunnel6 = func(on bool) {
			if prev6 != nil {
				prev6(on)
			}
			n.mu.Lock()
			n.mac6 = on
			r := n.reg
			n.mu.Unlock()
			if r != nil {
				r.SetMac6(on)
			}
		}
		prevMac := n.Network.MacChanged
		n.Network.MacChanged = func() {
			if prevMac != nil {
				prevMac()
			}
			n.macChanged()
		}
	}
	if n.Port != 0 {
		n.serveZone(n.Port)
	}
	n.publish()
	return nil
}

func (n *Names) newRegistry() *domains.Registry {
	p6, has6 := plan6InEffect(n.cfg)
	r := domains.New(domains.Options{
		Plan:        planInEffect(n.cfg),
		Plan6:       p6,
		Has6:        has6,
		AddressFile: n.AddressFile,
		HTTPS:       n.cfg.Bool(api.KeyNetworkHTTPS),
		OnChange:    n.changed,
	})
	r.SetMac6(n.mac6)
	return r
}

func (n *Names) Registry() *domains.Registry {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.reg
}

func (n *Names) Feed() *NamesFeed { return &NamesFeed{n: n} }

type NamesFeed struct{ n *Names }

func (f *NamesFeed) Container(id string, c *engine.Container) {
	if r := f.n.Registry(); r != nil {
		r.Containers().Container(id, c)
	}
	var nets []netip.Prefix
	if c != nil && c.Running {
		for _, nw := range c.Networks {
			if nw.Prefix.IsValid() {
				nets = append(nets, nw.Prefix)
			}
		}
	}
	f.n.setNets(func(m map[string][]netip.Prefix) {
		if len(nets) == 0 {
			delete(m, id)
		} else {
			m[id] = nets
		}
	})
}

func (f *NamesFeed) Synced(running map[string]bool) {
	if r := f.n.Registry(); r != nil {
		r.Synced(running)
	}
	f.n.setNets(func(m map[string][]netip.Prefix) {
		for id := range m {
			if !running[id] {
				delete(m, id)
			}
		}
	})
}

func (f *NamesFeed) Clear() {
	if r := f.n.Registry(); r != nil {
		r.ClearContainers()
	}
	f.n.setNets(func(m map[string][]netip.Prefix) { clear(m) })
}

func (n *Names) setNets(change func(map[string][]netip.Prefix)) {
	before := n.EngineNetworks()
	n.netsMu.Lock()
	if n.nets == nil {
		n.nets = map[string][]netip.Prefix{}
	}
	change(n.nets)
	n.netsMu.Unlock()
	if after := n.EngineNetworks(); !slices.Equal(before, after) && n.Network != nil {
		n.Network.Reroute("the engine's networks changed")
	}
}

func (n *Names) EngineNetworks() []netip.Prefix {
	n.netsMu.Lock()
	defer n.netsMu.Unlock()
	var out []netip.Prefix
	for _, ps := range n.nets {
		for _, p := range ps {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	slices.SortFunc(out, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	return out
}

func (n *Names) Machine(st agent.MachineState) {
	r := n.Registry()
	if r == nil {
		return
	}
	r.Machine(st, n.machinePort(st.Name, api.MachineKeyHTTPPort), n.machinePort(st.Name, api.MachineKeyHTTPSPort))
}

func (n *Names) machinePort(name, short string) int {
	key := config.MachinePrefix + name + "." + short
	got, err := n.cfg.Get(key)
	if err != nil {
		return 0
	}
	v, _ := got[key].Value.(int64)
	return int(v)
}

func (n *Names) MachinesGone() {
	if r := n.Registry(); r != nil {
		r.ClearMachines()
	}
}

func (n *Names) StateChanged(st api.Status) {
	n.mu.Lock()
	cur := n.boot
	n.mu.Unlock()
	if st.State == api.StateStopped || (cur != "" && st.BootID != cur) {
		n.mu.Lock()
		n.boot, n.vm, n.client, n.seq = "", nil, nil, 0
		n.mu.Unlock()
	}
	switch {
	case st.State == api.StateStarting && st.Phase != api.PhaseEngine:
		n.mu.Lock()
		if n.reg != nil {
			n.reg.SetPlan6(plan6InEffect(n.cfg))
		}
		port := 0
		var zone io.Closer
		if n.reg.Plan().Base() != planInEffect(n.cfg).Base() {
			old := n.reg
			n.reg = n.newRegistry()
			n.log.Info("names: the registry follows the boot's address plan", "base", n.reg.Plan().Base(), "was", old.Plan().Base())
			port, zone = n.zonePort, n.zone
			n.zone, n.zonePort = nil, 0
		}
		n.mu.Unlock()
		if zone != nil {
			zone.Close()
			n.serveZone(port)
		}
	case st.State == api.StateRunning || (st.State == api.StateStarting && st.Phase == api.PhaseEngine):
		n.mu.Lock()
		first := n.boot != st.BootID
		again := !first && st.Engine == api.EngineReady && n.unsent
		n.mu.Unlock()
		if first {
			vm, ok := n.host.VM()
			if !ok || vm.BootID() != st.BootID {
				return
			}
			client := vm.Agent()
			client.OnTLSRequest(n.tlsRequests(vm))
			n.mu.Lock()
			n.boot, n.vm, n.client, n.seq, n.unsent = st.BootID, vm, client, 0, false
			n.mu.Unlock()
			n.sendDomains()
		} else if again {
			n.sendDomains()
		}
	}
}

func (n *Names) ConfigChanged(ch api.ConfigChange) {
	switch {
	case ch.Key == api.KeyNetworkHTTPS:
		if r := n.Registry(); r != nil {
			r.SetHTTPS(n.cfg.Bool(api.KeyNetworkHTTPS))
		}
	case strings.HasPrefix(ch.Key, config.MachinePrefix) &&
		(strings.HasSuffix(ch.Key, "."+api.MachineKeyHTTPPort) || strings.HasSuffix(ch.Key, "."+api.MachineKeyHTTPSPort)):
	}
}

func (n *Names) Close() {
	n.mu.Lock()
	n.closed = true
	zone := n.zone
	n.zone = nil
	if n.retry != nil {
		n.retry.Stop()
	}
	sent := len(n.sent) > 0 || n.anyPresentLocked()
	n.mu.Unlock()
	if zone != nil {
		zone.Close()
	}
	if sent && n.Network != nil {
		var r priv.LocalNames
		if err := n.Network.HelperCall(priv.MethodLocalNamesSet, priv.LocalNamesParams{Names: []string{}}, &r); err != nil {
			n.log.Warn("names: the custom names' entries were not removed at exit", "err", err)
		}
	}
}

func (n *Names) anyPresentLocked() bool {
	for _, l := range n.local {
		if l.State == priv.NamePresent {
			return true
		}
	}
	return false
}

func (n *Names) changed() {
	n.sendDomains()
	n.sendLocal(false)
	n.publish()
}

func (n *Names) RootChanged() { n.sendDomains() }

func (n *Names) sendDomains() {
	n.mu.Lock()
	if n.client == nil {
		n.mu.Unlock()
		return
	}
	if n.sending {
		n.dirty = true
		n.mu.Unlock()
		return
	}
	n.sending = true
	n.mu.Unlock()
	go n.domainsLoop()
}

func (n *Names) domainsLoop() {
	for {
		n.mu.Lock()
		client, reg := n.client, n.reg
		if client == nil {
			n.sending, n.dirty = false, false
			n.mu.Unlock()
			return
		}
		n.seq++
		seq := n.seq
		n.dirty = false
		n.mu.Unlock()
		root := ""
		if n.CA != nil && n.cfg.Bool(api.KeyNetworkHTTPS) {
			root = n.CA.RootPEM()
		}
		p := reg.Params(seq, root)
		ctx, cancel := context.WithTimeout(context.Background(), domainsBound)
		send := n.domainsSet
		if send == nil {
			send = func(ctx context.Context, c *agent.Client, p agent.DomainsParams) error {
				_, err := c.DomainsSet(ctx, p)
				return err
			}
		}
		err := send(ctx, client, p)
		cancel()
		if err != nil {
			n.log.Warn("names: init did not take the names' targets", "seq", seq, "targets", len(p.Targets), "err", err)
		} else {
			n.log.Info("names: domains.set", "seq", seq, "targets", len(p.Targets))
		}
		n.mu.Lock()
		n.unsent = err != nil
		if !n.dirty {
			n.sending = false
			n.mu.Unlock()
			return
		}
		n.mu.Unlock()
	}
}

func (n *Names) tlsRequests(vm VM) func(agent.StreamRequest) {
	return func(req agent.StreamRequest) {
		if (relay.Request{Kind: relay.Stream, Target: req.Token}).Validate() != nil {
			return
		}
		select {
		case n.slots <- struct{}{}:
		default:
			go n.refuse(vm, req.Token, "the daemon serves too many leaf requests")
			return
		}
		go func() {
			defer func() { <-n.slots }()
			ctx, cancel := context.WithTimeout(context.Background(), bridgeDialBound)
			conn, err := vsock.Relay{Path: vm.VsockPath(relay.Port)}.Open(ctx, relay.Request{Kind: relay.Stream, Target: req.Token})
			cancel()
			if err != nil {
				n.log.Debug("names: the guest did not take a leaf's stream", "err", err)
				return
			}
			if err := n.issuer().Serve(conn); err != nil {
				n.log.Debug("names: a leaf request was not answered", "err", err)
			}
			n.offerTrust()
		}()
	}
}

func (n *Names) issuer() *domains.Issuer {
	return &domains.Issuer{Registry: n.Registry(), CA: n.signingCA, OnMac: n.onMac}
}

func (n *Names) onMac(entry string) (on, wait bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.helper || n.closed {
		return false, false
	}
	l, ok := n.local[entry]
	if !ok || l.State == api.LocalWaiting {
		return false, true
	}
	return l.State == priv.NamePresent, false
}

func (n *Names) refuse(vm VM, token, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), bridgeDialBound)
	defer cancel()
	if a := vm.Agent(); a != nil {
		a.StreamRefuse(ctx, token, reason)
	}
}

func (n *Names) signingCA() (*trust.CA, error) {
	if !n.cfg.Bool(api.KeyNetworkHTTPS) {
		return nil, errors.New("HTTPS is off (network.https)")
	}
	if n.CA == nil {
		return nil, errors.New("this daemon has no CA")
	}
	return n.CA.CA()
}

func (n *Names) offerTrust() {
	n.mu.Lock()
	done := n.offered
	n.offered = true
	n.mu.Unlock()
	if done || n.Offer == nil {
		return
	}
	go n.Offer()
}

func (n *Names) hello(h *priv.HelloResult) {
	n.mu.Lock()
	n.helper = h != nil
	if h != nil {
		n.resolver = h.Resolver
	}
	n.mu.Unlock()
	if h != nil && h.ResolverPort > 0 && n.Port == 0 {
		n.serveZone(h.ResolverPort)
	}
	if h != nil {
		n.sendLocal(false)
	}
	n.publish()
}

func (n *Names) serveZone(port int) {
	n.mu.Lock()
	if n.closed || (n.zone != nil && n.zonePort == port) {
		n.mu.Unlock()
		return
	}
	old := n.zone
	n.zone, n.zonePort = nil, 0
	n.mu.Unlock()
	if old != nil {
		old.Close()
	}
	serve := n.Serve
	if serve == nil {
		serve = func(addr netip.AddrPort, r *domains.Registry) (io.Closer, error) { return domains.Serve(addr, r) }
	}
	addr := netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(port))
	z, err := serve(addr, n.zoneRegistry())
	if err != nil {
		n.host.Warn(WarnNamesZone, fmt.Sprintf("fev's names are not answered on this Mac: 127.0.0.1:%d cannot be served (%v)", port, err))
		n.log.Warn("names: the zone is not served", "addr", addr, "err", err)
		n.publish()
		return
	}
	n.host.Warn(WarnNamesZone, "")
	n.mu.Lock()
	n.zone, n.zonePort = z, port
	n.mu.Unlock()
	n.log.Info("names: fev.local answered for the Mac", "addr", addr)
}

func (n *Names) Answer(req *dns.Msg) (*dns.Msg, bool) {
	if r := n.Registry(); r != nil {
		return r.AnswerGuest(req)
	}
	return nil, false
}

func (n *Names) zoneRegistry() *domains.Registry { return n.Registry() }

func (n *Names) macChanged() { n.sendLocal(true) }

func (n *Names) sendLocal(network bool) {
	if n.Network == nil {
		n.publish()
		return
	}
	n.mu.Lock()
	if network && (len(n.sent) > 0 || n.anyPresentLocked()) {
		n.netChanged = true
	}
	if !n.helper || n.closed {
		n.mu.Unlock()
		return
	}
	if n.localBusy {
		n.localDirty = true
		n.mu.Unlock()
		return
	}
	n.localBusy = true
	n.mu.Unlock()
	go n.localLoop()
}

func localNamesWanted(all []domains.LocalName) (send []string, over []domains.LocalName) {
	for i, l := range all {
		if i >= priv.MaxLocalNames {
			over = append(over, all[i:]...)
			break
		}
		send = append(send, l.Name)
	}
	if send == nil {
		send = []string{}
	}
	return send, over
}

func (n *Names) localLoop() {
	for {
		n.mu.Lock()
		reg, network, sent := n.reg, n.netChanged, n.sent
		n.localDirty = false
		n.mu.Unlock()
		all := reg.LocalNames()
		want, over := localNamesWanted(all)
		if slices.Equal(want, sent) && !network && (len(want) > 0 || sent != nil) {
			n.localDone(all, over, nil)
			if n.localAgain() {
				continue
			}
			return
		}
		var r priv.LocalNames
		err := n.Network.HelperCall(priv.MethodLocalNamesSet, priv.LocalNamesParams{Names: want, NetworkChanged: network}, &r)
		var pe *priv.Error
		if errors.As(err, &pe) && pe.Code == priv.CodeTooSoon {
			n.mu.Lock()
			if !n.closed && n.retry == nil {
				n.retry = time.AfterFunc(time.Duration(priv.LocalNamesInterval)*time.Millisecond, func() {
					n.mu.Lock()
					n.retry = nil
					n.mu.Unlock()
					n.sendLocal(false)
				})
			}
			n.localBusy = false
			n.mu.Unlock()
			return
		}
		if err != nil {
			n.log.Warn("names: the helper did not take the custom names", "names", want, "err", err)
			n.mu.Lock()
			n.localBusy = false
			n.mu.Unlock()
			n.localDone(all, over, nil)
			return
		}
		n.mu.Lock()
		n.sent = want
		if network {
			n.netChanged = false
		}
		n.mu.Unlock()
		n.localDone(all, over, &r)
		if n.localAgain() {
			continue
		}
		return
	}
}

func (n *Names) localAgain() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.localDirty && !n.closed {
		return true
	}
	n.localBusy = false
	return false
}

func (n *Names) localDone(all, over []domains.LocalName, r *priv.LocalNames) {
	owner := map[string]string{}
	for _, l := range all {
		owner[l.Name] = l.Container
	}
	n.mu.Lock()
	if r != nil {
		for _, o := range r.Names {
			if o.State == priv.NameRemoved {
				delete(n.local, o.Name)
				continue
			}
			n.local[o.Name] = api.LocalNameStatus{Name: o.Name, Container: owner[o.Name], State: o.State, Reason: o.Reason, AnsweredBy: o.AnsweredBy}
		}
	}
	for name := range n.local {
		if _, ok := owner[name]; !ok {
			delete(n.local, name)
		}
	}
	for _, l := range all {
		if _, ok := n.local[l.Name]; !ok {
			n.local[l.Name] = api.LocalNameStatus{Name: l.Name, Container: l.Container, State: api.LocalWaiting}
		}
	}
	for _, l := range over {
		n.local[l.Name] = api.LocalNameStatus{Name: l.Name, Container: l.Container, State: priv.NameRefused,
			Reason: fmt.Sprintf("more than %d custom .local names", priv.MaxLocalNames)}
	}
	warn := map[string]string{}
	for _, l := range n.local {
		if l.State != priv.NameRefused {
			continue
		}
		text := fmt.Sprintf("%s (from container %s) does not resolve on this Mac: %s", l.Name, l.Container, l.Reason)
		if l.AnsweredBy != "" {
			text += "; answered by " + l.AnsweredBy
		}
		warn[warnLocalName+l.Name] = text
	}
	old := n.localWarned
	n.localWarned = map[string]bool{}
	for k := range warn {
		n.localWarned[k] = true
	}
	n.mu.Unlock()
	for k := range old {
		if _, ok := warn[k]; !ok {
			n.host.Warn(k, "")
		}
	}
	keys := make([]string, 0, len(warn))
	for k := range warn {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n.host.Warn(k, warn[k])
	}
	n.publish()
}

func (n *Names) Status() api.DomainsStatus {
	n.mu.Lock()
	reg, port, resolver, helper, mdns := n.reg, n.zonePort, n.resolver, n.helper, n.mdns
	local := make([]api.LocalNameStatus, 0, len(n.local))
	for _, l := range n.local {
		local = append(local, l)
	}
	n.mu.Unlock()
	st := api.DomainsStatus{Resolver: resolver, Port: port, MDNS: mdns}
	if st.Resolver == "" {
		st.Resolver = priv.StateAbsent
	}
	if reg != nil {
		st.Names = reg.Count()
		for _, c := range reg.Collisions() {
			st.Collisions = append(st.Collisions, fmt.Sprintf("%s: %s keeps it, %s does not get it", c.Name, c.Holder, c.Loser))
		}
		if !helper {
			state := api.LocalWaiting
			if mdns != "" {
				state = api.LocalMDNS
			}
			seen := map[string]bool{}
			for _, l := range local {
				seen[l.Name] = true
			}
			for _, l := range reg.LocalNames() {
				if !seen[l.Name] {
					local = append(local, api.LocalNameStatus{Name: l.Name, Container: l.Container, State: state})
				}
			}
		}
	}
	sort.Slice(local, func(i, j int) bool { return local[i].Name < local[j].Name })
	if len(local) > 0 {
		st.Local = local
	}
	return st
}

func (n *Names) logShadowed(cs []domains.Collision) {
	type pair struct{ machine, container string }
	names := map[pair][]string{}
	var order []pair
	for _, c := range cs {
		p := pair{strings.TrimPrefix(c.Loser, "machine "), strings.TrimPrefix(c.Holder, "container ")}
		if _, ok := names[p]; !ok {
			order = append(order, p)
		}
		names[p] = append(names[p], c.Name)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].machine != order[j].machine {
			return order[i].machine < order[j].machine
		}
		return order[i].container < order[j].container
	})
	var key strings.Builder
	for _, p := range order {
		fmt.Fprintf(&key, "%s<%s:%s;", p.machine, p.container, strings.Join(names[p], ","))
	}
	if key.String() == n.shadowed {
		return
	}
	was := n.shadowed
	n.shadowed = key.String()
	if n.log == nil {
		return
	}
	for _, p := range order {
		n.log.Info("names: a container holds a machine's name; the machine is reached by its name again when the container goes",
			"machine", p.machine, "container", p.container, "names", strings.Join(names[p], " "))
	}
	if len(order) == 0 && was != "" {
		n.log.Info("names: no machine's name is held by a container")
	}
}

func (n *Names) SetMDNS(iface string) {
	n.mu.Lock()
	changed := n.mdns != iface
	n.mdns = iface
	n.mu.Unlock()
	if changed {
		n.publish()
	}
}

func (n *Names) Subscribe(fn func()) func() {
	n.subsMu.Lock()
	id := n.nextID
	n.nextID++
	n.subs[id] = fn
	n.subsMu.Unlock()
	return func() {
		n.subsMu.Lock()
		delete(n.subs, id)
		n.subsMu.Unlock()
	}
}

func (n *Names) publish() {
	if n.host == nil {
		return
	}
	reg := n.Registry()
	if reg != nil {
		if c := reg.Collisions(); len(c) > 0 {
			parts := make([]string, len(c))
			for i, x := range c {
				parts[i] = fmt.Sprintf("%s (%s keeps it, %s does not get it)", x.Name, x.Holder, x.Loser)
			}
			n.host.Warn(WarnNamesCollision, "two claim one name: "+strings.Join(parts, "; "))
		} else {
			n.host.Warn(WarnNamesCollision, "")
		}
	}
	n.subsMu.Lock()
	if reg != nil {
		n.logShadowed(reg.Shadowed())
	}
	fns := make([]func(), 0, len(n.subs))
	for _, fn := range n.subs {
		fns = append(fns, fn)
	}
	n.subsMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}
