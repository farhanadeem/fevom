package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"fev/host/daemon/boottrace"
	"fev/host/daemon/config"
	"fev/host/daemon/home"
	"fev/host/daemon/launch"
	"fev/host/daemon/logs"
	"fev/host/daemon/responsible"
	"fev/host/daemon/vm"
	"fev/host/power"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/version"
	"fev/proto/vmmctl"
)

type Options struct {
	Layout           home.Layout
	Launcher         vm.Launcher
	Modules          []Module
	Autostart        bool
	Ready            io.WriteCloser
	MainNS, OriginNS uint64
	LogMaxBytes      int64
	Echo             io.Writer
	Timing           *Timing
	Host             *config.Host
	Warnings         []string
	Power            power.Source
	PowerInject      string
	Assert           func(reason string, display bool) (release func(), err error)
	LoginItem        *LoginItem
	Login            bool
	OnDemand         bool
	KernelRelease    string
}

var ErrAlreadyRunning = errors.New("a daemon is already running in this home")

type Daemon struct {
	opts   Options
	layout home.Layout
	log    *slog.Logger
	logs   *logs.Hub
	cfg    *config.Store
	hub    *Hub
	sup    *Supervisor

	daemonLock, startLock *os.File
	listener              net.Listener
	methods               map[string]MethodFunc
	events                *moduleQueue

	cfgMu   sync.Mutex
	nextSub atomic.Uint64
	pumps   sync.WaitGroup

	mu    sync.Mutex
	info  home.Info
	conns map[*jsonrpc.Conn]struct{}

	caffeine             *caffeine
	power                power.Source
	powerStop, powerDone chan struct{}

	askMu   sync.Mutex
	askedNS uint64

	filesMu   sync.Mutex
	lastFiles *api.FilesStatus
	filesView func() *api.FilesView

	shutMu       sync.Mutex
	stopping     bool
	stopAsked    bool
	shuttingDown atomic.Bool
	done         chan struct{}
}

var responsibleSelf = responsible.Self

func writeReady(w io.WriteCloser, r launch.Ready) {
	if w == nil {
		return
	}
	data, _ := json.Marshal(r)
	w.Write(append(data, '\n'))
	w.Close()
}

