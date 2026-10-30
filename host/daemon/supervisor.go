package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"fev/host/daemon/boottrace"
	"fev/host/daemon/config"
	"fev/host/daemon/logs"
	"fev/host/daemon/responsible"
	"fev/host/daemon/vm"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/fsevents"
	"fev/proto/version"
	"fev/proto/vmmctl"
)

type Timing struct {
	StartDeadline  time.Duration
	HelloTimeout   time.Duration
	LifelineGrace  time.Duration
	Backoff        []time.Duration
	MaxBackoff     time.Duration
	MaxFailures    int
	StableAfter    time.Duration
	EngineDeadline time.Duration
	PauseBound     time.Duration
	SyncBound      time.Duration
	ResumeBound    time.Duration
	ClockBound     time.Duration
	AliveBound     time.Duration
	HeldStopBound  time.Duration
}

func (t Timing) filled() Timing {
	d := DefaultTiming()
	for _, f := range []struct{ have, def *time.Duration }{
		{&t.EngineDeadline, &d.EngineDeadline}, {&t.PauseBound, &d.PauseBound}, {&t.ResumeBound, &d.ResumeBound},
		{&t.SyncBound, &d.SyncBound},
		{&t.ClockBound, &d.ClockBound}, {&t.AliveBound, &d.AliveBound}, {&t.HeldStopBound, &d.HeldStopBound},
	} {
		if *f.have == 0 {
			*f.have = *f.def
		}
	}
	return t
}

func (t Timing) Capped(d time.Duration) Timing {
	t = t.filled()
	for _, bound := range []*time.Duration{
		&t.StartDeadline, &t.HelloTimeout, &t.EngineDeadline, &t.PauseBound, &t.SyncBound, &t.ResumeBound, &t.ClockBound, &t.AliveBound,
	} {
		*bound = min(*bound, d)
	}
	return t
}

func DefaultTiming() Timing {
	return Timing{
		StartDeadline: 30 * time.Second,
		HelloTimeout:  10 * time.Second,
		LifelineGrace: 2 * time.Second,
		Backoff:       []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second},
		MaxBackoff:    30 * time.Second,
		MaxFailures:   5,
		StableAfter:   60 * time.Second,

		EngineDeadline: 60 * time.Second,
		PauseBound:     api.PauseBoundMS * time.Millisecond,
		SyncBound:      api.PauseBoundMS * time.Millisecond / 2,
		ResumeBound:    10 * time.Second,
		ClockBound:     5 * time.Second,
		AliveBound:     time.Second,
		HeldStopBound:  3 * time.Second,
	}
}

const MaxStopTimeoutMS = agent.MaxShutdownDeadlineMS

const tailBytes = 64 << 10

type Supervisor struct {
	hub           *Hub
	logs          *logs.Hub
	cfg           *config.Store
	launcher      vm.Launcher
	timing        Timing
	log           *slog.Logger
	traceLog      string
	traceMax      int64
	kernelRelease string
	promised      map[string]bool
	vmmChanged    func(pid int)
	before        []func() error
	beforeStop    []func(boot string)
	ended         []func(boot string)
	quiet         []Quieter
	baseWarnings  []string
	denied        func(path string)
	grantee       responsible.Who
	granted       func(path string)
	space         func(sp vmmctl.Space)
	kernelLine    func(boot, text string)
	heldMu        sync.Mutex
	held          map[string]string

	mu            sync.Mutex
	cur           *boot
	early         map[string]uint64
	firstOrigin   string
	firstOriginNS uint64
	failures      int
	retry         *time.Timer
	restart       bool
	lastTrace     *api.Trace
	shuttingDown  bool
	asleep        atomic.Int64
	verbose       bool
	wakeMu        sync.Mutex
	wakeBegun     bool
	waking        chan struct{}
}

type boot struct {
	s      *Supervisor
	id     string
	rec    *boottrace.Recorder
	inst   vm.Instance
	ctx    context.Context
	cancel context.CancelFunc

	sawBuild      bool
	agentConn     bool
	agent         *agent.Client
	windows       *api.Windows
	agentLogs     int
	seq           uint64
	traceWritten  bool
	runningSince  time.Time
	reason, fatal string
	deniedPaths   []string
	outcome       string
	killed        bool
	deadline      *time.Timer
	stopTimer     *time.Timer
	stopBy        time.Time
	killTimer     *time.Timer
	engineTimer   *time.Timer
	engineLate    string
	engineErr     atomic.Pointer[string]
	engineOff     bool
	ctl           *vmmctl.Client
	pause         *pauseCall
	ctlTurn       chan struct{}
	alive         chan struct{}
}

type pauseCall struct {
	done      chan struct{}
	err       error
	took      time.Duration
	overtaken bool
}

