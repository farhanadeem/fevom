package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fev/host/daemon"
	"fev/host/daemon/boottrace"
	"fev/host/docker/dockerctx"
	"fev/host/docker/engine"
	"fev/host/docker/forward"
	"fev/host/docker/proxy"
	"fev/host/docker/vsock"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

const (
	portEngine = 2375
	portRelay  = 1026
)

const SocketName = "docker.sock"

const MarkSockBound = "docker.sock_bound"

type Options struct {
	Autostart     bool
	DockerConfig  string
	CompatLink    string
	SSHAuthSock   string
	Agent         func() string
	Binds         Binds
	Containers    Containers
	Trimmed       func()
	Quiet         func()
	LowPorts      forward.LowBinder
	BeforeRestart func(ctx context.Context, vm daemon.VM) error
}

const ListenerShare = 2

const (
	WarnPorts  = "docker.ports"
	WarnFeed   = "docker.events"
	WarnConfig = "docker.config"
)

func (m *Module) RunningContainers() (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.running), m.counted
}

func (m *Module) forwardChanged(f api.Forward, removed bool) {
	m.mu.Lock()
	if m.conflicts == nil {
		m.conflicts = map[string]api.Forward{}
	}
	_, was := m.conflicts[f.ID]
	is := !removed && f.State == api.ForwardConflict
	if is {
		m.conflicts[f.ID] = f
	} else {
		delete(m.conflicts, f.ID)
	}
	text := ""
	if was || is {
		text = conflictWarning(m.conflicts)
	}
	m.mu.Unlock()
	if was || is {
		m.host.Warn(WarnPorts, text)
	}
}

func conflictWarning(conflicts map[string]api.Forward) string {
	if len(conflicts) == 0 {
		return ""
	}
	names := make([]string, 0, len(conflicts))
	what := "container"
	for _, f := range conflicts {
		port := ""
		if len(f.Mac) > 0 {
			port = f.Mac[0][strings.LastIndex(f.Mac[0], ":")+1:]
		}
		who := f.Source
		if f.Owner != "" {
			owner := f.Owner
			if len(owner) > 12 {
				owner = owner[:12]
			}
			who = "container " + owner
			if f.Source == api.ForwardMachine {
				who, what = "machine "+f.Owner, "machine"
			}
		}
		names = append(names, fmt.Sprintf("%s/%s (%s)", port, f.Proto, who))
	}
	sort.Strings(names)
	const more = `"fev ports" says more and "fev ports --retry" tries again`
	if len(names) == 1 {
		return "port " + names[0] + " is not served: another program on this Mac has it, so the " + what + " is not reachable there; " + more
	}
	kind := "published"
	if what == "machine" {
		kind = "forwarded"
	}
	return fmt.Sprintf("%d %s ports are not served because other programs on this Mac have them: %s; %s", len(names), kind, strings.Join(names, ", "), more)
}

type Binds interface {
	Set(id string, sources []string)
	Synced(running map[string]bool)
	Clear()
}

type Containers interface {
	Container(id string, c *engine.Container)
	Synced(running map[string]bool)
	Clear()
}

type Module struct {
	opts Options
	host daemon.Host
	log  *slog.Logger

	proxy    *proxy.Proxy
	table    *forward.Table
	ctx      dockerctx.Store
	machines *machinePorts
	engine   *enginePorts

	state      sync.Mutex
	ready      bool
	bootID     string
	stop       context.CancelFunc
	watched    chan struct{}
	dial       proxy.Dialer
	parked     bool
	settling   string
	offStopped string
	settleGen  uint64
	settles    int

	mu                       sync.Mutex
	subs                     map[int]func(engine.Event)
	next                     int
	trim                     *time.Timer
	trimVM                   daemon.VM
	trimOwed                 bool
	paused                   bool
	trimArming               uint64
	quiet                    *quiet
	uplink                   netip.Addr
	uplinkCall               func(ctx context.Context, vm daemon.VM) netip.Addr
	trimCall                 func(ctx context.Context, vm daemon.VM) (*agent.TrimResult, error)
	restarts                 map[*restartWait]struct{}
	running                  map[string]bool
	counted                  bool
	conflicts                map[string]api.Forward
	configState, configError string
	configureCall            func(ctx context.Context, vm daemon.VM, config json.RawMessage) (*agent.EngineConfigured, error)
	restartCall              func(ctx context.Context, vm daemon.VM) error
	configRestart            string
	streams                  chan struct{}
	refusals                 chan struct{}
	streamFailures           atomic.Uint64
	streamFailuresSince      atomic.Int64
	refuseCall               func(ctx context.Context, vm daemon.VM, token, reason string)
}