func Start(opts Options) (d *Daemon, err error) {
	layout := opts.Layout
	fail := func(err error) (*Daemon, error) {
		writeReady(opts.Ready, launch.Ready{Status: launch.StatusError, Error: err.Error()})
		return nil, err
	}
	if err := layout.Validate(); err != nil {
		return fail(err)
	}
	if err := layout.Create(); err != nil {
		return fail(err)
	}
	startLock, _, err := home.Lock(layout.StartLock(), true)
	if err != nil {
		return fail(err)
	}
	daemonLock, held, err := home.Lock(layout.DaemonLock(), false)
	if err != nil || held {
		startLock.Close()
		if err != nil {
			return fail(err)
		}
		writeReady(opts.Ready, launch.Ready{Status: launch.StatusAlreadyRunning})
		return nil, ErrAlreadyRunning
	}
	d = &Daemon{
		opts: opts, layout: layout, daemonLock: daemonLock, startLock: startLock,
		methods: map[string]MethodFunc{}, conns: map[*jsonrpc.Conn]struct{}{}, done: make(chan struct{}),
		powerStop: make(chan struct{}), powerDone: make(chan struct{}),
	}
	d.caffeine = &caffeine{assert: opts.Assert, holders: map[*client]bool{}}
	if d.caffeine.assert == nil {
		d.caffeine.assert = power.Assert
	}
	defer func() {
		if err != nil {
			d.release()
			writeReady(opts.Ready, launch.Ready{Status: launch.StatusError, Error: err.Error()})
			d = nil
		}
	}()

	if d.logs, err = logs.Open(logs.Options{Dir: layout.LogDir(), MaxBytes: opts.LogMaxBytes, Echo: opts.Echo}); err != nil {
		return d, err
	}
	d.log = slog.New(logHandler{hub: d.logs})
	host := config.ThisMac()
	if opts.Host != nil {
		host = *opts.Host
	}
	d.cfg, err = config.Load(layout.ConfigFile(), config.Keys(host), func(format string, args ...any) {
		d.log.Warn(fmt.Sprintf(format, args...))
	})
	if err != nil {
		return d, err
	}
	warnings := append(append([]string{}, d.cfg.Warnings()...), opts.Warnings...)
	if len(warnings) == 0 {
		warnings = nil
	}
	stopStands := false
	if !opts.Autostart && !opts.Login {
		_, err := os.Stat(api.StopMarkPath(layout.Home))
		stopStands = err == nil
	}
	d.hub = NewHub(api.Status{
		State: api.StateStopped, Engine: api.EngineStopped, StopRequested: stopStands,
		Fake: opts.Launcher.Fake(), Warnings: warnings,
	})
	os.Remove(api.PauseMarkPath(layout.Home))
	d.hub.paused = func(paused bool) {
		d.pauseMark(paused)
		for _, m := range opts.Modules {
			if ps, ok := m.(PauseSink); ok {
				ps.Paused(paused)
			}
		}
	}
	timing := DefaultTiming()
	if opts.Timing != nil {
		timing = opts.Timing.filled()
	}
	d.sup = &Supervisor{
		hub: d.hub, logs: d.logs, cfg: d.cfg, launcher: opts.Launcher, timing: timing, log: d.log,
		traceLog: layout.BootTraceLog(), promised: map[string]bool{}, vmmChanged: d.vmmChanged,
		baseWarnings: warnings, traceMax: logs.DefaultMaxBytes, kernelRelease: opts.KernelRelease,
	}
	if opts.LogMaxBytes > 0 {
		d.sup.traceMax = opts.LogMaxBytes
	}
	d.sup.grantee = responsibleSelf()
	d.log.Info("macOS holds this program responsible for fev's access to protected files",
		"pid", d.sup.grantee.PID, "path", d.sup.grantee.Path, "app", d.sup.grantee.Name, "self", d.sup.grantee.Self)
	d.sup.denied = func(folder string) {
		for _, m := range opts.Modules {
			if sink, ok := m.(DenialSink); ok {
				sink.Denied(folder)
			}
		}
	}
	d.sup.granted = func(folder string) {
		for _, m := range opts.Modules {
			if sink, ok := m.(GrantSink); ok {
				sink.Granted(folder)
			}
		}
	}
	d.sup.kernelLine = func(boot, text string) {
		for _, m := range opts.Modules {
			if sink, ok := m.(KernelSink); ok {
				sink.KernelLine(boot, text)
			}
		}
	}
	d.sup.space = func(sp vmmctl.Space) {
		for _, m := range opts.Modules {
			if sink, ok := m.(SpaceSink); ok {
				sink.Space(sp)
			}
		}
	}
	if opts.Autostart {
		d.sup.firstOrigin, d.sup.firstOriginNS = "daemon", opts.MainNS
		if opts.OriginNS != 0 {
			d.sup.firstOrigin, d.sup.firstOriginNS = "client", opts.OriginNS
		}
		if opts.MainNS == 0 {
			d.sup.firstOriginNS = boottrace.Now()
		} else {
			d.sup.early = map[string]uint64{boottrace.DaemonMain: opts.MainNS}
		}
	}
	d.sup.asleep.Store(int64(asleepNow()))
	if opts.Login {
		d.clearStopMark()
	}
	d.applyLoginItem()
	d.info = home.Info{PID: os.Getpid(), Build: version.Build, APIVersion: api.APIVersion, Started: stamp(time.Now())}
	if err = layout.WriteInfo(d.info); err != nil {
		return d, err
	}

	os.Remove(layout.Socket())
	if d.listener, err = net.Listen("unix", layout.Socket()); err != nil {
		return d, err
	}
	if err = os.Chmod(layout.Socket(), 0o600); err != nil {
		return d, err
	}
	d.sup.Mark(boottrace.DaemonAPIListening)

	d.events = newModuleQueue(opts.Modules)
	d.hub.mu.Lock()
	d.hub.tap = d.events.push
	d.hub.mu.Unlock()
	for i, m := range opts.Modules {
		for _, mark := range m.Marks() {
			d.sup.promised[mark] = true
		}
		if p, ok := m.(Preparer); ok {
			d.sup.before = append(d.sup.before, p.BeforeBoot)
		}
		if p, ok := m.(Stopper); ok {
			d.sup.beforeStop = append(d.sup.beforeStop, p.BeforeStop)
		}
		if p, ok := m.(Ender); ok {
			d.sup.ended = append(d.sup.ended, p.VMEnded)
		}
		if q, ok := m.(Quieter); ok {
			d.sup.quiet = append(d.sup.quiet, q)
		}
		if err = m.Start(moduleHost{d}); err != nil {
			for _, started := range opts.Modules[:i] {
				started.Close()
			}
			return d, fmt.Errorf("%s: %w", m.Name(), err)
		}
	}
	go d.events.run()
	if opts.PowerInject != "" {
		if d.power, err = power.InjectOn(opts.PowerInject); err != nil {
			d.events.stop()
			for _, started := range opts.Modules {
				started.Close()
			}
			return d, fmt.Errorf("power injection: %w", err)
		}
	} else {
		d.power = opts.Power
	}
	if d.power != nil {
		go d.powerLoop(d.power)
	} else {
		close(d.powerDone)
	}
	d.mu.Lock()
	d.startLock.Close()
	d.startLock = nil
	d.mu.Unlock()
	go d.accept()

	d.log.Info("fevd is ready", "build", version.Build, "pid", os.Getpid(), "home", layout.Home, "fake_vm", opts.Launcher.Fake())
	writeReady(opts.Ready, launch.Ready{Status: launch.StatusReady, PID: os.Getpid(), Build: version.Build})
	if opts.Autostart {
		if opts.OnDemand && d.stopStands() {
			d.log.Info("a stop was asked for while this daemon was starting for a docker command; starting nothing and stopping")
			d.sup.StandStopped()
			go d.shutdown(false, true)
		} else {
			d.sup.Start()
		}
	}
	return d, nil
}

