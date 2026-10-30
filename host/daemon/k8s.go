package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"fev/host/daemon/config"
	"fev/host/docker/forward"
	"fev/host/docker/vsock"
	"fev/host/domains"
	"fev/host/k8s"
	"fev/host/k8s/kubeconfig"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	priv "fev/proto/privhelper"
	"fev/proto/relay"
)

type K8s struct {
	Names          *Names
	Network        *Network
	Machines       *Machines
	Forwards       func(wants []forward.Want)
	ForwardList    func() []api.Forward
	Taken          func() []netip.Prefix
	Kubeconfig     string
	KubeconfigNote string
	APIPort        int
	Child          func(ctx context.Context, vm VM) (*k8s.Client, func(), error)
	Fence          FenceFunc
	Freed          func()

	host Host
	log  *slog.Logger

	mu                  sync.Mutex
	boot                string
	guest               agent.K8sStatus
	client              *k8s.Client
	stop                func()
	reach               k8s.Reach
	pulling             map[string]bool
	busy                string
	api, apiNote, kcErr string
	up                  chan struct{}
	upDone              bool
	ready               chan struct{}
	readyDone           bool
	clientErr           string
	ends                []time.Time
	nextSub             uint64
	subs                map[uint64]*k8sSub
	said                *k8sSaid
	kick                chan struct{}
}

type k8sSaid struct {
	vm VM
	st agent.K8sStatus
}

type k8sSub struct {
	caller Caller
	stop   func()
}

const k8sCost = "Kubernetes uses about a tenth of a core and 2 GiB of memory while on"

const (
	k8sVersion      = "v1.37.1+k3s1"
	k8sDownloadSize = 73728162 + 176489629
	k8sArchiveURL   = "https://github.com/k3s-io/k3s/releases/download/v1.37.1%2Bk3s1/k3s-airgap-images-arm64.tar.zst"
)

func (k *K8s) Name() string    { return "k8s" }
func (k *K8s) Marks() []string { return nil }

func (k *K8s) Start(h Host) error {
	k.host, k.log = h, h.Log()
	if k.Kubeconfig != "" && k.KubeconfigNote == "" {
		k.KubeconfigNote = kubeconfig.NotePath(h.Layout().Home)
	}
	k.subs = map[uint64]*k8sSub{}
	k.kick = make(chan struct{}, 1)
	go k.follow()
	if k.Network != nil {
		prev := k.Network.Extra
		k.Network.Extra = func() []netip.Prefix {
			var out []netip.Prefix
			if prev != nil {
				out = prev()
			} else if k.Names != nil {
				out = k.Names.EngineNetworks()
			}
			if k.connected() {
				out = append(out, planInEffect(h.Config()).Kube()...)
			}
			return out
		}
	}
	for method, fn := range map[string]MethodFunc{
		api.MethodK8sStatus:  k.status,
		api.MethodK8sEnable:  k.enable,
		api.MethodK8sDisable: k.disable,
		api.MethodK8sReset:   k.reset,
		api.MethodK8sWatch:   k.watch,
		api.MethodK8sUnwatch: k.unwatch,
		api.MethodK8sGet:     k.get,
		api.MethodK8sApply:   k.apply,
		api.MethodK8sUpdate:  k.update,
		api.MethodK8sScale:   k.scale,
		api.MethodK8sRestart: k.restart,
		api.MethodK8sDelete:  k.delete,
		api.MethodK8sLogs:    k.logs,
		api.MethodK8sExec:    k.exec,
	} {
		h.Handle(method, fn)
	}
	return nil
}

func (k *K8s) connected() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.client != nil
}

func (k *K8s) StateChanged(st api.Status) {
	if st.State == api.StateStopped || st.State == api.StateStopping {
		k.disconnect("the VM " + st.State)
		k.mu.Lock()
		k.boot, k.guest = "", agent.K8sStatus{}
		k.mu.Unlock()
		return
	}
	if st.State != api.StateRunning || (st.Engine != api.EngineReady && st.Engine != api.EngineOff) {
		return
	}
	vm, ok := k.host.VM()
	if !ok || vm.Agent() == nil {
		return
	}
	k.mu.Lock()
	if k.boot == vm.BootID() {
		k.mu.Unlock()
		return
	}
	k.boot = vm.BootID()
	k.mu.Unlock()
	a := vm.Agent()
	a.OnK8sState(func(s agent.K8sStatus) { k.tell(vm, s) })
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s, err := a.K8sStatus(ctx)
		if err != nil {
			if !k8s.IsCode(err, agent.CodeNotImplemented) {
				k.log.Warn("k8s: the guest's cluster", "err", err)
			}
			return
		}
		k.tell(vm, *s)
		if k.host.Config().Bool(api.KeyKubernetesEnabled) && !s.Wanted {
			k.startOp(context.Background(), api.MethodK8sEnable)
		}
	}()
}

var k8sClientRetry = 2 * time.Second