var TrimDelay = 2 * time.Second

func New(opts Options) *Module {
	if opts.CompatLink == "" {
		opts.CompatLink = CompatLink
	}
	return &Module{opts: opts, quiet: newQuiet(), subs: map[int]func(engine.Event){}, restarts: map[*restartWait]struct{}{}, trimCall: func(ctx context.Context, vm daemon.VM) (*agent.TrimResult, error) {
		return vm.Agent().StorageTrim(ctx, agent.TrimParams{})
	}, configState: api.EngineConfigNone, streams: make(chan struct{}, agent.MaxStreams),
		refusals: make(chan struct{}, maxRefusals),
		refuseCall: func(ctx context.Context, vm daemon.VM, token, reason string) {
			if a := vm.Agent(); a != nil {
				a.StreamRefuse(ctx, token, reason)
			}
		},
		configureCall: func(ctx context.Context, vm daemon.VM, config json.RawMessage) (*agent.EngineConfigured, error) {
			a := vm.Agent()
			if a == nil {
				return nil, errNoAgent
			}
			return a.EngineConfigure(ctx, config)
		}, restartCall: func(ctx context.Context, vm daemon.VM) error {
			a := vm.Agent()
			if a == nil {
				return errNoAgent
			}
			_, err := a.EngineRestart(ctx, agent.EngineStopParams{})
			return err
		}, uplinkCall: func(ctx context.Context, vm daemon.VM) netip.Addr {
			a := vm.Agent()
			if a == nil {
				return netip.Addr{}
			}
			info, err := a.Info(ctx)
			if err != nil {
				return netip.Addr{}
			}
			return uplinkOf(info.Interfaces)
		}}
}

var errNoAgent = errors.New("the guest's agent is not connected")

func (m *Module) Name() string { return "docker" }

func (m *Module) WorkEnded() { m.quiet.ended() }

func (m *Module) Marks() []string { return []string{boottrace.DockerReleased} }

func (m *Module) Socket() string { return filepath.Join(m.host.Layout().RunDir(), SocketName) }

