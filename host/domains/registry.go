package domains

import (
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/miekg/dns"

	"fev/proto/agent"
	"fev/proto/netplan"
)

type Container struct {
	ID          string
	Name        string
	Labels      map[string]string
	NetworkMode string
	Networks    []Network
	SandboxKey  string
	Exposed     []string
}

type Network struct {
	Name     string
	Address  netip.Addr
	Address6 netip.Addr
}

type Machine struct {
	Name                string
	Address             netip.Addr
	Address6            netip.Addr
	HTTPPort, HTTPSPort int
}

type target struct {
	kind        string
	id          string
	name        string
	order       uint64
	c           *Container
	m           *Machine
	k           *K8sService
	key         string
	addr        netip.Addr
	dnat, dnat6 netip.Addr
	wants       []string
	names       []string
}

type Collision struct {
	Name, Holder, Loser string
}

type Options struct {
	Plan        netplan.Plan
	Plan6       netplan.Plan6
	Has6        bool
	AddressFile string
	HTTPS       bool
	OnChange    func()
}

type Registry struct {
	plan     netplan.Plan
	plan6    atomic.Pointer[netplan.Plan6]
	mac6     atomic.Bool
	onChange func()
	notify   sync.Mutex

	mu          sync.Mutex
	https       bool
	order       uint64
	gen         uint64
	containers  map[string]*target
	machines    map[string]*target
	k8s         map[string]*target
	forward     map[string]func(*dns.Msg) (*dns.Msg, error)
	extraLocal  []string
	addrs       *addresses
	exact, wild map[string]*target
	collisions  []Collision
}

func New(o Options) *Registry {
	plan := o.Plan
	if plan.Base() == (netip.Prefix{}) {
		plan = netplan.Default()
	}
	names := plan.Names()
	first := names.Addr()
	for !plan.NameAddress(first) {
		first = first.Next()
	}
	last := first
	for n := last.Next(); names.Contains(n) && plan.NameAddress(n); n = n.Next() {
		last = n
	}
	r := &Registry{
		plan:       plan,
		onChange:   o.OnChange,
		https:      o.HTTPS,
		containers: map[string]*target{},
		machines:   map[string]*target{},
		k8s:        map[string]*target{},
		addrs:      newAddresses(first, last, plan.Index(), o.AddressFile),
		exact:      map[string]*target{},
		wild:       map[string]*target{},
	}
	r.SetPlan6(o.Plan6, o.Has6)
	return r
}

func (r *Registry) Plan() netplan.Plan { return r.plan }

func (r *Registry) SetContainer(id string, c *Container) {
	r.mu.Lock()
	changed := r.setContainer(id, c)
	if changed {
		r.rebuild()
	}
	r.mu.Unlock()
	if changed {
		r.changed()
	}
}

func (r *Registry) setContainer(id string, c *Container) bool {
	old := r.containers[id]
	if c == nil {
		if old == nil {
			return false
		}
		delete(r.containers, id)
		r.releaseKey(old.key, id)
		return true
	}
	t := &target{kind: agent.TargetContainer, id: id, name: c.Name, c: c, key: "c:" + strings.ToLower(c.Name)}
	if old != nil {
		t.order = old.order
		if old.key != t.key {
			r.releaseKey(old.key, id)
		}
	} else {
		r.order++
		t.order = r.order
	}
	t.dnat, t.dnat6 = currentAddress(c)
	if t.dnat.IsValid() {
		if ip, ok := r.addrs.take(t.key); ok {
			t.addr = ip
		}
	} else if old != nil && old.key == t.key {
		r.addrs.release(t.key)
	}
	r.containers[id] = t
	return true
}

func (r *Registry) releaseKey(key, id string) {
	for oid, o := range r.containers {
		if oid != id && o.key == key {
			return
		}
	}
	r.addrs.release(key)
}

func (r *Registry) Synced(running map[string]bool) {
	r.mu.Lock()
	changed := false
	for id := range r.containers {
		if !running[id] {
			changed = r.setContainer(id, nil) || changed
		}
	}
	if changed {
		r.rebuild()
	}
	r.mu.Unlock()
	if changed {
		r.changed()
	}
}

func (r *Registry) ClearContainers() { r.Synced(nil) }