const (
	k8sClientEnds   = 3
	k8sClientWindow = time.Minute
)

const k8sConnectWait = 30 * time.Second

func (k *K8s) readyLocked() chan struct{} {
	if k.ready == nil {
		k.ready = make(chan struct{})
	}
	return k.ready
}

const k8sReadyWait = 90 * time.Second

func (k *K8s) upLocked() chan struct{} {
	if k.up == nil {
		k.up = make(chan struct{})
	}
	return k.up
}

func (k *K8s) tell(vm VM, s agent.K8sStatus) {
	k.mu.Lock()
	k.said = &k8sSaid{vm: vm, st: s}
	k.mu.Unlock()
	select {
	case k.kick <- struct{}{}:
	default:
	}
}

func (k *K8s) follow() {
	for range k.kick {
		k.mu.Lock()
		s := k.said
		k.said = nil
		k.mu.Unlock()
		if s != nil {
			k.guestSaid(s.vm, s.st)
		}
	}
}

func (k *K8s) guestSaid(vm VM, s agent.K8sStatus) {
	k.mu.Lock()
	if vm.BootID() != k.boot {
		k.mu.Unlock()
		return
	}
	if s.Kubeconfig == "" {
		s.Kubeconfig = k.guest.Kubeconfig
	}
	k.guest = s
	connected := k.client != nil
	k.mu.Unlock()
	k.showLine()
	switch {
	case s.State == agent.K8sRunning && !connected:
		k.connectOrRetry(vm)
	case s.State != agent.K8sRunning && connected:
		k.disconnect("the cluster is " + s.State)
	}
	if s.State != agent.K8sRunning {
		k.mu.Lock()
		k.clientErr, k.ends = "", nil
		k.mu.Unlock()
	}
}

func (k *K8s) clientWanted(vm VM) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return vm.BootID() == k.boot && k.guest.State == agent.K8sRunning && k.client == nil &&
		k.busy != api.MethodK8sDisable && k.busy != api.MethodK8sReset
}

func (k *K8s) connectOrRetry(vm VM) {
	err := k.connect(vm)
	if err == nil || !k.clientWanted(vm) {
		return
	}
	k.log.Warn("k8s: the cluster's client did not start; starting it once more", "in", k8sClientRetry, "err", err)
	time.AfterFunc(k8sClientRetry, func() {
		if !k.clientWanted(vm) {
			return
		}
		if err := k.connect(vm); err != nil && k.clientWanted(vm) {
			k.clientFailed("fev-k8s, the cluster's client, did not start again: " + err.Error() + " (fevd's log has its words; `fev k8s start` tries again)")
		}
	})
}

func (k *K8s) clientFailed(why string) {
	k.log.Warn("k8s: " + why)
	k.mu.Lock()
	k.clientErr = why
	k.mu.Unlock()
	k.showLine()
}

func (k *K8s) clientEnded(vm VM) {
	if !k.clientWanted(vm) {
		return
	}
	now := time.Now()
	k.mu.Lock()
	kept := k.ends[:0]
	for _, t := range k.ends {
		if now.Sub(t) < k8sClientWindow {
			kept = append(kept, t)
		}
	}
	k.ends = append(kept, now)
	n := len(k.ends)
	k.mu.Unlock()
	if n >= k8sClientEnds {
		k.clientFailed(fmt.Sprintf("fev-k8s, the cluster's client, ended %d times in %v; fevd's log has its words (`fev k8s start` tries again)", n, k8sClientWindow))
		return
	}
	k.log.Warn("k8s: the cluster's client ended while the cluster runs; starting it again", "ends", n)
	k.connectOrRetry(vm)
}