const (
	agentLogsPending = iota
	agentLogsAttached
	agentLogsGone
)

var bootSeq atomic.Uint64

func newBootID() string {
	var b [3]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

var ErrShuttingDown = errors.New("the daemon is shutting down")

func (s *Supervisor) Mark(name string) {
	now := boottrace.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		if s.early == nil {
			s.early = map[string]uint64{}
		}
		if _, ok := s.early[name]; !ok {
			s.early[name] = now
		}
		return
	}
	s.cur.rec.MarkAt(name, now)
	s.cur.writeTrace(false)
}

func (s *Supervisor) Trace() (api.Trace, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil && !s.cur.traceWritten {
		return s.cur.rec.Trace(), true
	}
	if s.lastTrace != nil {
		return *s.lastTrace, true
	}
	return api.Trace{}, false
}

func (s *Supervisor) VM() (VM, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil || s.cur.agent == nil {
		return nil, false
	}
	return runningVM{s.cur.inst, s.cur.agent, s.cur.id}, true
}

type runningVM struct {
	inst  vm.Instance
	agent *agent.Client
	id    string
}

func (v runningVM) VsockPath(port uint32) string { return v.inst.VsockPath(port) }
func (v runningVM) Agent() *agent.Client         { return v.agent }
func (v runningVM) BootID() string               { return v.id }
func (v runningVM) Control() *vmmctl.Client      { return v.inst.Control() }

func (s *Supervisor) standingLocked() []string {
	out := append([]string{}, s.baseWarnings...)
	keys := make([]string, 0, len(s.held))
	for key := range s.held {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, s.held[key])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Supervisor) updateStanding(change func(*api.Status)) {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	s.hub.Update(change)
}

func (s *Supervisor) writesHeld() bool { return s.warned(WarnWritesHeld) }

func (s *Supervisor) warned(key string) bool {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	return s.held[key] != ""
}

func (s *Supervisor) Warn(key, text string) {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()
	if s.held[key] == text {
		return
	}
	standing := map[string]bool{}
	for _, w := range s.standingLocked() {
		standing[w] = true
	}
	if s.held == nil {
		s.held = map[string]string{}
	}
	if text == "" {
		delete(s.held, key)
	} else {
		s.held[key] = text
	}
	next := s.standingLocked()
	for _, w := range next {
		standing[w] = true
	}
	s.hub.Update(func(st *api.Status) {
		out := append([]string{}, next...)
		for _, w := range st.Warnings {
			if !standing[w] {
				out = append(out, w)
			}
		}
		if len(out) == 0 {
			out = nil
		}
		st.Warnings = out
	})
}

func (s *Supervisor) Refresh() {
	s.hub.Update(func(st *api.Status) { st.PendingRestart = s.cfg.PendingRestart() })
}

func (s *Supervisor) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shuttingDown {
		return ErrShuttingDown
	}
	switch s.hub.Status().State {
	case api.StateStopped:
		s.failures = 0
		origin, ns := "daemon", boottrace.Now()
		if s.firstOrigin != "" {
			origin, ns = s.firstOrigin, s.firstOriginNS
		}
		s.bootLocked(origin, ns)
	case api.StateStopping:
		s.restart = true
		s.hub.Update(func(st *api.Status) { st.StopRequested = false })
	}
	return nil
}

func (s *Supervisor) bootLocked(origin string, originNS uint64) {
	cpus, mem := s.cfg.VMStarted()
	ctx, cancel := context.WithCancel(context.Background())
	b := &boot{s: s, id: newBootID(), ctx: ctx, cancel: cancel, seq: bootSeq.Add(1), ctlTurn: make(chan struct{}, 1),
		engineOff: !s.cfg.Bool(api.KeyDockerEnabled)}
	b.rec = boottrace.New(b.id, version.Build, origin, originNS, cpus, mem)
	if s.firstOrigin != "" {
		for name, ns := range s.early {
			b.rec.MarkAt(name, ns)
		}
	}
	s.firstOrigin, s.early = "", nil
	s.cur = b
	s.updateStanding(func(st *api.Status) {
		st.State, st.Phase, st.RetryInMS = api.StateStarting, api.PhaseVMM, 0
		st.Engine, st.BootID, st.Error = api.EngineStopped, b.id, ""
		st.PendingRestart = s.cfg.PendingRestart()
		st.Warnings = s.standingLocked()
	})
	s.log.Info("starting the VM", "boot", b.id, "cpus", cpus, "memory_mib", mem)
	var inst vm.Instance
	var err error
	for _, prepare := range s.before {
		if err = prepare(); err != nil {
			break
		}
	}
	if err == nil {
		share, _ := s.cfg.InEffect(api.KeyFilesShare)
		node, _ := s.cfg.InEffect(api.KeyDockerNodeName)
		nodeName, _ := node.(string)
		if s.verbose {
			s.log.Info("the last start failed while the kernel was booting; this one boots it without quiet, so that the console says why", "boot", b.id)
		}
		inst, err = s.launcher.Start(vm.Spec{BootID: b.id, CPUs: cpus, MemMiB: mem, Share: share == true, Verbose: s.verbose, EngineOff: b.engineOff, NodeName: nodeName,
			K8s: s.cfg.Bool(api.KeyKubernetesEnabled)}, b)
	}
	if err != nil {
		b.reason = err.Error()
		s.endedLocked(b, nil)
		return
	}
	b.rec.Mark(boottrace.VMMSpawn)
	b.inst = inst
	b.ctl = inst.Control()
	if b.ctl != nil && s.space != nil {
		b.ctl.OnSpace(s.space)
	}
	if s.vmmChanged != nil {
		s.vmmChanged(inst.PID())
	}
	b.deadline = time.AfterFunc(s.timing.StartDeadline, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cur == b && s.hub.Status().State == api.StateStarting {
			b.fail(fmt.Sprintf("the VM was not ready %v after its VMM started", s.timing.StartDeadline))
		}
	})
}