func (r *Registry) SetMachine(name string, m *Machine) {
	r.mu.Lock()
	key := strings.ToLower(name)
	old := r.machines[key]
	changed := true
	switch {
	case m == nil && old == nil:
		changed = false
	case m == nil:
		delete(r.machines, key)
	default:
		if old != nil && *old.m == *m {
			changed = false
			break
		}
		if old != nil {
			if was := *old.m; was.Address6 != m.Address6 {
				if was.Address6 = m.Address6; was == *m {
					old.m = m
					changed = false
					break
				}
			}
		}
		t := &target{kind: agent.TargetMachine, id: name, name: name, m: m, addr: m.Address}
		if old != nil {
			t.order = old.order
		} else {
			r.order++
			t.order = r.order
		}
		r.machines[key] = t
	}
	if changed {
		r.rebuild()
	}
	r.mu.Unlock()
	if changed {
		r.changed()
	}
}

func (r *Registry) ClearMachines() {
	r.mu.Lock()
	changed := len(r.machines) > 0
	r.machines = map[string]*target{}
	if changed {
		r.rebuild()
	}
	r.mu.Unlock()
	if changed {
		r.changed()
	}
}

func (r *Registry) SetHTTPS(on bool) {
	r.mu.Lock()
	changed := r.https != on
	r.https = on
	if changed {
		r.gen++
	}
	r.mu.Unlock()
	if changed {
		r.changed()
	}
}

func (r *Registry) changed() {
	if r.onChange == nil {
		return
	}
	r.notify.Lock()
	defer r.notify.Unlock()
	r.onChange()
}

func currentAddress(c *Container) (netip.Addr, netip.Addr) {
	switch {
	case c.NetworkMode == "host", c.NetworkMode == "none", strings.HasPrefix(c.NetworkMode, "container:"):
		return netip.Addr{}, netip.Addr{}
	}
	nets := append([]Network(nil), c.Networks...)
	sort.Slice(nets, func(i, j int) bool { return nets[i].Name < nets[j].Name })
	for _, n := range nets {
		if n.Name == c.NetworkMode && n.Address.Is4() {
			return n.Address, n.Address6
		}
	}
	for _, n := range nets {
		if n.Address.Is4() {
			return n.Address, n.Address6
		}
	}
	return netip.Addr{}, netip.Addr{}
}

func (r *Registry) wantsOf(t *target, firstReplica map[string]string) []string {
	var out []string
	add := func(n string) {
		if checkName(n, true) == nil {
			out = append(out, n)
		}
	}
	own := func(base string) {
		base = canonical(base)
		add(base + "." + Zone)
		add("*." + base + "." + Zone)
	}
	switch t.kind {
	case agent.TargetMachine:
		own(t.name)
	case agent.TargetK8s:
		out = append(out, k8sWants(t.k)...)
	case agent.TargetContainer:
		own(t.name)
		if svc, proj := t.c.Labels[labelComposeService], t.c.Labels[labelComposeProject]; svc != "" && proj != "" && firstReplica[proj+"/"+svc] == t.id {
			own(svc + "." + proj)
		}
		if v, ok := t.c.Labels[agent.LabelDomains]; ok {
			names, _ := ParseLabel(v)
			out = append(out, names...)
		}
	}
	return out
}

func (r *Registry) rebuild() {
	r.gen++
	firstReplica := map[string]string{}
	best := map[string][2]uint64{}
	for _, t := range r.containers {
		svc, proj := t.c.Labels[labelComposeService], t.c.Labels[labelComposeProject]
		if svc == "" || proj == "" || !t.addr.IsValid() {
			continue
		}
		n, err := strconv.ParseUint(t.c.Labels[labelComposeNumber], 10, 32)
		if err != nil {
			n = 1 << 32
		}
		k := proj + "/" + svc
		if b, ok := best[k]; !ok || n < b[0] || n == b[0] && t.order < b[1] {
			best[k] = [2]uint64{n, t.order}
			firstReplica[k] = t.id
		}
	}
	all := make([]*target, 0, len(r.machines)+len(r.containers)+len(r.k8s))
	for _, t := range r.machines {
		all = append(all, t)
	}
	for _, t := range r.k8s {
		all = append(all, t)
	}
	for _, t := range r.containers {
		all = append(all, t)
	}
	sort.Slice(all, func(i, j int) bool {
		if (all[i].kind == agent.TargetMachine) != (all[j].kind == agent.TargetMachine) {
			return all[j].kind == agent.TargetMachine
		}
		return all[i].order < all[j].order
	})
	r.exact, r.wild, r.collisions = map[string]*target{}, map[string]*target{}, nil
	for _, t := range all {
		t.wants = r.wantsOf(t, firstReplica)
		t.names = nil
		for _, n := range t.wants {
			table, key := r.exact, n
			if strings.HasPrefix(n, "*.") {
				table, key = r.wild, n[2:]
			}
			if h, ok := table[key]; ok {
				if h != t {
					r.collisions = append(r.collisions, Collision{Name: n, Holder: h.label(), Loser: t.label()})
				}
				continue
			}
			table[key] = t
			t.names = append(t.names, n)
		}
		sort.Strings(t.names)
	}
}