func (k *K8s) connect(vm VM) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a := vm.Agent()
	s, err := a.K8sStatus(ctx)
	switch {
	case err != nil:
		k.log.Warn("k8s: the cluster's kubeconfig", "err", err)
		return fmt.Errorf("the guest's k8s.status: %w", err)
	case s.State != agent.K8sRunning:
		return nil
	case s.Kubeconfig == "":
		return errors.New("the cluster runs and its kubeconfig could not be read")
	}
	child := k.Child
	if child == nil {
		child = k.child
	}
	c, stop, err := child(ctx, vm)
	if err != nil {
		k.log.Warn("k8s: the cluster's client did not start", "err", err)
		k.setReach(k8s.Reach{Error: "the cluster's client did not start: " + err.Error()})
		return err
	}
	plan := planInEffect(k.host.Config())
	apiAddr, apiNote := k.chooseAPI()
	cfg := k8s.Config{
		Kubeconfig: s.Kubeconfig,
		Relay:      vm.VsockPath(relay.Port),
		Ranges: api.K8sRanges{LoadBalancers: plan.KubeLoadBalancers().String(), Pods: plan.KubePods().String(),
			Services: plan.KubeServices().String(), DNS: plan.KubeDNS().String()},
		LAN:        k.host.Config().Bool(api.KeyKubernetesExposeLAN),
		APIAddress: apiAddr,
	}
	k.mu.Lock()
	if k.client != nil || vm.BootID() != k.boot {
		k.mu.Unlock()
		c.Close()
		stop()
		return nil
	}
	k.client, k.stop = c, stop
	k.api, k.apiNote = apiAddr, apiNote
	k.mu.Unlock()
	if err := c.Start(ctx, cfg, k.applyReach); err != nil {
		k.log.Warn("k8s: the cluster's client did not begin watching", "err", err)
		k.disconnect("its watch did not begin")
		k.setReach(k8s.Reach{Error: "watching the cluster: " + err.Error()})
		return err
	}
	k.mergeContext(s.Kubeconfig, apiAddr)
	k.setZoneHook(vm, plan.KubeDNS())
	k.mu.Lock()
	if !k.upDone {
		close(k.upLocked())
		k.upDone = true
	}
	k.clientErr = ""
	k.mu.Unlock()
	k.showLine()
	if k.Network != nil {
		k.Network.Reroute("the cluster runs")
	}
	k.log.Info("k8s: the cluster's client watches", "boot", vm.BootID())
	go func() {
		<-c.Done()
		k.mu.Lock()
		gone := k.client == c
		k.mu.Unlock()
		if gone {
			k.disconnect("the cluster's client ended")
			k.clientEnded(vm)
		}
	}()
	return nil
}

func (k *K8s) disconnect(why string) {
	k.mu.Lock()
	c, stop := k.client, k.stop
	k.client, k.stop = nil, nil
	k.api, k.apiNote = "", ""
	if k.upDone {
		k.up, k.upDone = nil, false
	}
	if k.readyDone {
		k.ready, k.readyDone = nil, false
	}
	subs := k.subs
	k.subs = map[uint64]*k8sSub{}
	k.reach = k8s.Reach{}
	k.mu.Unlock()
	for _, s := range subs {
		s.stop()
	}
	if c == nil {
		return
	}
	c.Close()
	if stop != nil {
		stop()
	}
	if k.Forwards != nil {
		k.Forwards(nil)
	}
	if r := k.registry(); r != nil {
		r.SetK8s(nil)
		r.SetForward(domains.ClusterZone, nil)
		r.SetExtraLocal(nil)
	}
	if k.Network != nil {
		k.Network.Reroute("the cluster stopped")
	}
	k.log.Info("k8s: the cluster's client stopped", "why", why)
}

func (k *K8s) registry() *domains.Registry {
	if k.Names == nil {
		return nil
	}
	return k.Names.Registry()
}

func (k *K8s) setReach(r k8s.Reach) {
	k.mu.Lock()
	k.reach = r
	if r.Ready && k.client != nil && !k.readyDone {
		close(k.readyLocked())
		k.readyDone = true
	}
	k.mu.Unlock()
	k.showLine()
}

func (k *K8s) applyReach(r k8s.Reach) {
	pulls := r.Pulls
	r.Pulls = nil
	k.setReach(r)
	k.pull(pulls)
	if reg := k.registry(); reg != nil {
		var svcs []domains.K8sService
		for _, t := range r.Targets {
			a, err := netip.ParseAddr(t.Address)
			if err != nil {
				continue
			}
			svcs = append(svcs, domains.K8sService{ID: t.ID, Address: a, Names: t.Names, HTTPPort: t.HTTPPort, HTTPSPort: t.HTTPSPort,
				Redirect80: t.Redirect80, Own443: t.Own443, Pass443: t.Pass443, Hosts: t.Hosts})
		}
		reg.SetK8s(svcs)
	}
	if k.Forwards != nil {
		lo := netip.AddrFrom4([4]byte{127, 0, 0, 1})
		macAPI := k.macAPIPort()
		var wants []forward.Want
		for _, f := range r.Forwards {
			proto := forward.TCP
			if f.Proto == "udp" {
				proto = forward.UDP
			}
			mac := uint16(f.Port)
			if proto == forward.TCP && f.Port == k8sGuestAPIPort && macAPI != 0 {
				mac = macAPI
			}
			wants = append(wants, forward.Want{Proto: proto, Port: mac, Guest: netip.AddrPortFrom(lo, uint16(f.Port)), LAN: f.LAN})
		}
		k.Forwards(wants)
	}
}