func (b *boot) fail(reason string) {
	if b.reason == "" {
		b.reason = reason
	}
	b.s.log.Error("giving up on this boot", "boot", b.id, "reason", reason)
	b.halt()
}

func (b *boot) halt() {
	if b.inst == nil || b.killTimer != nil {
		return
	}
	b.inst.Stop()
	inst := b.inst
	b.killTimer = time.AfterFunc(b.s.timing.LifelineGrace, func() {
		b.s.log.Warn("the VMM did not stop when its lifeline closed; killing it", "boot", b.id)
		b.s.mu.Lock()
		b.killed = true
		b.s.mu.Unlock()
		inst.Kill()
	})
}

func (b *boot) Mark(m vm.Mark) {
	s := b.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != b {
		return
	}
	if m.Name == fsevents.MarkDenied {
		b.deniedFolder(m.Path)
		return
	}
	if m.Name == fsevents.MarkGranted {
		b.grantedFolder(m.Path)
		return
	}
	b.rec.MarkAt(m.Name, m.NS)
	if m.Outcome != "" {
		b.outcome = m.Outcome
	}
	if !b.sawBuild {
		b.sawBuild = true
		if err := version.Check(version.Build, m.Build, "VMM"); err != nil {
			b.fatal = err.Error()
			b.fail(b.fatal)
			return
		}
	}
	if m.Name == boottrace.VMMGuestEntry && b.reason == "" {
		s.hub.Update(func(st *api.Status) {
			if st.State == api.StateStarting && st.Phase == api.PhaseVMM {
				st.Phase = api.PhaseKernel
			}
		})
	}
}

const maxDenied = 32

func DeniedWarning(folder string, who responsible.Who) string {
	return "macOS did not let the VM read " + folder + ": containers get \"Operation not permitted\" there. " + who.Grant()
}

func (b *boot) deniedFolder(folder string) {
	s := b.s
	if !filepath.IsAbs(folder) || filepath.Clean(folder) != folder || len(folder) > 1024 || !utf8.ValidString(folder) {
		return
	}
	for _, have := range b.deniedPaths {
		if have == folder {
			return
		}
	}
	if len(b.deniedPaths) >= maxDenied {
		return
	}
	b.deniedPaths = append(b.deniedPaths, folder)
	s.log.Warn("macOS privacy protection refused the VM a folder", "boot", b.id, "folder", folder)
	s.hub.Update(func(st *api.Status) {
		st.Warnings = append(append([]string{}, st.Warnings...), DeniedWarning(folder, s.grantee))
	})
	if s.denied != nil {
		s.denied(folder)
	}
}

func (b *boot) grantedFolder(folder string) {
	s := b.s
	at := -1
	for i, have := range b.deniedPaths {
		if have == folder {
			at = i
		}
	}
	if at < 0 {
		return
	}
	b.deniedPaths = append(append([]string{}, b.deniedPaths[:at]...), b.deniedPaths[at+1:]...)
	s.log.Info("macOS now lets the VM read a folder it had refused", "boot", b.id, "folder", folder)
	warning := DeniedWarning(folder, s.grantee)
	s.hub.Update(func(st *api.Status) {
		var keep []string
		for _, w := range st.Warnings {
			if w != warning {
				keep = append(keep, w)
			}
		}
		st.Warnings = keep
	})
	if s.granted != nil {
		s.granted(folder)
	}
}

func (b *boot) VMMLine(text string) {
	b.s.logs.Write(logs.Entry{Source: api.LogVMM, Level: 6, Text: text})
}