func (m *Module) Start(h daemon.Host) error {
	m.host, m.log = h, h.Log()
	limit, err := proxy.RaiseFileLimit()
	if err != nil {
		m.log.Warn("docker: cannot raise the open-file limit; many connections at once may fail", "err", err)
	} else {
		m.log.Debug("docker: open-file limit", "limit", limit)
	}
	p, err := proxy.Listen(m.Socket(), m.log)
	if err != nil {
		return err
	}
	m.proxy = p
	m.quiet.log = m.log
	m.quiet.after = m.opts.Quiet
	p.OnLongClose(LongLived, m.quiet.ended)
	h.Mark(MarkSockBound)
	if !m.opts.Autostart {
		switch {
		case !h.Config().Bool(api.KeyDockerEnabled):
			p.Refuse(api.EngineOffLine)
		case h.Status().StopRequested:
			p.Refuse(stoppedReason(""))
		default:
			p.StartOnDemand(m.startVM)
		}
	}
	m.table = forward.New(m.log, func(f api.Forward, removed bool) {
		h.PublishForward(api.ForwardsChanged{Forward: f, Removed: removed})
		m.forwardChanged(f, removed)
	})
	if m.opts.LowPorts != nil {
		m.table.SetLowBinder(m.opts.LowPorts)
	}
	m.ctx = dockerctx.Store{
		Dir: m.opts.DockerConfig, Socket: m.Socket(),
		StateFile: filepath.Join(filepath.Dir(h.Layout().ConfigFile()), "docker-context.json"),
	}
	if limit > 0 {
		m.table.SetListenerCap(int(min(limit/ListenerShare, 1<<30)))
	}
	m.table.SetLAN(h.Config().Bool(api.KeyNetworkPublishLAN))
	m.machines = &machinePorts{m: m, on: h.Config().Bool(api.KeyMachinesForwardPorts), lan: h.Config().Bool(api.KeyMachinesExposePortsToLAN)}
	m.engine = &enginePorts{m: m}
	h.Handle(api.MethodDockerStatus, m.status)
	h.Handle(api.MethodDockerRestart, m.restart)
	h.Handle(api.MethodForwardsList, func(context.Context, json.RawMessage) (any, error) {
		return api.Forwards{Forwards: m.table.List()}, nil
	})
	h.Handle(api.MethodForwardsRetry, m.retry)
	m.applyContext(h.Config().Bool(api.KeyDockerSetContext))
	return nil
}

func (m *Module) retry(_ context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.ForwardsRetryParams](params)
	if err != nil {
		return nil, err
	}
	rows, err := m.table.Retry(p.ID)
	if err != nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "%v", err)
	}
	return api.Forwards{Forwards: rows}, nil
}

func (m *Module) startVM() error {
	m.log.Info("docker: a connection found the VM stopped; starting it")
	return m.host.StartOnDemand()
}