func (d *Daemon) vmmChanged(pid int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.info.VMMPID = pid
	d.layout.WriteInfo(d.info)
}

func (d *Daemon) Done() <-chan struct{} { return d.done }

func (d *Daemon) Shutdown(force bool) { d.shutdown(force, false) }

func (d *Daemon) ShutdownAsked(force bool) { d.shutdown(force, true) }

func (d *Daemon) shutdown(force, asked bool) {
	d.shutMu.Lock()
	if d.stopping {
		d.shutMu.Unlock()
		if force {
			d.sup.Stop(true, 0, false)
		}
		<-d.done
		return
	}
	d.stopping, d.stopAsked = true, asked
	d.shutMu.Unlock()

	lock, _, err := home.Lock(d.layout.StartLock(), true)
	if err != nil {
		d.log.Error("cannot take the start lock for shutdown", "err", err)
	}
	d.mu.Lock()
	d.startLock = lock
	d.mu.Unlock()
	d.shuttingDown.Store(true)
	d.finish(force, asked)
}

type shutdownStep int

const (
	shutdownBegun shutdownStep = iota
	shutdownUnderWay
	shutdownSuperseded
)

func (d *Daemon) beginIfStopStands(began uint64, asked bool) shutdownStep {
	d.shutMu.Lock()
	defer d.shutMu.Unlock()
	if d.stopping {
		return shutdownUnderWay
	}
	lock, _, err := home.Lock(d.layout.StartLock(), true)
	if err != nil {
		d.log.Error("cannot take the start lock for shutdown", "err", err)
	}
	d.askMu.Lock()
	stands := !d.startAskedSince(began)
	if stands {
		d.stopping, d.stopAsked = true, asked
		d.sup.Refuse()
	}
	d.askMu.Unlock()
	if !stands {
		if lock != nil {
			lock.Close()
		}
		return shutdownSuperseded
	}
	d.mu.Lock()
	d.startLock = lock
	d.mu.Unlock()
	d.shuttingDown.Store(true)
	return shutdownBegun
}