func isInitLine(text string) bool {
	if strings.HasPrefix(text, "[") {
		if end := strings.Index(text, "] "); end > 0 {
			text = text[end+2:]
		}
	}
	return strings.HasPrefix(text, "fev-init: ")
}

func (b *boot) ConsoleLine(text string) {
	s := b.s
	e := logs.Entry{Source: api.LogConsole, Level: 6, Text: text}
	if !isInitLine(text) {
		s.logs.Write(e)
		return
	}
	s.mu.Lock()
	gone := b.agentLogs == agentLogsGone
	s.mu.Unlock()
	if gone {
		s.logs.Write(e)
	} else {
		s.logs.WriteHeld(e, b.seq)
	}
}

func (b *boot) AgentConn(conn net.Conn) {
	accepted := boottrace.Now()
	s := b.s
	s.mu.Lock()
	if s.cur != b || b.agentConn || b.reason != "" || s.hub.Status().State != api.StateStarting {
		s.mu.Unlock()
		conn.Close()
		return
	}
	b.agentConn = true
	s.mu.Unlock()
	go b.attach(conn, accepted)
}

func (b *boot) attach(conn net.Conn, accepted uint64) {
	s := b.s
	ctx, cancel := context.WithTimeout(b.ctx, s.timing.HelloTimeout)
	client, hello, err := agent.NewClient(ctx, conn, "fevd")
	cancel()
	s.mu.Lock()
	if s.cur != b || b.reason != "" {
		s.mu.Unlock()
		if client != nil {
			client.Close()
		}
		return
	}
	if err != nil {
		var mismatch *version.Mismatch
		if errors.As(err, &mismatch) {
			b.fatal = mismatch.Error()
		}
		b.fail("the guest agent's handshake failed: " + err.Error())
		s.mu.Unlock()
		return
	}
	if s.kernelRelease != "" && hello.Kernel != s.kernelRelease {
		b.fatal = fmt.Sprintf("kernel mismatch: the guest's kernel is %q and the manifest beside the Image says %q; build the kernel again (kernel/build.sh), or use the Image the manifest was written for",
			hello.Kernel, s.kernelRelease)
		b.fail(b.fatal)
		s.mu.Unlock()
		client.Close()
		return
	}
	b.agent = client
	s.verbose = false
	b.rec.MarkAt(boottrace.AgentConnected, accepted)
	s.hub.Update(func(st *api.Status) {
		if st.State == api.StateStarting {
			st.Phase = api.PhaseInit
		}
	})
	s.mu.Unlock()
	s.log.Info("the guest agent connected", "boot", b.id, "kernel", clip(hello.Kernel, 80))
	go func() {
		<-client.Done()
		b.agentGone()
	}()
	go b.setClock(client, "hello")
	go b.readWindows()

	sent := boottrace.Now()
	bt, err := client.BootTrace(b.ctx, false)
	if err == nil {
		b.rec.SetGuestClock(sent, boottrace.Now(), bt.NowMS)
	}
	if err == nil {
		_, err = client.SubscribeLogs(b.ctx, agent.LogSubscribeParams{Backlog: true}, b.guestLog)
	}
	if err == nil {
		s.mu.Lock()
		if b.agentLogs == agentLogsPending {
			b.agentLogs = agentLogsAttached
		}
		s.mu.Unlock()
		bt, err = client.BootTrace(b.ctx, true)
	}
	if err != nil {
		s.mu.Lock()
		if s.cur == b && b.ctx.Err() == nil && s.hub.Status().State != api.StateStopping {
			b.fail("the guest agent stopped answering: " + err.Error())
		}
		s.mu.Unlock()
		return
	}
	b.rec.MarkGuest(boottrace.InitMain, bt.InitStartMS, false)
	b.rec.MarkGuest(boottrace.InitReady, bt.ReadyMS, false)
	for i, stage := range bt.Stages {
		if i == 32 {
			break
		}
		b.rec.MarkGuest("init.stage."+clip(stage.Name, 40), stage.EndMS, false)
		if stage.Error != "" {
			s.log.Warn("an init stage failed", "boot", b.id, "stage", clip(stage.Name, 40), "error", clip(stage.Error, 400))
		}
	}
	s.mu.Lock()
	if s.cur != b || b.reason != "" {
		s.mu.Unlock()
		return
	}
	if b.deadline != nil {
		b.deadline.Stop()
	}
	s.hub.Update(func(st *api.Status) {
		if st.State == api.StateStarting {
			st.Phase = api.PhaseEngine
		}
	})
	enginesOff := s.engineOff()
	if !enginesOff {
		b.armEngine()
	}
	s.mu.Unlock()
	if b.engineOff && !enginesOff {
		go func() {
			if _, err := client.EngineStart(b.ctx); err != nil && b.ctx.Err() == nil {
				s.log.Warn("docker.enabled was turned on during the boot and the engine was not started", "boot", b.id, "err", err)
			}
		}()
	}
	if _, err := client.SubscribeEngine(b.ctx, b.engineState); err != nil && b.ctx.Err() == nil {
		b.engineState(agent.EngineStatus{State: agent.EngineFailed, Error: "this guest has no engine to follow: " + err.Error()})
	}
}