func (k *K8s) pull(refs []string) {
	if len(refs) == 0 {
		return
	}
	vm, ok := k.host.VM()
	if !ok || vm.Agent() == nil {
		return
	}
	for _, ref := range slices.Compact(slices.Sorted(slices.Values(refs))) {
		k.mu.Lock()
		if k.pulling == nil {
			k.pulling = map[string]bool{}
		}
		busy := k.pulling[ref]
		k.pulling[ref] = true
		k.mu.Unlock()
		if busy {
			continue
		}
		go func(ref string) {
			defer func() {
				k.mu.Lock()
				delete(k.pulling, ref)
				k.mu.Unlock()
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			t0 := time.Now()
			r, err := vm.Agent().K8sImagePull(ctx, ref)
			switch {
			case err == nil:
				k.log.Info("k8s: a Never pod's image taken from Docker", "ref", ref, "digest", r.Digest, "ms", time.Since(t0).Milliseconds())
			case k8s.IsCode(err, agent.CodeNotImplemented):
			default:
				k.log.Info("k8s: a Never pod's image", "ref", ref, "err", err)
			}
		}(ref)
	}
}

func k8sRanges(cfg *config.Store) [][2]string {
	plan := planInEffect(cfg)
	return [][2]string{
		{"LoadBalancer", plan.KubeLoadBalancers().String()},
		{"pod", plan.KubePods().String()},
		{"service", plan.KubeServices().String()},
	}
}

func (k *K8s) rangeConflicts() []string {
	if k.Taken == nil {
		return nil
	}
	taken := k.Taken()
	var out []string
	for _, r := range k8sRanges(k.host.Config()) {
		p := netip.MustParsePrefix(r[1])
		var with []string
		for _, t := range taken {
			if t.Addr().Is4() && t.Overlaps(p) {
				with = append(with, t.Masked().String())
			}
		}
		if len(with) > 0 {
			out = append(out, fmt.Sprintf("the %s range %s meets the Mac's %s: fev reset moves fev to a base clear of it", r[0], r[1], strings.Join(with, ", ")))
		}
	}
	return out
}

func (k *K8s) portConflicts() []string {
	if k.ForwardList == nil {
		return nil
	}
	var out []string
	for _, f := range k.ForwardList() {
		if f.Source != k8sForwardSource || f.State == api.ForwardActive {
			continue
		}
		port := f.Guest
		if _, p, err := net.SplitHostPort(f.Guest); err == nil {
			port = p
		}
		line := fmt.Sprintf("%s %s on the Mac: %s", f.Proto, port, f.State)
		if f.Error != "" {
			line += " (" + f.Error + ")"
		}
		out = append(out, line)
	}
	return out
}

const k8sForwardSource = "kubernetes"

func (k *K8s) clusterLocal() api.K8sClusterLocal {
	if k.Names == nil {
		return api.K8sClusterLocal{Note: "this daemon answers no names"}
	}
	st := k.Names.Status()
	port := ""
	if st.Port != 0 {
		port = fmt.Sprintf(" (fev's zone still answers it on 127.0.0.1:%d)", st.Port)
	}
	for _, l := range st.Local {
		if l.Name != domains.ClusterZone {
			continue
		}
		switch l.State {
		case priv.NamePresent:
			return api.K8sClusterLocal{Answered: true}
		case api.LocalMDNS:
			return api.K8sClusterLocal{Answered: true, Note: "answered by mDNS on the no-admin path's bridge"}
		case api.LocalWaiting:
			return api.K8sClusterLocal{Note: "no admin helper: the Mac's resolver asks fev for fev.local alone; fev setup --admin adds cluster.local" + port}
		default:
			why := l.Reason
			if why == "" {
				why = l.State
			}
			return api.K8sClusterLocal{Note: "cluster.local is answered by another program's resolver, so fev leaves it: " + why + port}
		}
	}
	return api.K8sClusterLocal{Note: "not yet asked of the Mac's resolver" + port}
}

func (k *K8s) setZoneHook(vm VM, dnsAddr netip.Addr) {
	reg := k.registry()
	if reg == nil {
		return
	}
	opener := vsock.Relay{Path: vm.VsockPath(relay.Port)}
	at := netip.AddrPortFrom(dnsAddr, 53).String()
	reg.SetForward(domains.ClusterZone, func(req *dns.Msg) (*dns.Msg, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, err := opener.Open(ctx, relay.Request{Kind: relay.TCP, Target: at})
		if err != nil {
			return nil, err
		}
		defer c.Close()
		dl, _ := ctx.Deadline()
		c.SetDeadline(dl)
		dc := &dns.Conn{Conn: c}
		if err := dc.WriteMsg(req); err != nil {
			return nil, err
		}
		return dc.ReadMsg()
	})
	reg.SetExtraLocal([]string{domains.ClusterZone})
}

func (k *K8s) line() *api.K8sLine {
	k.mu.Lock()
	defer k.mu.Unlock()
	enabled := k.host.Config().Bool(api.KeyKubernetesEnabled)
	if !enabled && (k.guest.State == "" || k.guest.State == agent.K8sOff) {
		return nil
	}
	l := &api.K8sLine{Enabled: enabled, State: k.state(), Ready: k.reach.Ready, Version: k.guest.Version, Error: k.errorLocked()}
	if l.State != api.K8sStateError {
		l.Error = ""
	}
	return l
}

func (k *K8s) errorLocked() string {
	if k.guest.State == agent.K8sRunning && k.clientErr != "" {
		return k.clientErr
	}
	return k.guest.Error
}

func (k *K8s) state() string {
	switch k.guest.State {
	case agent.K8sRunning:
		if k.upDone {
			return api.K8sStateRunning
		}
		if k.clientErr != "" {
			return api.K8sStateError
		}
		return api.K8sStateStarting
	case agent.K8sStopping:
		return api.K8sStateStopping
	case agent.K8sError:
		return api.K8sStateError
	case "", agent.K8sOff:
	default:
		return api.K8sStateStarting
	}
	switch k.busy {
	case api.MethodK8sEnable:
		return api.K8sStateStarting
	case api.MethodK8sDisable, api.MethodK8sReset:
		return api.K8sStateStopping
	}
	return api.K8sStateOff
}

func (k *K8s) showLine() {
	line := k.line()
	k.mu.Lock()
	boot := k.boot
	k.mu.Unlock()
	updateStatus(k.host, func(st *api.Status) {
		if boot == "" || st.BootID == boot {
			st.K8s = line
		}
	})
}

func (k *K8s) status(ctx context.Context, params json.RawMessage) (any, error) {
	cfg := k.host.Config()
	plan := planInEffect(cfg)
	k.mu.Lock()
	st := api.K8sStatus{
		Enabled: cfg.Bool(api.KeyKubernetesEnabled),
		State:   k.state(),
		Ready:   k.reach.Ready,
		Node:    k.reach.Node,
		Version: k.guest.Version,
		Ranges: api.K8sRanges{LoadBalancers: plan.KubeLoadBalancers().String(), Pods: plan.KubePods().String(),
			Services: plan.KubeServices().String(), DNS: plan.KubeDNS().String()},
		Kubeconfig: api.K8sKubeconfig{Path: k.Kubeconfig, Context: "fev"},
		Services:   k.reach.Services,
		Ingresses:  k.reach.Ingresses,
		Archive:    api.K8sArchive{Version: k8sVersion, Size: k8sDownloadSize, URL: k8sArchiveURL},
		Cost:       k8sCost,
	}
	if st.Version == "" {
		st.Version = k.reach.Version
	}
	if st.State == api.K8sStateError {
		st.Error = k.errorLocked()
	}
	if k.reach.Error != "" && st.Error == "" {
		st.Error = k.reach.Error
	}
	if k.guest.Archive.Size != 0 {
		st.Archive.Cached, st.Archive.Bytes, st.Archive.Size = k.guest.Archive.Cached, k.guest.Archive.Bytes, k.guest.Archive.Size
		if k.guest.Archive.URL != "" {
			st.Archive.URL = k.guest.Archive.URL
		}
	}
	connected := k.client != nil
	if connected {
		st.API = api.K8sAPIForward{Address: k.api, Note: k.apiNote}
	}
	kcErr := k.kcErr
	k.mu.Unlock()
	if u := cfg.String(api.KeyKubernetesImagesURL); u != "" {
		st.Archive.URL = u
	}
	if connected {
		st.ClusterLocal = k.clusterLocal()
		st.PortConflicts = k.portConflicts()
	} else {
		st.ClusterLocal.Note = "the cluster is off"
	}
	if st.Enabled || connected || st.State != api.K8sStateOff {
		st.RangeConflicts = k.rangeConflicts()
	}
	if k.Kubeconfig != "" {
		st.Kubeconfig.Present, st.Kubeconfig.Current = kubeconfig.Present(k.Kubeconfig)
	}
	if kcErr != "" && st.Error == "" {
		st.Error = kcErr
	}
	return st, nil
}

func (k *K8s) startOp(ctx context.Context, method string) (any, error) {
	return k.startOpWith(ctx, method, api.K8sResetParams{})
}

func (k *K8s) startOpWith(ctx context.Context, method string, rp api.K8sResetParams) (any, error) {
	if k.Machines == nil || k.Machines.ops == nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "this daemon has no operations table")
	}
	cfg := k.host.Config()
	if method == api.MethodK8sEnable && !cfg.Bool(api.KeyDockerEnabled) {
		return nil, jsonrpc.Errorf(api.CodeFailed, "Kubernetes is refused while docker.enabled is false: the cluster runs beside the Docker engine and its images are Docker's")
	}
	k.mu.Lock()
	if k.busy != "" {
		busy := k.busy
		k.mu.Unlock()
		return nil, jsonrpc.Errorf(api.CodeBusy, "%s is under way", busy)
	}
	k.busy = method
	k.mu.Unlock()
	k.showLine()
	phase := api.K8sPhaseChecking
	if method != api.MethodK8sEnable {
		phase = api.K8sPhaseStopping
	}
	return k.Machines.ops.Start(ctx, method, "kubernetes", phase, func(op *Operation) {
		defer func() {
			k.mu.Lock()
			k.busy = ""
			k.mu.Unlock()
			k.showLine()
		}()
		var err error
		switch method {
		case api.MethodK8sEnable:
			err = k.runEnable(op)
		case api.MethodK8sDisable:
			err = k.runDisable(op)
		case api.MethodK8sReset:
			err = k.runReset(op, rp)
		}
		if err != nil {
			op.Fail(err)
		}
	}), nil
}