func (d *Daemon) finish(force, asked bool) {
	d.log.Info("fevd is shutting down", "force", force)
	d.listener.Close()
	close(d.powerStop)
	if d.power != nil {
		d.power.Close()
	}
	d.sup.Shutdown(force, asked)
	d.caffeine.releaseAll()
	d.events.stop()
	for i := len(d.opts.Modules) - 1; i >= 0; i-- {
		d.opts.Modules[i].Close()
	}
	os.Remove(d.layout.InfoFile())
	d.log.Info("fevd has stopped")
	d.logs.Close()
	d.hub.Finish()
	flushed := make(chan struct{})
	go func() {
		d.pumps.Wait()
		close(flushed)
	}()
	select {
	case <-flushed:
	case <-time.After(time.Second):
	}
	d.mu.Lock()
	for c := range d.conns {
		c.Close()
	}
	d.mu.Unlock()
	close(d.done)
}

func (d *Daemon) release() {
	if d.listener != nil {
		d.listener.Close()
	}
	if d.logs != nil {
		d.logs.Close()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.daemonLock != nil {
		d.daemonLock.Close()
		d.daemonLock = nil
	}
	if d.startLock != nil {
		d.startLock.Close()
		d.startLock = nil
	}
}

func (d *Daemon) Close() {
	d.Shutdown(false)
	d.release()
}

func (d *Daemon) accept() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			return
		}
		d.serve(conn)
	}
}

type moduleQueue struct {
	modules []Module
	mu      sync.Mutex
	queue   []any
	wake    chan struct{}
	done    chan struct{}
}

type stopQueue struct{}

func newModuleQueue(modules []Module) *moduleQueue {
	return &moduleQueue{modules: modules, wake: make(chan struct{}, 1), done: make(chan struct{})}
}

func (q *moduleQueue) push(event any) {
	q.mu.Lock()
	q.queue = append(q.queue, event)
	q.mu.Unlock()
	wake(q.wake)
}

func (q *moduleQueue) run() {
	defer close(q.done)
	for range q.wake {
		for {
			q.mu.Lock()
			if len(q.queue) == 0 {
				q.mu.Unlock()
				break
			}
			event := q.queue[0]
			q.queue = q.queue[1:]
			q.mu.Unlock()
			switch event := event.(type) {
			case stopQueue:
				return
			case api.Status:
				for _, m := range q.modules {
					m.StateChanged(event)
				}
			case api.ConfigChange:
				for _, m := range q.modules {
					m.ConfigChanged(event)
				}
			case api.DockerEvent, api.MachinesChanged:
				for _, m := range q.modules {
					if sink, ok := m.(EventSink); ok {
						sink.Event(event)
					}
				}
			}
		}
	}
}

func (q *moduleQueue) stop() {
	q.push(stopQueue{})
	<-q.done
}

type moduleHost struct{ d *Daemon }

func (h moduleHost) Layout() home.Layout   { return h.d.layout }
func (h moduleHost) Log() *slog.Logger     { return h.d.log }
func (h moduleHost) Config() *config.Store { return h.d.cfg }
func (h moduleHost) Fake() bool            { return h.d.opts.Launcher.Fake() }
func (h moduleHost) Status() api.Status    { return h.d.hub.Status() }
func (h moduleHost) VM() (VM, bool)        { return h.d.sup.VM() }
func (h moduleHost) Mark(name string)      { h.d.sup.Mark(name) }
func (h moduleHost) Handle(method string, fn MethodFunc) {
	if method == api.MethodFilesStatus {
		share := fn
		fn = func(ctx context.Context, params json.RawMessage) (any, error) {
			r, err := share(ctx, params)
			if st, ok := r.(api.FilesStatus); ok && err == nil {
				return h.d.joinFiles(st), nil
			}
			return r, err
		}
	}
	h.d.methods[method] = fn
}
func (h moduleHost) PublishForward(change api.ForwardsChanged) {
	h.d.hub.Publish(change)
}
func (h moduleHost) PublishDocker(event api.DockerEvent) { h.d.hub.Publish(event) }
func (h moduleHost) WorkEnded() {
	for _, m := range h.d.opts.Modules {
		if w, ok := m.(WorkEnder); ok {
			w.WorkEnded()
		}
	}
}
func (h moduleHost) PublishFiles(status api.FilesStatus) {
	h.d.filesMu.Lock()
	last := status
	h.d.lastFiles = &last
	h.d.filesMu.Unlock()
	h.d.hub.Publish(api.FilesChanged{FilesStatus: h.d.joinFiles(status)})
}
func (h moduleHost) StartVM() error { return h.d.sup.Start() }