func (b *boot) readWindows() {
	s := b.s
	if b.ctl == nil {
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, s.timing.AliveBound)
	st, err := b.ctl.Stats(ctx)
	cancel()
	if err != nil {
		if b.ctx.Err() == nil {
			s.log.Warn("the VMM did not say which windows it has; the status will not show them", "boot", b.id, "err", err)
		}
		return
	}
	w := statusWindows(st.Windows)
	if w == nil {
		return
	}
	if d := w.DataDisk; d != nil && d.State != api.WindowOn {
		s.log.Info("the data disk has no read window; every read is a request to the VMM", "boot", b.id, "state", clip(d.State, 40), "reason", clip(d.Reason, 400))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != b || b.reason != "" {
		return
	}
	b.windows = w
	s.hub.Update(func(st *api.Status) { st.Windows = w })
}

func (b *boot) setClock(client *agent.Client, why string) (stepped bool, offsetMS float64) {
	s := b.s
	asleep := int64(asleepNow())
	ctx, cancel := context.WithTimeout(b.ctx, s.timing.ClockBound)
	defer cancel()
	r, err := client.TimeSet(ctx, time.Now().UnixNano())
	switch {
	case err == nil:
		s.asleep.Store(asleep)
		if r.Stepped {
			s.log.Info("clock: the guest's wall clock was stepped", "boot", b.id, "why", why, "offset_ms", math.Round(r.OffsetMS))
		}
		return r.Stepped, r.OffsetMS
	case agent.IsNotImplemented(err):
		s.asleep.Store(asleep)
	case b.ctx.Err() == nil:
		s.log.Warn("clock: the guest did not take the Mac's time", "boot", b.id, "why", why, "err", err)
	}
	return false, 0
}

func (b *boot) armEngine() {
	s := b.s
	if b.engineTimer != nil {
		return
	}
	b.engineTimer = time.AfterFunc(s.timing.EngineDeadline, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cur != b || b.reason != "" || b.engineTimer == nil {
			return
		}
		b.engineTimer = nil
		st := s.hub.Status()
		waiting := st.Engine != api.EngineReady && st.Engine != api.EngineFailed
		if !waiting || (st.State != api.StateRunning && st.State != api.StateStarting) {
			return
		}
		why := fmt.Sprintf("the Docker engine was not ready %v after it was started", s.timing.EngineDeadline)
		s.log.Error(why, "boot", b.id, "engine", st.Engine)
		b.engineLate = why
		text := b.engineWhy(why)
		s.hub.Update(func(st *api.Status) { st.Engine, st.Error = api.EngineFailed, text })
	})
}

func (b *boot) engineWhy(why string) string {
	if line := b.engineErr.Load(); line != nil {
		why += "; the engine's last error: " + *line
	}
	if b.s.warned(WarnRecovered) {
		why += ". The data disk was recovered when it was mounted (see the warning): if the engine does not come up on it, `fev reset` makes a new, empty disk, and every image, container and volume on the old one is lost"
	}
	return why
}

func engineFault(level int, text string) bool {
	if level <= 3 {
		return true
	}
	for _, mark := range []string{"fatal error:", "unexpected fault address", "[signal SIG", "panic:", "failed to start daemon:"} {
		if strings.Contains(text, mark) {
			return true
		}
	}
	return false
}

func (s *Supervisor) engineOff() bool { return !s.cfg.Bool(api.KeyDockerEnabled) }