func (k *K8s) enable(ctx context.Context, params json.RawMessage) (any, error) {
	return k.startOp(ctx, api.MethodK8sEnable)
}

func (k *K8s) disable(ctx context.Context, params json.RawMessage) (any, error) {
	return k.startOp(ctx, api.MethodK8sDisable)
}

func (k *K8s) reset(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.K8sResetParams](params)
	if err != nil {
		return nil, err
	}
	return k.startOpWith(ctx, api.MethodK8sReset, p)
}

func k8sConfig(h Host) agent.K8sConfig {
	cfg := h.Config()
	c := agent.K8sConfig{MetricsServer: cfg.Bool(api.KeyKubernetesMetricsServer)}
	if v := cfg.Object(api.KeyKubernetesKubelet); len(v) > 0 {
		c.Kubelet, _ = json.Marshal(v)
	}
	return c
}

func archiveURL(u string) string { return u }

func (k *K8s) runEnable(op *Operation) error {
	octx := op.Context()
	k.mu.Lock()
	k.clientErr, k.ends = "", nil
	k.mu.Unlock()
	if c := k.rangeConflicts(); len(c) > 0 {
		op.Note(strings.Join(c, "; "))
		k.log.Warn("k8s: the cluster's ranges meet the Mac's networks", "conflicts", c)
	}
	client, err := k.Machines.engineGuest(octx)
	if err != nil {
		return err
	}
	fenced, err := k.criRestart(op, client, true)
	if err != nil {
		return err
	}
	undo := func() {
		if fenced {
			if _, err := k.criRestart(op, client, false); err != nil {
				k.log.Warn("k8s: containerd back without its CRI plugins", "err", err)
			}
		}
	}
	g := newGuestOp(op)
	gop, err := client.K8sStart(octx, agent.K8sStartParams{ArchiveURL: archiveURL(k.host.Config().String(api.KeyKubernetesImagesURL)), Config: k8sConfig(k.host)}, g.progress)
	if err != nil {
		undo()
		return guestError(err)
	}
	end, err := g.wait(client, gop.ID)
	switch {
	case err != nil:
		undo()
		return err
	case end.State == agent.OperationCancelled:
		undo()
		op.Cancelled()
		return nil
	case end.State != agent.OperationDone:
		undo()
		return errors.New(end.Error)
	}
	if _, err := k.host.Config().Set(map[string]any{api.KeyKubernetesEnabled: true}); err != nil {
		return err
	}
	var st agent.K8sStatus
	json.Unmarshal(end.Result, &st)
	if vm, ok := k.host.VM(); ok {
		k.tell(vm, st)
	}
	switch st.State {
	case agent.K8sError:
		return fmt.Errorf("the cluster failed to start: %s", st.Error)
	case agent.K8sRunning:
	default:
		return fmt.Errorf("the cluster's start ended with the cluster %s, not running", st.State)
	}
	if st.State == agent.K8sRunning {
		k.mu.Lock()
		up := k.upLocked()
		k.mu.Unlock()
		select {
		case <-up:
		case <-octx.Done():
		case <-time.After(k8sConnectWait):
			k.log.Warn("k8s: the cluster runs and its client did not connect", "within", k8sConnectWait)
		}
	}
	op.Progress(api.K8sPhaseReady, 0, 0)
	if st.State == agent.K8sRunning {
		k.mu.Lock()
		ready := k.readyLocked()
		k.mu.Unlock()
		select {
		case <-ready:
		case <-octx.Done():
		case <-time.After(k8sReadyWait):
			k.log.Warn("k8s: the cluster runs and its node is not Ready", "within", k8sReadyWait)
		}
	}
	line := api.K8sLine{Enabled: true, State: api.K8sStateStarting, Version: st.Version}
	k.mu.Lock()
	if st.State == agent.K8sRunning && k.upDone {
		line.State = api.K8sStateRunning
	}
	line.Ready = k.reach.Ready
	k.mu.Unlock()
	op.Done(line)
	return nil
}