func (t *target) label() string {
	if t.kind == agent.TargetMachine {
		return "machine " + t.name
	}
	if t.kind == agent.TargetK8s {
		return "service " + t.name
	}
	return "container " + t.name
}

func (r *Registry) lookup(name string) *target {
	if t, ok := r.exact[name]; ok {
		return t
	}
	for n := name; ; {
		if t, ok := r.wild[n]; ok {
			return t
		}
		_, rest, ok := strings.Cut(n, ".")
		if !ok {
			return nil
		}
		n = rest
	}
}

type Answer struct {
	Found    bool
	Address  netip.Addr
	Target   string
	Address6 netip.Addr
	Mac6     netip.Addr
}

func (r *Registry) SetMac6(on bool) { r.mac6.Store(on) }

func (r *Registry) SetPlan6(p netplan.Plan6, ok bool) {
	if !ok {
		r.plan6.Store(nil)
		return
	}
	r.plan6.Store(&p)
}

func (r *Registry) mac6Of(a *Answer, t *target) {
	p6 := r.plan6.Load()
	if p6 == nil || !r.mac6.Load() {
		return
	}
	switch {
	case t != nil && t.m != nil:
		a.Mac6 = t.m.Address6
	case t != nil && t.k != nil:
	case a.Address.Is4():
		a.Mac6 = p6.Name6(a.Address)
	}
}

func (r *Registry) Lookup(name string) Answer {
	name = canonical(name)
	if name == Zone {
		a := Answer{Found: true, Address: r.plan.Index(), Target: "index"}
		r.mac6Of(&a, nil)
		return a
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.lookup(name)
	if t == nil {
		return Answer{}
	}
	a := Answer{Found: true, Address: t.addr, Target: t.label()}
	if t.m != nil {
		a.Address6 = t.m.Address6
	}
	r.mac6Of(&a, t)
	return a
}

func (r *Registry) Owns(name string) bool {
	name = canonical(name)
	if under(name, Zone) {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lookup(name) != nil
}

func (r *Registry) zoneOf(name string) string {
	if under(name, Zone) {
		return Zone
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entryOf(name)
}

func (r *Registry) Generation() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gen
}

func (r *Registry) Collisions() []Collision {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Collision(nil), r.collisions...)
}

func (r *Registry) Shadowed() []Collision {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Collision
	for _, c := range r.collisions {
		if strings.HasPrefix(c.Loser, "machine ") && strings.HasPrefix(c.Holder, "container ") {
			out = append(out, c)
		}
	}
	return out
}

func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.exact) + len(r.wild)
}

type LocalName struct {
	Name      string
	Container string
}