func clipText(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func (m *Module) Held() int {
	if m.proxy == nil {
		return 0
	}
	held, _, _ := m.proxy.Counts()
	return held
}

func stoppedReason(why string) string {
	if why != "" {
		return "the VM is stopped: " + why
	}
	return "the VM is stopped"
}

func (m *Module) applyContext(set bool) {
	if m.ctx.Dir == "" {
		return
	}
	if !set {
		if err := m.ctx.Unselect(); err != nil {
			m.log.Warn("docker context: cannot restore the previous context", "err", err)
		}
		if err := m.ctx.Ensure(); err != nil {
			m.log.Warn("docker context: cannot write the context", "dir", m.ctx.Dir, "err", err)
		}
		return
	}
	selected, left, err := m.ctx.Select()
	switch {
	case err != nil:
		m.log.Warn("docker context: left as it is", "dir", m.ctx.Dir, "err", err)
	case !selected:
		m.log.Info("docker context: another context is selected and stays selected; use `docker --context fev`", "current", left)
	default:
		m.log.Info("docker context: fev is the current context", "dir", m.ctx.Dir)
	}
}

func (m *Module) status(context.Context, json.RawMessage) (any, error) {
	st := api.DockerStatus{Engine: m.host.Status().Engine, Socket: m.Socket(), Context: api.DockerContext{Name: dockerctx.Name}}
	if m.ctx.Dir != "" {
		if c, err := m.ctx.Status(); err == nil {
			st.Context = c
		}
	}
	st.CompatSocket, st.CompatTarget = compatStatus(m.opts.CompatLink, m.Socket())
	st.ConfigFile = m.ConfigFile()
	m.mu.Lock()
	st.ConfigState, st.ConfigError = m.configState, m.configError
	m.mu.Unlock()
	return st, nil
}

func (m *Module) restart(ctx context.Context, _ json.RawMessage) (any, error) {
	if !m.host.Config().Bool(api.KeyDockerEnabled) {
		return nil, jsonrpc.Errorf(api.CodeFailed, "%s", api.EngineOffLine)
	}
	if m.host.Status().State == api.StatePaused {
		m.host.Rouse(true)
	}
	vm, ok := m.host.VM()
	if !ok {
		return nil, jsonrpc.Errorf(api.CodeFailed, "the VM is not running")
	}
	w := &restartWait{done: make(chan error, 1)}
	m.mu.Lock()
	m.restarts[w] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.restarts, w)
		m.mu.Unlock()
	}()
	m.configure(ctx, vm)
	if m.opts.BeforeRestart != nil {
		if err := m.opts.BeforeRestart(ctx, vm); err != nil {
			m.log.Warn("docker: what the engine's start applies was not applied; the engine restarts all the same", "err", err)
		}
	}
	seq := m.host.Status().Seq
	m.mu.Lock()
	w.armed, w.after = true, seq
	m.mu.Unlock()
	m.log.Info("docker: restarting the engine")
	if err := m.restartCall(ctx, vm); err != nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "the engine was not restarted: %v", err)
	}
	select {
	case err := <-w.done:
		if err != nil {
			return nil, err
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return m.status(ctx, nil)
}

type restartWait struct {
	armed bool
	after uint64
	away  bool
	done  chan error
}

func (m *Module) restartsSee(st api.Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for w := range m.restarts {
		if !w.armed || st.Seq <= w.after {
			continue
		}
		var err error
		switch {
		case st.State == api.StateStopped || st.State == api.StateStopping:
			err = jsonrpc.Errorf(api.CodeFailed, "the VM was stopped during the restart")
		case st.Engine != api.EngineReady && st.Engine != api.EngineFailed:
			w.away = true
			continue
		case !w.away:
			continue
		case st.Engine == api.EngineFailed:
			err = jsonrpc.Errorf(api.CodeFailed, "%s", st.Error)
		}
		w.done <- err
		delete(m.restarts, w)
	}
}

func (m *Module) StateChanged(st api.Status) {
	see := true
	defer func() {
		if see {
			m.restartsSee(st)
		}
	}()
	m.state.Lock()
	defer m.state.Unlock()
	vm, haveVM := m.host.VM()
	m.followMachines(st, vm, haveVM)
	same := haveVM && vm.BootID() == m.bootID
	if st.State == api.StatePaused && m.ready && same {
		if !m.parked {
			m.parked = true
			m.proxy.Park(func() { m.host.Rouse(false) }, func() { go m.host.CheckAlive() })
			m.pauseTrim(true)
			m.quiet.pause(true)
		}
		return
	}
	off := !m.host.Config().Bool(api.KeyDockerEnabled)
	up := !off && haveVM && st.Engine == api.EngineReady && (st.State == api.StateRunning || st.State == api.StateStarting)
	if up && m.ready && same {
		if m.parked {
			m.parked = false
			released := m.proxy.Release(m.dial)
			m.pauseTrim(false)
			m.quiet.pause(false)
			m.log.Info("docker: the VM is resumed", "boot", m.bootID, "held", released.Joined, "abandoned", released.Abandoned)
		}
		return
	}
	if up && m.settling == vm.BootID() {
		see = false
		return
	}
	if m.ready {
		m.down()
	}
	m.settling = ""
	m.settleGen++
	switch {
	case off:
		m.proxy.Refuse(api.EngineOffLine)
		if haveVM && vm.Agent() != nil && (st.Engine == api.EngineReady || st.Engine == api.EngineStarting) && m.offStopped != vm.BootID() {
			m.offStopped = vm.BootID()
			m.stopForOff(vm)
		}
	case up:
		m.proxy.Hold()
		m.settle(vm, st)
		see = false
	case st.State == api.StateStopped && st.Error == "" && !st.StopRequested:
		m.proxy.StartOnDemand(m.startVM)
	case st.State == api.StateStopped:
		m.proxy.Refuse(stoppedReason(st.Error))
	case st.Engine == api.EngineFailed && st.State != api.StateStopping:
		reason := st.Error
		if reason == "" {
			reason = "the Docker engine did not start"
		}
		m.proxy.Refuse(reason)
	default:
		m.proxy.Hold()
	}
}

func (m *Module) settle(vm daemon.VM, st api.Status) {
	m.settling = vm.BootID()
	gen := m.settleGen
	m.settles++
	go func() {
		restart := m.configure(context.Background(), vm)
		m.state.Lock()
		m.settles--
		if m.settleGen != gen {
			m.state.Unlock()
			return
		}
		m.settling = ""
		if restart && m.reconfigure(vm) {
			m.state.Unlock()
			return
		}
		m.up(vm)
		m.state.Unlock()
		m.restartsSee(st)
	}()
}

func (m *Module) reconfigure(vm daemon.VM) bool {
	if m.configRestart == vm.BootID() {
		m.log.Error("docker: the engine was restarted for "+ConfigName+" and still does not run on it; left as it is", "boot", vm.BootID())
		return false
	}
	m.configRestart = vm.BootID()
	m.log.Info("docker: "+ConfigName+" changed while the VM was down; restarting the engine with it", "boot", vm.BootID())
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), guestCallTimeout)
		defer cancel()
		if err := m.restartCall(ctx, vm); err != nil {
			m.log.Error("docker: the engine was not restarted for "+ConfigName, "err", err)
		}
	}()
	return true
}