func (k *K8s) runDisable(op *Operation) error {
	octx := op.Context()
	k.disconnect("k8s.disable")
	if vm, ok := k.host.VM(); ok && vm.Agent() != nil {
		a := vm.Agent()
		st, err := a.K8sStopKeepingCRI(octx)
		if err != nil && !k8s.IsCode(err, agent.CodeNotImplemented) {
			return guestError(err)
		}
		if err == nil {
			k.tell(vm, *st)
			if st.CRI {
				if _, err := k.criRestart(op, a, false); err != nil {
					return err
				}
			}
		}
	}
	if _, err := k.host.Config().Set(map[string]any{api.KeyKubernetesEnabled: false}); err != nil {
		return err
	}
	k.removeContext("k8s.disable")
	op.Done(api.K8sLine{State: api.K8sStateOff})
	return nil
}

func (k *K8s) runReset(op *Operation, p api.K8sResetParams) error {
	octx := op.Context()
	k.disconnect("k8s.reset")
	client, err := k.Machines.engineGuest(octx)
	if err != nil {
		return err
	}
	g := newGuestOp(op)
	gop, err := client.K8sReset(octx, agent.K8sResetParams{All: p.All, KeepCRI: true}, g.progress)
	if err != nil {
		return guestError(err)
	}
	end, err := g.wait(client, gop.ID)
	switch {
	case err != nil:
		return err
	case end.State == agent.OperationCancelled:
		op.Cancelled()
		return nil
	case end.State != agent.OperationDone:
		return errors.New(end.Error)
	}
	if _, err := k.criRestart(op, client, false); err != nil {
		return err
	}
	if _, err := k.host.Config().Set(map[string]any{api.KeyKubernetesEnabled: false}); err != nil {
		return err
	}
	if vm, ok := k.host.VM(); ok {
		k.tell(vm, agent.K8sStatus{State: agent.K8sOff})
	}
	k.removeContext("k8s.reset")
	var r agent.K8sResetResult
	json.Unmarshal(end.Result, &r)
	if k.Freed != nil {
		k.Freed()
	}
	op.Done(api.K8sResetResult{FreedBytes: r.FreedBytes, ArchiveKept: r.ArchiveKept})
	return nil
}