func (b *boot) disarmEngine() {
	if b.engineTimer != nil {
		b.engineTimer.Stop()
		b.engineTimer = nil
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (b *boot) guestLog(e agent.LogEntry) {
	switch e.Source {
	case agent.LogKernel, agent.LogInit, agent.LogEngine:
	default:
		return
	}
	level := min(max(e.Level, 0), 7)
	if e.TimeMS < 0 || e.TimeMS > 3e12 {
		e.TimeMS = 0
	}
	entry := logs.Entry{Source: e.Source, GuestMS: e.TimeMS, Level: level, Text: e.Text}
	if at, ok := b.rec.GuestToWall(e.TimeMS); ok && e.TimeMS > 0 && !at.After(time.Now()) {
		entry.Time = at
	}
	if e.Source == agent.LogKernel && strings.Contains(e.Text, "Run /init as init process") {
		b.rec.MarkGuest(boottrace.KernelRunInit, e.TimeMS, true)
	}
	if e.Source == agent.LogKernel && b.s.kernelLine != nil {
		b.s.kernelLine(b.id, e.Text)
	}
	if e.Source == agent.LogEngine && engineFault(level, e.Text) {
		line := clip(strings.TrimSpace(e.Text), 300)
		b.engineErr.Store(&line)
	}
	b.s.logs.Write(entry)
}

func (b *boot) engineState(e agent.EngineStatus) {
	switch e.State {
	case agent.EngineStopped, agent.EngineStarting, agent.EngineReady, agent.EngineStopping, agent.EngineFailed:
	default:
		return
	}
	s := b.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != b || b.reason != "" {
		return
	}
	if e.StartedMS > 0 {
		b.rec.MarkGuest(boottrace.EngineStart, e.StartedMS, false)
	}
	if e.State == agent.EngineReady && e.ReadyMS > 0 {
		b.rec.MarkGuest(boottrace.EngineReady, e.ReadyMS, false)
	}
	switch e.State {
	case agent.EngineReady:
		b.engineLate = ""
		b.engineErr.Store(nil)
	case agent.EngineStopping, agent.EngineStopped:
		b.engineLate = ""
	}
	off := s.engineOff() && (e.State == agent.EngineStopped || e.State == agent.EngineStopping)
	switch st := s.hub.Status(); {
	case e.State == agent.EngineReady || e.State == agent.EngineFailed || st.State == api.StateStopping || off:
		b.disarmEngine()
	case b.engineLate != "":
	case st.State == api.StateRunning || st.State == api.StateStarting:
		b.armEngine()
	}
	why := ""
	switch {
	case e.State == agent.EngineFailed:
		why = b.engineWhy("the Docker engine failed: " + clip(e.Error, 400))
	case b.engineLate != "":
		why = b.engineWhy(b.engineLate)
	}
	became := false
	s.hub.Update(func(st *api.Status) {
		st.Engine = e.State
		if off && e.State == agent.EngineStopped {
			st.Engine = api.EngineOff
		}
		if st.State == api.StateStopping {
			return
		}
		st.Error = why
		if why != "" {
			st.Engine = api.EngineFailed
		}
		if st.State == api.StateStarting && st.Phase == api.PhaseEngine && (e.State == agent.EngineReady || st.Engine == api.EngineOff) {
			st.State, st.Phase = api.StateRunning, ""
			st.Windows = b.windows
			became = true
		}
	})
	if became {
		b.runningSince = time.Now()
		s.log.Info("the VM is running", "boot", b.id)
		b.writeTrace(false)
	}
}

func (b *boot) agentGone() {
	s := b.s
	s.mu.Lock()
	defer s.mu.Unlock()
	b.agentLogs = agentLogsGone
	if s.cur != b || b.ctx.Err() != nil {
		return
	}
	if s.hub.Status().State != api.StateStopping {
		b.fail("the connection to the guest agent ended")
	}
}

func (b *boot) writeTrace(final bool) {
	if b.traceWritten {
		return
	}
	s := b.s
	t := b.rec.Trace()
	if !t.Complete && !final {
		if s.hub.Status().State != api.StateRunning {
			return
		}
		have := map[string]bool{boottrace.Origin: true}
		for _, m := range t.Marks {
			have[m.Name] = true
		}
		for _, def := range boottrace.Budget {
			if !have[def.To] && s.promised[def.To] {
				return
			}
		}
	}
	b.traceWritten = true
	b.rec.End()
	s.lastTrace = &t
	if err := boottrace.AppendRotating(s.traceLog, t, s.traceMax, logs.Generations); err != nil {
		s.log.Error("cannot write the boot trace", "err", err)
	}
	for _, stage := range boottrace.Over(t) {
		s.log.Warn(fmt.Sprintf("boot stage %d (%s to %s) took %.1f ms, over its budget of %.0f ms",
			stage.Stage, stage.From, stage.To, stage.MS, stage.BudgetMS), "boot", b.id)
	}
	if t.Complete && t.TotalMS > t.BudgetMS {
		s.log.Warn(fmt.Sprintf("the boot took %.1f ms, over the budget of %.0f ms", t.TotalMS, t.BudgetMS), "boot", b.id)
	}
}

func (b *boot) Exited(exit vm.Exit) {
	s := b.s
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endedLocked(b, &exit)
}

func (b *boot) why(exit *vm.Exit) string {
	switch {
	case exit == nil:
		return "the VM could not be started: " + b.reason
	case exit.Signal != "" && !b.killed:
		return "the VMM was killed by " + exit.Signal
	case exit.Signal == "" && exit.Code != 0:
		return fmt.Sprintf("the VMM exited with status %d", exit.Code)
	case b.outcome == "power_off":
		return "the guest powered itself off"
	case b.outcome == "reset":
		return "the guest reset itself"
	case b.reason != "":
		return b.reason
	case exit.Signal != "":
		return "the VMM was killed by " + exit.Signal
	default:
		return "the VMM stopped without being asked"
	}
}

func (s *Supervisor) tail(source string) []string {
	lines := s.logs.Tail(source, logs.RingLines)
	total := 0
	for i := len(lines) - 1; i >= 0; i-- {
		total += len(lines[i])
		if total > tailBytes {
			return lines[i+1:]
		}
	}
	return lines
}

func (s *Supervisor) endedLocked(b *boot, exit *vm.Exit) {
	if s.cur != b {
		return
	}
	s.cur = nil
	b.cancel()
	for _, t := range []*time.Timer{b.deadline, b.stopTimer, b.killTimer, b.engineTimer} {
		if t != nil {
			t.Stop()
		}
	}
	if b.agent != nil {
		b.agent.Close()
	}
	if b.agentLogs == agentLogsPending {
		s.logs.Release(api.LogConsole, b.seq)
	}
	b.writeTrace(true)
	s.cfg.VMStopped()
	if s.vmmChanged != nil {
		s.vmmChanged(0)
	}
	for _, fn := range s.ended {
		fn(b.id)
	}

	asked := s.hub.Status().State == api.StateStopping
	var last *api.Exit
	if exit != nil {
		last = &api.Exit{Code: exit.Code, Signal: exit.Signal, At: stamp(exit.At), Expected: asked}
	}
	if asked {
		s.log.Info("the VM has stopped", "boot", b.id)
		if s.restart && !s.shuttingDown {
			s.restart = false
			s.failures = 0
			s.hub.Update(func(st *api.Status) { st.LastExit = last })
			s.bootLocked("daemon", boottrace.Now())
			return
		}
		s.restart = false
		s.updateStanding(func(st *api.Status) {
			st.State, st.Phase, st.RetryInMS, st.Engine, st.Error = api.StateStopped, "", 0, api.EngineStopped, ""
			st.LastExit, st.PendingRestart = last, []string{}
			st.Warnings = s.standingLocked()
		})
		return
	}

	if st := s.hub.Status(); st.State == api.StateStarting && st.Phase == api.PhaseKernel {
		s.verbose = true
	}
	reason := b.why(exit)
	if last != nil {
		last.VMMTail, last.ConsoleTail = s.tail(api.LogVMM), s.tail(api.LogConsole)
	}
	if !b.runningSince.IsZero() && time.Since(b.runningSince) >= s.timing.StableAfter {
		s.failures = 0
	}
	s.failures++
	stopped := func(why string) {
		s.hub.Update(func(st *api.Status) {
			st.State, st.Phase, st.RetryInMS, st.Engine = api.StateStopped, "", 0, api.EngineStopped
			st.Error, st.PendingRestart = why, []string{}
			if last != nil {
				st.LastExit = last
			}
		})
	}
	switch {
	case b.fatal != "":
		s.log.Error("the VM cannot be started", "boot", b.id, "reason", b.fatal)
		stopped(b.fatal)
	case s.shuttingDown:
		stopped(reason)
	case s.failures >= s.timing.MaxFailures:
		s.log.Error("the VM failed too many times in a row and is left stopped", "boot", b.id, "failures", s.failures, "reason", reason)
		stopped(fmt.Sprintf("the VM failed %d times in a row and was left stopped; the last time, %s", s.failures, reason))
	default:
		delay := s.timing.Backoff[min(s.failures, len(s.timing.Backoff))-1]
		delay = min(delay, s.timing.MaxBackoff)
		s.log.Error("the VM failed and will be started again", "boot", b.id, "reason", reason, "failures", s.failures, "retry_in", delay)
		s.hub.Update(func(st *api.Status) {
			st.State, st.Phase, st.RetryInMS, st.Engine = api.StateStarting, api.PhaseBackoff, int(delay.Milliseconds()), api.EngineStopped
			st.Error, st.PendingRestart = reason, []string{}
			if last != nil {
				st.LastExit = last
			}
		})
		s.retry = time.AfterFunc(delay, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			st := s.hub.Status()
			if s.cur == nil && st.State == api.StateStarting && st.Phase == api.PhaseBackoff {
				s.retry = nil
				s.bootLocked("daemon", boottrace.Now())
			}
		})
	}
}

func (s *Supervisor) Stop(force bool, timeout time.Duration, asked bool) (bootID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil {
		bootID = s.cur.id
	}
	s.stopLocked(force, timeout, asked)
	return bootID
}

func (s *Supervisor) stopLocked(force bool, timeout time.Duration, asked bool) {
	named := timeout > 0
	if !named {
		timeout = api.DefaultStopTimeoutMS * time.Millisecond
	}
	s.restart = false
	st := s.hub.Status()
	b := s.cur
	if asked && (st.State == api.StateStopped || st.State == api.StateStopping) {
		s.hub.Update(func(st *api.Status) { st.StopRequested = true })
	}
	switch {
	case st.State == api.StateStopped:
		return
	case b == nil:
		if s.retry != nil {
			s.retry.Stop()
			s.retry = nil
		}
		s.hub.Update(func(st *api.Status) {
			st.State, st.Phase, st.RetryInMS, st.Error = api.StateStopped, "", 0, ""
			st.StopRequested = asked
		})
		return
	case st.State == api.StateStopping:
		if force {
			b.halt()
		} else if by := time.Now().Add(timeout); b.stopTimer != nil && by.Before(b.stopBy) {
			b.stopTimer.Reset(timeout)
			b.stopBy = by
		}
		return
	}
	if b.deadline != nil {
		b.deadline.Stop()
	}
	b.disarmEngine()
	wasPaused := st.State == api.StatePaused || b.pause != nil
	if b.pause != nil {
		b.pause.overtaken = true
	}
	s.hub.Update(func(st *api.Status) {
		st.State, st.Phase, st.RetryInMS, st.Error = api.StateStopping, "", 0, ""
		st.StopRequested = asked
	})
	if force || b.agent == nil {
		s.log.Info("stopping the VM by its lifeline", "boot", b.id, "force", force)
		b.halt()
		return
	}
	if !named && timeout > s.timing.HeldStopBound && s.writesHeld() {
		timeout = s.timing.HeldStopBound
		s.log.Warn("writes are held for lack of room on the Mac: the guest cannot sync, and is given less time to shut down; what it could not write is not on the disk", "boot", b.id, "timeout", timeout)
	}
	s.log.Info("asking the guest to shut down", "boot", b.id, "timeout", timeout)
	b.stopBy = time.Now().Add(timeout)
	b.stopTimer = time.AfterFunc(timeout, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cur == b {
			s.log.Warn("the guest did not shut down in time; closing the lifeline", "boot", b.id)
			b.halt()
		}
	})
	guest := timeout
	if guest >= 2*time.Second {
		guest -= 500 * time.Millisecond
	}
	client := b.agent
	go func() {
		if wasPaused && b.ctl != nil {
			ctx, cancel := context.WithTimeout(b.ctx, s.timing.ResumeBound)
			err := b.takeTurn(ctx)
			if err == nil {
				_, err = b.ctl.Resume(ctx)
				b.giveTurn()
			}
			cancel()
			if err != nil && b.ctx.Err() == nil {
				s.log.Warn("the paused VM did not resume to be stopped; the lifeline will stop it", "boot", b.id, "err", err)
			}
		}
		for _, fn := range s.beforeStop {
			if b.ctx.Err() != nil {
				break
			}
			fn(b.id)
		}
		err := client.Shutdown(b.ctx, agent.ShutdownParams{DeadlineMS: int(guest.Milliseconds())}, func(p agent.ShutdownProgress) {
			s.log.Debug("guest shutdown", "boot", b.id, "phase", clip(p.Phase, 40), "error", clip(p.Error, 400))
		})
		if err != nil && b.ctx.Err() == nil {
			s.mu.Lock()
			if s.cur == b {
				s.log.Warn("the guest refused to shut down; closing the lifeline", "boot", b.id, "err", err)
				b.halt()
			}
			s.mu.Unlock()
		}
	}()
}

func (s *Supervisor) StandStopped() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hub.Status().State == api.StateStopped {
		s.hub.Update(func(st *api.Status) { st.StopRequested = true })
	}
}

func (s *Supervisor) Restart() error {
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return ErrShuttingDown
	}
	if s.cur != nil {
		s.stopLocked(false, 0, false)
		s.restart = true
		s.hub.Update(func(st *api.Status) { st.StopRequested = false })
		s.mu.Unlock()
		return nil
	}
	s.stopLocked(false, 0, false)
	s.mu.Unlock()
	return s.Start()
}

func (s *Supervisor) Refuse() {
	s.mu.Lock()
	s.shuttingDown = true
	s.mu.Unlock()
}

func (s *Supervisor) Shutdown(force, asked bool) {
	s.mu.Lock()
	s.shuttingDown = true
	s.stopLocked(force, 0, asked)
	s.mu.Unlock()
	s.hub.Wait(context.Background(), func(st api.Status) bool { return st.State == api.StateStopped })
}