func (m *Module) followMachines(st api.Status, vm daemon.VM, haveVM bool) {
	if !haveVM || st.State == api.StateStopped {
		m.machines.gone()
		m.engine.gone()
		m.table.SetOpener(nil)
		return
	}
	m.table.SetOpener(vsock.Relay{Path: vm.VsockPath(portRelay)})
	m.machines.follow(vm)
	m.engine.follow(vm)
}

func (m *Module) up(vm daemon.VM) {
	m.ready, m.bootID = true, vm.BootID()
	if a := vm.Agent(); a != nil {
		a.OnStreamRequest(m.streamRequests(vm))
	}
	m.mu.Lock()
	m.trimVM = vm
	m.mu.Unlock()
	m.quiet.up(vm)
	enginePath := vm.VsockPath(portEngine)
	dial := func(ctx context.Context) (net.Conn, error) { return vsock.Dial(ctx, enginePath) }
	m.dial = dial
	m.table.SetOpener(vsock.Relay{Path: vm.VsockPath(portRelay)})

	ctx, cancel := context.WithCancel(context.Background())
	m.stop, m.watched = cancel, make(chan struct{})
	client := engine.NewClient(dial)
	watcher := &engine.Watcher{Client: client, Sink: sink{m}, Log: m.log}
	go func() {
		defer close(m.watched)
		defer client.Close()
		uplink := m.uplinkCall(ctx, vm)
		m.mu.Lock()
		m.uplink = uplink
		m.mu.Unlock()
		if !uplink.IsValid() {
			m.log.Warn("docker: the guest's uplink address is not known; ports published for every address are dialled at the guest's loopback")
		}
		if err := watcher.Run(ctx); err != nil && ctx.Err() == nil {
			m.log.Error("docker: the events feed is lost; published ports are not forwarded until the engine is restarted", "err", err)
			m.table.Drop(api.ForwardDocker)
			m.mu.Lock()
			m.running, m.counted = nil, false
			m.mu.Unlock()
			m.host.Warn(WarnFeed, "published ports are not forwarded to the Mac: the Docker engine's events could not be followed ("+
				clipText(err.Error(), 200)+"); \"fev restart docker\" starts the engine and the forwards again")
		}
	}()

	m.engine.again()
	released := m.proxy.Release(dial)
	m.host.Mark(boottrace.DockerReleased)
	m.log.Info("docker: the engine is ready", "boot", m.bootID, "held", released.Joined, "abandoned", released.Abandoned)
}

func (m *Module) down() {
	m.ready, m.parked, m.dial = false, false, nil
	m.mu.Lock()
	if m.trim != nil {
		m.trim.Stop()
		m.trim = nil
	}
	m.trimVM, m.trimOwed, m.paused = nil, false, false
	m.running, m.counted = nil, false
	m.mu.Unlock()
	m.quiet.down()
	m.host.Warn(WarnFeed, "")
	m.stop()
	<-m.watched
	m.table.Drop(api.ForwardDocker)
	if m.opts.Binds != nil {
		m.opts.Binds.Clear()
	}
	if m.opts.Containers != nil {
		m.opts.Containers.Clear()
	}
}