func (k *K8s) ConfigChanged(ch api.ConfigChange) {
	switch ch.Key {
	case api.KeyKubernetesEnabled:
		on, _ := ch.Value.(bool)
		k.mu.Lock()
		state, busy := k.state(), k.busy
		k.mu.Unlock()
		if busy != "" || on == (state == api.K8sStateRunning || state == api.K8sStateStarting) {
			if !on && busy == "" {
				k.removeContext("kubernetes.enabled false")
			}
			return
		}
		if _, ok := k.host.VM(); !ok && on {
			return
		}
		method := api.MethodK8sDisable
		if on {
			method = api.MethodK8sEnable
		}
		if _, err := k.startOp(context.Background(), method); err != nil {
			k.log.Warn("k8s: kubernetes.enabled", "err", err)
		}
	case api.KeyKubernetesExposeLAN:
		on, _ := ch.Value.(bool)
		k.mu.Lock()
		c := k.client
		k.mu.Unlock()
		if c != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := c.SetLAN(ctx, on); err != nil {
				k.log.Warn("k8s: kubernetes.expose_services_to_lan", "err", err)
			}
		}
	case api.KeyKubernetesKubelet, api.KeyKubernetesMetricsServer:
		vm, ok := k.host.VM()
		if !ok || vm.Agent() == nil {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if _, err := vm.Agent().K8sConfigure(ctx, k8sConfig(k.host)); err != nil && !k8s.IsCode(err, agent.CodeNotImplemented) {
				k.log.Warn("k8s: the kubelet's settings", "key", ch.Key, "err", err)
			}
		}()
	}
}

func (k *K8s) Close() { k.disconnect("the daemon stops") }

func (k *K8s) cluster() (*k8s.Client, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.client == nil {
		if k.reach.Error != "" {
			return nil, jsonrpc.Errorf(api.CodeFailed, "Kubernetes is not reachable: %s", k.reach.Error)
		}
		return nil, jsonrpc.Errorf(api.CodeFailed, "Kubernetes is off: fev k8s start turns it on")
	}
	return k.client, nil
}

func (k *K8s) openSub(ctx context.Context, stop func()) uint64 {
	caller, _ := CallerOf(ctx)
	k.mu.Lock()
	k.nextSub++
	id := k.nextSub
	k.subs[id] = &k8sSub{caller: caller, stop: stop}
	k.mu.Unlock()
	if caller != nil {
		caller.OnGone(func() { k.endSub(id) })
	}
	return id
}

func (k *K8s) endSub(id uint64) bool {
	k.mu.Lock()
	s := k.subs[id]
	delete(k.subs, id)
	k.mu.Unlock()
	if s != nil {
		s.stop()
	}
	return s != nil
}