func (r *Registry) LocalNames() []LocalName {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	var out []LocalName
	for _, t := range r.containers {
		if !t.addr.IsValid() {
			continue
		}
		for _, n := range t.names {
			n = strings.TrimPrefix(n, "*.")
			if !IsLocal(n) || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, LocalName{Name: n, Container: t.name})
		}
	}
	for _, n := range r.extraLocal {
		if !seen[n] {
			seen[n] = true
			out = append(out, LocalName{Name: n, Container: "kubernetes"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) CanIssue(name, requester string, onMac func(entry string) (on, wait bool)) error {
	name = canonical(name)
	if strings.Contains(name, "*") {
		return fmt.Errorf("%s: fev signs exact names, never a wildcard: list each name", name)
	}
	if !strings.HasSuffix(name, ".local") {
		return fmt.Errorf("%s: only registered names under .local are signed", name)
	}
	if name == Zone {
		if requester != "" {
			return fmt.Errorf("%s is fev's own", name)
		}
		return nil
	}
	r.mu.Lock()
	t := r.lookup(name)
	own := t != nil && (requester == "" || t.isRequester(requester))
	entry := r.entryOf(name)
	r.mu.Unlock()
	if t == nil {
		return fmt.Errorf("%s: %w", name, errNoName)
	}
	if !own {
		return fmt.Errorf("%s is not the requester's own", name)
	}
	if requester != "" && !under(name, Zone) {
		on, wait := false, false
		if onMac != nil {
			on, wait = onMac(entry)
		}
		switch {
		case wait:
			return fmt.Errorf("%s is not on this Mac yet: ask again in a moment", name)
		case !on:
			return fmt.Errorf("%s is not on this Mac: a custom name gets a leaf only while the Mac resolves it to fev (fev status says why it does not)", name)
		}
	}
	return nil
}

func (r *Registry) entryOf(name string) string {
	if _, ok := r.exact[name]; ok {
		return name
	}
	for n := name; ; {
		if _, ok := r.wild[n]; ok {
			return n
		}
		_, rest, ok := strings.Cut(n, ".")
		if !ok {
			return name
		}
		n = rest
	}
}

const MachineRequester = "machine:"

func (t *target) isRequester(requester string) bool {
	if m, ok := strings.CutPrefix(requester, MachineRequester); ok {
		return t.kind == agent.TargetMachine && m != "" && strings.EqualFold(t.name, m)
	}
	return t.kind == agent.TargetContainer && t.id == requester
}

func (r *Registry) Params(seq uint64, ca string) agent.DomainsParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := agent.DomainsParams{Seq: seq, HTTPS: r.https, Index: r.plan.Index().String(), Targets: []agent.DomainTarget{}}
	if r.https {
		p.CA = ca
	}
	add := func(t *target) {
		if !t.addr.IsValid() || len(t.names) == 0 {
			return
		}
		d := agent.DomainTarget{Kind: t.kind, ID: t.id, Name: t.name, Address: t.addr.String(), Names: append([]string(nil), t.names...)}
		switch t.kind {
		case agent.TargetContainer:
			d.Target = t.dnat.String()
			if t.dnat6.IsValid() {
				d.Target6 = t.dnat6.String()
			}
			d.NetNS = t.c.SandboxKey
			d.HTTPPort = labelPort(t.c.Labels[agent.LabelHTTPPort])
			d.HTTPSPort = labelPort(t.c.Labels[agent.LabelHTTPSPort])
			d.Exposed = append([]string(nil), t.c.Exposed...)
			sort.Strings(d.Exposed)
			if svc, proj := t.c.Labels[labelComposeService], t.c.Labels[labelComposeProject]; svc != "" && proj != "" {
				for _, n := range t.names {
					if n == canonical(svc+"."+proj)+"."+Zone {
						d.Name = svc + "." + proj
					}
				}
			}
		case agent.TargetK8s:
			d.HTTPPort, d.HTTPSPort, d.Redirect80 = t.k.HTTPPort, t.k.HTTPSPort, t.k.Redirect80
			d.Own443, d.Pass443 = t.k.Own443, t.k.Pass443
			for _, h := range t.k.Hosts {
				if h = canonical(h); h != "" && !slices.Contains(d.Hosts, h) {
					d.Hosts = append(d.Hosts, h)
				}
			}
			sort.Strings(d.Hosts)
		case agent.TargetMachine:
			d.HTTPPort, d.HTTPSPort = t.m.HTTPPort, t.m.HTTPSPort
			if t.m.Address6.IsValid() {
				d.Address6 = t.m.Address6.String()
			}
		}
		p.Targets = append(p.Targets, d)
	}
	for _, t := range r.machines {
		add(t)
	}
	for _, t := range r.k8s {
		add(t)
	}
	for _, t := range r.containers {
		add(t)
	}
	sort.Slice(p.Targets, func(i, j int) bool {
		a, _ := netip.ParseAddr(p.Targets[i].Address)
		b, _ := netip.ParseAddr(p.Targets[j].Address)
		return a.Less(b)
	})
	return p
}

func labelPort(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 || n > 65535 {
		return 0
	}
	return n
}