func (m *Module) ConfigChanged(ch api.ConfigChange) {
	set, ok := ch.Value.(bool)
	switch {
	case !ok:
	case ch.Key == api.KeyDockerSetContext:
		m.applyContext(set)
	case ch.Key == api.KeyNetworkPublishLAN:
		m.table.SetLAN(set)
	case ch.Key == api.KeyMachinesForwardPorts:
		m.machines.setOn(set)
	case ch.Key == api.KeyMachinesExposePortsToLAN:
		m.machines.setLAN(set)
	case ch.Key == api.KeyDockerEnabled:
		m.enable(set)
	}
}

func (m *Module) enable(on bool) {
	if !on {
		m.state.Lock()
		m.offStopped = ""
		m.state.Unlock()
	}
	m.StateChanged(m.host.Status())
	vm, ok := m.host.VM()
	if !on || !ok || vm.Agent() == nil {
		return
	}
	a := vm.Agent()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), guestCallTimeout)
		defer cancel()
		m.log.Info("docker: docker.enabled turned on; starting the engine")
		if _, err := a.EngineStart(ctx); err != nil {
			m.log.Error("docker: the engine was not started for docker.enabled", "err", err)
		}
	}()
}

func (m *Module) stopForOff(vm daemon.VM) {
	a := vm.Agent()
	if a == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), guestCallTimeout)
		defer cancel()
		m.log.Info("docker: docker.enabled is false; stopping the engine")
		if _, err := a.EngineStop(ctx, agent.EngineStopParams{}); err != nil {
			m.log.Error("docker: the engine was not stopped for docker.enabled", "err", err)
		}
	}()
}

func (m *Module) Close() {
	if m.proxy == nil {
		return
	}
	m.state.Lock()
	m.settleGen++
	if m.ready {
		m.down()
	}
	m.state.Unlock()
	m.table.Close()
	m.proxy.Close()
}

func (m *Module) Subscribe(fn func(engine.Event)) (cancel func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.next
	m.next++
	m.subs[id] = fn
	return func() {
		m.mu.Lock()
		delete(m.subs, id)
		m.mu.Unlock()
	}
}

type sink struct{ m *Module }

func (s sink) Container(id string, c *engine.Container) {
	s.m.mu.Lock()
	if s.m.running == nil {
		s.m.running = map[string]bool{}
	}
	if c == nil {
		delete(s.m.running, id)
	} else {
		s.m.running[id] = true
	}
	s.m.mu.Unlock()
	if s.m.opts.Containers != nil {
		s.m.opts.Containers.Container(id, c)
	}
	if c == nil {
		s.m.table.Set(api.ForwardDocker, id, nil)
		if s.m.opts.Binds != nil {
			s.m.opts.Binds.Set(id, nil)
		}
		return
	}
	if s.m.opts.Binds != nil {
		s.m.opts.Binds.Set(id, c.Binds)
	}
	if len(c.SCTP) > 0 {
		s.m.log.Info("docker: a container publishes SCTP ports, which are not forwarded to the Mac", "container", c.Name, "ports", c.SCTP)
	}
	s.m.mu.Lock()
	uplink := s.m.uplink
	s.m.mu.Unlock()
	s.m.table.Set(api.ForwardDocker, id, Wants(c.Bindings, uplink))
}