func (h moduleHost) StartOnDemand() error {
	if h.d.stopStands() {
		h.d.log.Info("a stop was asked for (its mark is in the home); a docker command starts nothing")
		h.d.sup.StandStopped()
		return nil
	}
	return h.d.sup.Start()
}

func (h moduleHost) StartAsked() error {
	if err := h.d.startAsked(); err != nil {
		return err
	}
	h.d.rouse(true)
	return nil
}

func (h moduleHost) WaitStatus(ctx context.Context, until func(api.Status) bool) (api.Status, error) {
	return h.d.hub.Wait(ctx, until)
}

func (h moduleHost) PublishMachine(change api.MachinesChanged) { h.d.hub.Publish(change) }
func (h moduleHost) PublishOperation(op api.Operation) {
	h.d.hub.Publish(api.OperationProgress{Operation: op})
}

func (h moduleHost) CheckAlive() { h.d.sup.CheckAlive() }

func (h moduleHost) Warn(key, text string) { h.d.sup.Warn(key, text) }

type statusUpdater interface {
	UpdateStatus(change func(*api.Status))
}

func (h moduleHost) UpdateStatus(change func(*api.Status)) { h.d.hub.Update(change) }

func (h moduleHost) SetConfigStarting(values map[string]any) ([]api.ConfigChange, error) {
	r, err := h.d.changeConfig("fevd", func() ([]api.ConfigChange, error) { return h.d.cfg.SetStarting(values) })
	if err != nil {
		return nil, err
	}
	return r.(api.ConfigChanges).Changed, nil
}

func updateStatus(h Host, change func(*api.Status)) {
	if u, ok := h.(statusUpdater); ok {
		u.UpdateStatus(change)
	}
}

func (h moduleHost) Rouse(asked bool) bool { return h.d.rouse(asked) }

func (h moduleHost) StopVM(ctx context.Context) error {
	boot := h.d.sup.Stop(false, 0, false)
	_, err := h.d.hub.Wait(ctx, func(st api.Status) bool {
		return st.State == api.StateStopped || (boot != "" && st.BootID != boot)
	})
	return err
}

func (d *Daemon) applyLoginItem() {
	on := d.cfg.Bool(api.KeyStartAtLogin)
	if d.opts.LoginItem == nil {
		if on {
			d.log.Warn("start_at_login is on and this daemon manages no LaunchAgent (it has a home of its own or a stand-in VM); nothing is written")
		}
		return
	}
	if err := d.opts.LoginItem.Apply(on); err != nil {
		d.log.Error("start_at_login: cannot bring the LaunchAgent up to date", "path", d.opts.LoginItem.Path(), "err", err)
		return
	}
	d.log.Debug("start_at_login: the LaunchAgent is up to date", "path", d.opts.LoginItem.Path(), "on", on)
}

func (d *Daemon) joinFiles(st api.FilesStatus) api.FilesStatus {
	d.filesMu.Lock()
	view := d.filesView
	d.filesMu.Unlock()
	if view != nil {
		st.View = view()
	}
	return st
}

func (d *Daemon) setFilesView(view func() *api.FilesView) {
	d.filesMu.Lock()
	d.filesView = view
	d.filesMu.Unlock()
}

func (d *Daemon) publishView() {
	d.filesMu.Lock()
	st := api.FilesStatus{Watch: api.FilesWatch{Paths: []string{}}}
	if d.lastFiles != nil {
		st = *d.lastFiles
	}
	d.filesMu.Unlock()
	d.hub.Publish(api.FilesChanged{FilesStatus: d.joinFiles(st)})
}