func (k *K8s) watch(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.K8sWatchParams](params)
	if err != nil {
		return nil, err
	}
	c, err := k.cluster()
	if err != nil {
		return nil, err
	}
	caller, ok := CallerOf(ctx)
	if !ok {
		return nil, jsonrpc.Errorf(api.CodeFailed, "k8s.watch needs a connection")
	}
	var mu sync.Mutex
	var id uint64
	var held []api.K8sChanged
	objects, stop, err := c.Watch(ctx, p, func(ch api.K8sChanged) {
		mu.Lock()
		defer mu.Unlock()
		if id == 0 {
			held = append(held, ch)
			return
		}
		ch.Subscription = id
		caller.Notify(api.NotifyK8sChanged, ch)
	})
	if err != nil {
		return nil, err
	}
	sub := k.openSub(ctx, stop)
	if objects == nil {
		objects = []api.K8sObject{}
	}
	jsonrpc.AfterReply(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		id = sub
		for _, ch := range held {
			ch.Subscription = id
			caller.Notify(api.NotifyK8sChanged, ch)
		}
		held = nil
	})
	return api.K8sWatching{Subscription: sub, Objects: objects}, nil
}

func (k *K8s) unwatch(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.Subscription](params)
	if err != nil {
		return nil, err
	}
	k.endSub(p.Subscription)
	return api.Empty{}, nil
}

func (k *K8s) get(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.K8sRef](params)
	if err != nil {
		return nil, err
	}
	c, err := k.cluster()
	if err != nil {
		return nil, err
	}
	return c.Get(ctx, p)
}

func (k *K8s) apply(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.K8sApplyParams](params)
	if err != nil {
		return nil, err
	}
	if len(p.Documents) == 0 {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "k8s.apply: no documents")
	}
	c, err := k.cluster()
	if err != nil {
		return nil, err
	}
	return c.Apply(ctx, p)
}

func (k *K8s) update(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.K8sUpdateParams](params)
	if err != nil {
		return nil, err
	}
	c, err := k.cluster()
	if err != nil {
		return nil, err
	}
	return c.Update(ctx, p)
}

func (k *K8s) scale(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.K8sScaleParams](params)
	if err != nil {
		return nil, err
	}
	c, err := k.cluster()
	if err != nil {
		return nil, err
	}
	return c.Scale(ctx, p)
}

func (k *K8s) restart(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.K8sRef](params)
	if err != nil {
		return nil, err
	}
	c, err := k.cluster()
	if err != nil {
		return nil, err
	}
	return c.Restart(ctx, p)
}

func (k *K8s) delete(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.K8sRef](params)
	if err != nil {
		return nil, err
	}
	c, err := k.cluster()
	if err != nil {
		return nil, err
	}
	return api.Empty{}, c.Delete(ctx, p)
}

func (k *K8s) logs(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.K8sLogsParams](params)
	if err != nil {
		return nil, err
	}
	if p.Namespace == "" || p.Pod == "" {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "k8s.logs: a namespace and a pod")
	}
	c, err := k.cluster()
	if err != nil {
		return nil, err
	}
	caller, ok := CallerOf(ctx)
	if !ok {
		return nil, jsonrpc.Errorf(api.CodeFailed, "k8s.logs needs a connection")
	}
	var mu sync.Mutex
	var id uint64
	var held []func(uint64)
	send := func(fn func(uint64)) {
		mu.Lock()
		defer mu.Unlock()
		if id == 0 {
			held = append(held, fn)
			return
		}
		fn(id)
	}
	stop, err := c.Logs(ctx, p, func(text string) {
		send(func(id uint64) { caller.Notify(api.NotifyK8sLog, api.K8sLog{Subscription: id, Text: text}) })
	}, func(err error) {
		send(func(id uint64) {
			k.mu.Lock()
			delete(k.subs, id)
			k.mu.Unlock()
			end := api.K8sLogEnd{Subscription: id}
			if err != nil {
				end.Error = err.Error()
			}
			caller.Notify(api.NotifyK8sLogEnd, end)
		})
	})
	if err != nil {
		return nil, err
	}
	sub := k.openSub(ctx, stop)
	jsonrpc.AfterReply(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		id = sub
		for _, fn := range held {
			fn(id)
		}
		held = nil
	})
	return api.Subscription{Subscription: sub}, nil
}

func (k *K8s) exec(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.K8sExecParams](params)
	if err != nil {
		return nil, err
	}
	if p.Namespace == "" || p.Pod == "" {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "k8s.exec: a namespace and a pod")
	}
	c, err := k.cluster()
	if err != nil {
		return nil, err
	}
	caller, ok := CallerOf(ctx)
	if !ok || k.Machines == nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "k8s.exec needs a connection and the session table")
	}
	e, err := c.Exec(ctx, p)
	if err != nil {
		return nil, err
	}
	id, token := k.Machines.sessions.openHost(caller, e.Stream, func(rows, cols int) { e.Resize(rows, cols) }, e.Close, func() api.SessionExit {
		x := e.Wait()
		return api.SessionExit{ExitCode: x.ExitCode, Error: x.Error, StreamEnded: x.Error == ""}
	})
	return api.K8sExecStarted{ID: id, Token: token, Namespace: p.Namespace, Pod: p.Pod, Container: e.Container}, nil
}

func standinK8sSocket(vm VM) string {
	return filepath.Join(filepath.Dir(vm.VsockPath(agent.Port)), "k8s.sock")
}