func (s sink) Synced(running map[string]bool) {
	s.m.mu.Lock()
	s.m.running = make(map[string]bool, len(running))
	for id := range running {
		s.m.running[id] = true
	}
	s.m.counted = true
	s.m.mu.Unlock()
	s.m.host.Warn(WarnFeed, "")
	for _, owner := range s.m.table.Owners(api.ForwardDocker) {
		if !running[owner] && owner != HostNetworkOwner {
			s.m.table.Set(api.ForwardDocker, owner, nil)
		}
	}
	if s.m.opts.Binds != nil {
		s.m.opts.Binds.Synced(running)
	}
	if s.m.opts.Containers != nil {
		s.m.opts.Containers.Synced(running)
	}
}

func frees(e engine.Event) bool {
	switch e.Type {
	case engine.TypeImage:
		return e.Action == "delete" || e.Action == "untag" || e.Action == "prune"
	case engine.TypeVolume:
		return e.Action == "destroy" || e.Action == "prune"
	case engine.TypeContainer:
		return e.Action == "destroy" || e.Action == "prune"
	case "builder":
		return e.Action == "prune"
	}
	return false
}

func (m *Module) removed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.trimVM == nil {
		return
	}
	if m.trim != nil {
		m.trim.Stop()
	}
	if m.paused {
		m.trimOwed = true
		return
	}
	m.armTrim()
}

func (m *Module) pauseTrim(paused bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.paused = paused
	switch {
	case paused && m.trim != nil:
		m.trim.Stop()
		m.trim, m.trimOwed = nil, true
	case !paused && m.trimOwed && m.trimVM != nil:
		m.trimOwed = false
		m.armTrim()
	}
}

func (m *Module) trimNow(vm daemon.VM, arming uint64) {
	m.mu.Lock()
	current := m.trimVM == vm
	if m.trimArming == arming {
		m.trim = nil
	}
	m.mu.Unlock()
	if !current {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), guestCallTimeout)
	defer cancel()
	r, err := m.trimCall(ctx, vm)
	if err != nil {
		m.mu.Lock()
		owed := m.paused && m.trimVM == vm
		if owed {
			m.trimOwed = true
		}
		m.mu.Unlock()
		if owed {
			m.log.Info("docker: the trim after a removal did not finish before the VM was paused; it is made again at the wake", "err", err)
		} else {
			m.log.Warn("docker: the trim after a removal failed; the space returns with the next one", "err", err)
		}
		return
	}
	m.log.Info("docker: trimmed the data disk after a removal", "ms", math.Round(r.DurationMS), "free_bytes", r.TrimmedBytes)
	if m.opts.Trimmed != nil {
		m.opts.Trimmed()
	}
}

func (m *Module) armTrim() {
	vm := m.trimVM
	m.trimArming++
	arming := m.trimArming
	m.trim = time.AfterFunc(TrimDelay, func() { m.trimNow(vm, arming) })
}

func (s sink) Event(e engine.Event) {
	if frees(e) {
		s.m.removed()
	}
	if ends(e) {
		s.m.quiet.ended()
	}
	if e.Type == engine.TypeContainer && e.Action == "start" {
		s.m.quiet.started()
	}
	switch e.Type {
	case engine.TypeContainer, engine.TypeNetwork, engine.TypeVolume, engine.TypeImage:
		s.m.host.PublishDocker(api.DockerEvent{
			Type: e.Type, Action: e.Action, Detail: e.Detail, ID: e.ID, Name: e.Name,
			Attributes: e.Attributes, Time: e.Time.UTC().Format(time.RFC3339Nano),
		})
	}
	s.m.mu.Lock()
	subs := make([]func(engine.Event), 0, len(s.m.subs))
	for _, fn := range s.m.subs {
		subs = append(subs, fn)
	}
	s.m.mu.Unlock()
	for _, fn := range subs {
		fn(e)
	}
}

func (m *Module) SetK8sForwards(wants []forward.Want) {
	if m.table != nil {
		m.table.Set("kubernetes", "k8s", wants)
	}
}

func (m *Module) K8sForwards() []api.Forward {
	if m.table == nil {
		return nil
	}
	var out []api.Forward
	for _, f := range m.table.List() {
		if f.Source == "kubernetes" {
			out = append(out, f)
		}
	}
	return out
}
