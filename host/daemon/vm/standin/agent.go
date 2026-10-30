package standin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	api "fev/proto/agent"
	"fev/proto/fsevents"
	"fev/proto/jsonrpc"
	"fev/proto/netplan"
	"fev/proto/relay"
)

type Agent struct {
	Build       string
	Kernel      string
	Clock       func() float64
	EngineDelay time.Duration
	EngineError string
	EngineFlap  bool
	EngineOff   bool
	OnShutdown  func(reboot bool, deadline time.Duration)
	Console     func(text string)
	Share       *Share
	Feed        string
	NoDevice    bool
	ConfigFile  string
	BudgetFail  [2]int
	budgetCalls int
	Gate        func()

	trust   trustState
	domains domainsState
	view    viewState
	emu     emulationState
	debug   debugState
	k8s     k8sState

	mu                 sync.Mutex
	ready              chan struct{}
	initStart          float64
	readyMS            float64
	stages             []api.Stage
	engine             api.EngineStatus
	engineGen          int
	firstStart         float64
	firstReady         float64
	backlog            []api.LogEntry
	nextID             uint64
	logSubs            map[uint64]*stream
	engineSubs         map[uint64]*stream
	MountRung          int
	CheckTime          time.Duration
	CheckUncorrectable uint64
	NoData             string
	budgets            []api.BudgetParams
	cache              uint64
	reclaims           int

	watching      int
	watches       int
	kept, running string
	shareSent     bool
	SyncDelay     time.Duration
	StatsFirst    time.Duration
	syncs         int
	bound         map[string]bool
	statSubs      map[uint64]*statSub
	behind        time.Duration
	clockSets     int
	host          *jsonrpc.Conn
	streams       map[string]chan streamAnswer
	mtable        *machineTable
	sessions      map[uint64]*fakeSession
	CreateTime    time.Duration
	CreateFail    string
	StartingTime  time.Duration
	Containers    func() []StatsName
	Volumes       VolumeEngine
}

type StatsName struct{ ID, Name string }

type Share struct {
	Root string
	Deny []string
}

type streamAnswer struct {
	conn    net.Conn
	refused string
	done    chan struct{}
}

func NewAgent(build string, clock func() float64) *Agent {
	return &Agent{
		Build: build, Kernel: "7.2.0-fev-standin", Clock: clock,
		ready:   make(chan struct{}),
		logSubs: map[uint64]*stream{}, engineSubs: map[uint64]*stream{}, sessions: map[uint64]*fakeSession{},
		statSubs: map[uint64]*statSub{},
		streams:  map[string]chan streamAnswer{},
		engine:   api.EngineStatus{State: api.EngineStopped},
	}
}

type stream struct {
	conn    *jsonrpc.Conn
	sources map[string]bool
	ch      chan func() error
}

func newStream(ctx context.Context, conn *jsonrpc.Conn) *stream {
	s := &stream{conn: conn, ch: make(chan func() error, 4096)}
	replied := make(chan struct{})
	if !jsonrpc.AfterReply(ctx, func() { close(replied) }) {
		close(replied)
	}
	go func() {
		select {
		case <-replied:
		case <-conn.Done():
			return
		}
		for {
			select {
			case send := <-s.ch:
				if send() != nil {
					return
				}
			case <-conn.Done():
				return
			}
		}
	}()
	return s
}

func (s *stream) send(method string, params any) {
	select {
	case s.ch <- func() error { return s.conn.Notify(method, params) }:
	default:
	}
}

func (a *Agent) Logf(source string, level int, format string, args ...any) {
	a.logAt(a.Clock(), source, level, fmt.Sprintf(format, args...))
}

func (a *Agent) logAt(timeMS float64, source string, level int, text string) {
	a.mu.Lock()
	e := api.LogEntry{Source: source, TimeMS: timeMS, Level: level, Text: text}
	a.backlog = append(a.backlog, e)
	if len(a.backlog) > 512 {
		a.backlog = a.backlog[1:]
	}
	for id, s := range a.logSubs {
		if s.sources[source] {
			e.Subscription = id
			s.send(api.NotifyLogEntry, e)
		}
	}
	console := a.Console
	a.mu.Unlock()
	if source == api.LogInit && console != nil {
		console(text)
	}
}

func (a *Agent) Boot(initStart float64) {
	now := a.Clock()
	a.mu.Lock()
	a.initStart, a.readyMS = initStart, now
	step := (now - initStart) / 5
	for i, name := range []string{"mounts", "agent", "cgroups", "data-disk", "network"} {
		a.stages = append(a.stages, api.Stage{Name: name, StartMS: initStart + float64(i)*step, EndMS: initStart + float64(i+1)*step})
	}
	a.mu.Unlock()
	a.logAt(0, api.LogKernel, 6, "Linux version 7.2.0-fev-standin")
	a.logAt(initStart*0.9, api.LogKernel, 6, "Run /init as init process")
	a.Logf(api.LogInit, 6, "init: ready at %.1f ms, %.1f ms after reaching main", now, now-initStart)
	close(a.ready)
	a.bindShare(true)
	if a.EngineOff {
		a.Logf(api.LogInit, 6, "init: stage engine skipped: the Docker engine is off (%s=0)", api.EngineCmdlineParam)
		return
	}
	a.startEngine()
}

func (a *Agent) bindShare(engineStart bool) (bound, unbound []string) {
	bound, unbound = []string{}, []string{}
	if a.Share == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if engineStart || a.bound == nil {
		a.bound = map[string]bool{}
	}
	for _, sp := range api.SharePaths {
		if a.bound[sp.Target] {
			continue
		}
		st, err := os.Lstat(filepath.Join(a.Share.Root, sp.Source))
		if err != nil || !st.IsDir() || (!engineStart && sp.Target == "/tmp") {
			unbound = append(unbound, sp.Target)
			continue
		}
		a.bound[sp.Target] = true
		bound = append(bound, sp.Target)
	}
	return
}

func (a *Agent) Syncs() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.syncs
}

func (a *Agent) SetFeed(state string) {
	a.mu.Lock()
	a.Feed = state
	a.mu.Unlock()
	a.sendShare()
}

func (a *Agent) shareNow() *api.FileShare {
	if a.Share == nil {
		return nil
	}
	fs := &api.FileShare{Tag: "mac", Target: "/mnt/mac", Device: !a.NoDevice, Feed: a.feed(), Watching: a.watching}
	if fs.Feed != api.FeedConnected {
		fs.FeedError = "the stand-in was told so"
	}
	if a.bound != nil {
		for _, sp := range api.SharePaths {
			if !a.bound[sp.Target] {
				fs.Unbound = append(fs.Unbound, sp.Target)
			}
		}
	}
	return fs
}

func (a *Agent) sendShare() {
	a.mu.Lock()
	host, fs := a.host, a.shareNow()
	a.mu.Unlock()
	if host != nil && fs != nil {
		go host.Notify(api.NotifyFileShareState, api.FileShareState{FileShare: *fs})
	}
}

type statSub struct {
	conn *jsonrpc.Conn
	stop chan struct{}
}

func (a *Agent) sample(id uint64) api.StatsSample {
	now := a.Clock()
	a.mu.Lock()
	ready := a.engine.State == api.EngineReady
	a.mu.Unlock()
	s := api.StatsSample{Subscription: id, TimeMS: now, MemTotalKB: 4 << 20, MemAvailableKB: 3 << 20, CachedKB: 256 << 10,
		Groups: []api.StatsGroup{{Kind: api.StatsSystem, Name: "guest", CPUUsec: uint64(now * 100), MemoryBytes: 1 << 30, MemoryFileBytes: 256 << 20,
			DiskReadBytes: 8 << 20, DiskWriteBytes: uint64(now) * 512, NetRxBytes: uint64(now) * 10, NetTxBytes: uint64(now) * 5, PIDs: 40}}}
	if ready {
		s.Groups = append(s.Groups,
			api.StatsGroup{Kind: api.StatsService, Name: "engine", CPUUsec: uint64(now * 40), MemoryBytes: 96 << 20, MemoryFileBytes: 32 << 20, DiskReadBytes: 4 << 20, PIDs: 24})
		var cs []StatsName
		if a.Containers != nil {
			cs = a.Containers()
		}
		sort.Slice(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
		for i, c := range cs {
			k := uint64(i + 1)
			s.Groups = append(s.Groups, api.StatsGroup{Kind: api.StatsContainer, Name: c.Name, ID: c.ID, CPUUsec: uint64(now*10) * k,
				MemoryBytes: (8 << 20) * k, MemoryFileBytes: 2 << 20, DiskReadBytes: 1 << 20, DiskWriteBytes: uint64(now) * 64 * k,
				NetRxBytes: uint64(now) * k, NetTxBytes: uint64(now) * k / 2, PIDs: 3})
		}
	}
	for i, m := range a.runningMachines() {
		k := uint64(i + 1)
		s.Groups = append(s.Groups, api.StatsGroup{Kind: api.StatsMachine, Name: m.Name, ID: m.UUID, CPUUsec: uint64(now*5) * k,
			MemoryBytes: (64 << 20) * k, MemoryFileBytes: 16 << 20, DiskReadBytes: 2 << 20, NetRxBytes: uint64(now) * 2 * k, NetTxBytes: uint64(now) * k, PIDs: 12})
	}
	return s
}

func (a *Agent) subscribeStats(ctx context.Context, c *jsonrpc.Conn, p api.StatsSubscribeParams) (any, error) {
	if p.IntervalMS < 0 || (p.IntervalMS != 0 && p.IntervalMS < api.MinStatsIntervalMS) {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "interval_ms must be at least %d", api.MinStatsIntervalMS)
	}
	if p.IntervalMS == 0 {
		p.IntervalMS = api.DefaultStatsIntervalMS
	}
	sub := &statSub{conn: c, stop: make(chan struct{})}
	a.mu.Lock()
	a.nextID++
	id := a.nextID
	a.statSubs[id] = sub
	a.mu.Unlock()
	a.Logf(api.LogInit, 7, "stats: stream %d begun, a sample every %d ms", id, p.IntervalMS)
	jsonrpc.AfterReply(ctx, func() {
		go func() {
			if a.StatsFirst > 0 {
				select {
				case <-time.After(a.StatsFirst):
				case <-sub.stop:
					return
				case <-c.Done():
					return
				}
			}
			tick := time.NewTicker(time.Duration(p.IntervalMS) * time.Millisecond)
			defer tick.Stop()
			defer a.Logf(api.LogInit, 7, "stats: stream ended (%d)", id)
			for {
				if a.Gate != nil {
					a.Gate()
				}
				if c.Notify(api.NotifyStatsSample, a.sample(id)) != nil {
					return
				}
				select {
				case <-tick.C:
				case <-sub.stop:
					return
				case <-c.Done():
					return
				}
			}
		}()
	})
	return api.Subscription{Subscription: id}, nil
}

func (a *Agent) StatsSubscriptions() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, sub := range a.statSubs {
		select {
		case <-sub.conn.Done():
		default:
			n++
		}
	}
	return n
}

func (a *Agent) setEngine(change func(*api.EngineStatus)) api.EngineStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	change(&a.engine)
	st := a.engine
	for id, s := range a.engineSubs {
		s.send(api.NotifyEngineState, api.EngineState{Subscription: id, EngineStatus: st})
	}
	return st
}

func (a *Agent) startEngine() api.EngineStatus {
	a.mu.Lock()
	if a.engine.State == api.EngineStarting || a.engine.State == api.EngineReady {
		st := a.engine
		a.mu.Unlock()
		return st
	}
	a.engineGen++
	gen := a.engineGen
	a.running = a.keptConfig()
	a.mu.Unlock()
	st := a.setEngine(func(e *api.EngineStatus) {
		*e = api.EngineStatus{State: api.EngineStarting, PID: 100 + e.Starts, Starts: e.Starts + 1, StartedMS: a.Clock(), LastExit: e.LastExit}
		if a.firstStart == 0 {
			a.firstStart = e.StartedMS
		}
	})
	a.Logf(api.LogEngine, 6, "Starting up")
	var settle func()
	settle = func() {
		a.mu.Lock()
		current := a.engineGen == gen && a.engine.State == api.EngineStarting
		a.mu.Unlock()
		if !current {
			return
		}
		if a.EngineFlap {
			a.flap(gen)
			time.AfterFunc(a.EngineDelay, settle)
			return
		}
		if a.EngineError != "" {
			a.setEngine(func(e *api.EngineStatus) {
				e.State, e.Error, e.PID, e.LastExit = api.EngineFailed, a.EngineError, 0, "exit 1"
			})
			return
		}
		a.setEngine(func(e *api.EngineStatus) {
			e.State, e.ReadyMS = api.EngineReady, a.Clock()
			if a.firstReady == 0 {
				a.firstReady = e.ReadyMS
			}
		})
		a.Logf(api.LogEngine, 6, "API listen on /run/docker.sock")
	}
	time.AfterFunc(a.EngineDelay, settle)
	return st
}

var flapLines = []string{
	"unexpected fault address 0xffff8c4a2000",
	"fatal error: fault",
	"[signal SIGBUS: bus error code=0x2 addr=0xffff8c4a2000 pc=0x5f1d3c]",
	"goroutine 1 gp=0x4000002380 m=0 mp=0x2d8a1e0 [running]:",
}

func (a *Agent) flap(gen int) {
	for _, line := range flapLines {
		a.Logf(api.LogEngine, 6, "%s", line)
	}
	a.Logf(api.LogInit, 4, "engine: exited unexpectedly (exit status 2)")
	for _, next := range []func(e *api.EngineStatus){
		func(e *api.EngineStatus) { e.PID, e.ReadyMS, e.LastExit = 0, 0, "exit status 2" },
		func(e *api.EngineStatus) { e.PID, e.Starts, e.StartedMS = 100+e.Starts, e.Starts+1, a.Clock() },
	} {
		a.mu.Lock()
		current := a.engineGen == gen && a.engine.State == api.EngineStarting
		a.mu.Unlock()
		if !current {
			return
		}
		a.setEngine(next)
	}
	a.Logf(api.LogEngine, 6, "Starting up")
}

func (a *Agent) stopEngine() api.EngineStatus {
	a.mu.Lock()
	a.engineGen++
	a.mu.Unlock()
	return a.setEngine(func(e *api.EngineStatus) {
		if e.State != api.EngineStopped {
			*e = api.EngineStatus{State: api.EngineStopped, Starts: e.Starts, LastExit: "exit 0"}
		}
	})
}

type helloKey struct{}
type subsKey struct{}

func (a *Agent) ServeConn(rw io.ReadWriteCloser) *jsonrpc.Conn {
	conn := jsonrpc.NewConn(rw, jsonrpc.HandlerFunc(a.handle))
	go func() {
		<-conn.Done()
		a.mu.Lock()
		for id, s := range a.logSubs {
			if s.conn == conn {
				delete(a.logSubs, id)
			}
		}
		for id, s := range a.engineSubs {
			if s.conn == conn {
				delete(a.engineSubs, id)
			}
		}
		a.mu.Unlock()
	}()
	return conn
}

func rpcError(code int, data any, format string, args ...any) *jsonrpc.Error {
	e := jsonrpc.Errorf(code, format, args...)
	if data != nil {
		e.Data, _ = json.Marshal(data)
	}
	return e
}

func (a *Agent) FallBehind(d time.Duration) {
	a.mu.Lock()
	a.behind += d
	a.mu.Unlock()
}

func (a *Agent) ClockSets() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.clockSets
}

func (a *Agent) LeaveCache(bytes uint64) {
	a.mu.Lock()
	a.cache = bytes
	a.mu.Unlock()
}

const (
	standinUsed       = 6 << 20
	standinReferenced = 5 << 20
	standinReserved   = 256 << 20
)

func (a *Agent) Budgets() []api.BudgetParams {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]api.BudgetParams{}, a.budgets...)
}

func (a *Agent) Reclaims() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reclaims
}

func (a *Agent) memoryReclaim(p api.ReclaimParams) (any, error) {
	if p.FloorMiB < 0 {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "floor_mib must not be negative")
	}
	floor := p.FloorMiB
	if floor == 0 {
		floor = api.DefaultReclaimFloorMiB
	}
	a.mu.Lock()
	r := api.Reclaimed{FileBeforeBytes: a.cache, FileAfterBytes: a.cache, FloorMiB: floor, DurationMS: 0.1}
	if a.cache <= uint64(floor)<<20 {
		r.Skipped = api.ReclaimSkippedFloor
		a.mu.Unlock()
		return r, nil
	}
	a.cache = uint64(floor) << 20
	a.reclaims++
	r.FileAfterBytes, r.ReclaimedBytes, r.Compacted = a.cache, r.FileBeforeBytes-a.cache, !p.NoCompact
	a.mu.Unlock()
	a.Logf(api.LogInit, 6, "memory: reclaimed %d MiB of file cache", r.ReclaimedBytes>>20)
	return r, nil
}

func (a *Agent) timeSet(p api.TimeSetParams) (any, error) {
	if p.ThresholdMS < 0 {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "threshold_ms must not be negative")
	}
	if p.UnixNS < time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano() {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "unix_ns is not a time after 2020")
	}
	threshold := p.ThresholdMS
	if threshold == 0 {
		threshold = api.DefaultTimeThresholdMS
	}
	a.mu.Lock()
	guest := time.Now().Add(-a.behind)
	offset := guest.Sub(time.Unix(0, p.UnixNS))
	r := api.TimeSet{OffsetMS: float64(offset.Microseconds()) / 1000, ThresholdMS: threshold}
	if offset.Abs() > time.Duration(threshold)*time.Millisecond {
		r.Stepped = true
		a.behind = 0
		a.clockSets++
	}
	a.mu.Unlock()
	if r.Stepped {
		a.Logf(api.LogInit, 6, "clock: stepped the wall clock by %.0f ms", -r.OffsetMS)
	} else {
		a.Logf(api.LogInit, 7, "clock: within %d ms of the host's; not stepped", threshold)
	}
	return r, nil
}

func (a *Agent) handle(ctx context.Context, c *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
	if a.Gate != nil {
		a.Gate()
	}
	if types, ok := api.Schema[method]; !ok || types.Result == nil {
		if types, ok := api.Pending[method]; !ok || types.Result == nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method %q not found", method)
		}
	}
	if method == api.MethodHello {
		p, err := jsonrpc.Bind[api.HelloParams](params)
		if err != nil {
			return nil, err
		}
		if p.APIVersion != api.APIVersion {
			jsonrpc.AfterReply(ctx, func() { c.Close() })
			return nil, jsonrpc.Errorf(api.CodeVersionMismatch, "agent speaks API version %d, client %d", api.APIVersion, p.APIVersion)
		}
		if p.Build != "" && p.Build != a.Build {
			jsonrpc.AfterReply(ctx, func() { c.Close() })
			return nil, rpcError(api.CodeBuildMismatch, api.BuildMismatch{AgentBuild: a.Build, ClientBuild: p.Build},
				"build mismatch: the agent is %q and the client is %q", a.Build, p.Build)
		}
		c.Set(helloKey{}, true)
		a.mu.Lock()
		host := a.host == c
		a.mu.Unlock()
		if host {
			jsonrpc.AfterReply(ctx, a.firstShare)
		}
		return api.HelloResult{APIVersion: api.APIVersion, AgentVersion: a.Build, Kernel: a.Kernel}, nil
	}
	if _, ok := c.Get(helloKey{}); !ok {
		return nil, jsonrpc.Errorf(api.CodeHelloRequired, "call agent.hello first")
	}
	if _, pending := api.Pending[method]; pending && !servedAhead[method] {
		return nil, jsonrpc.Errorf(api.CodeNotImplemented, "%s is not implemented by the stand-in agent", method)
	}

	switch method {
	case api.MethodPing:
		return api.Empty{}, nil
	case api.MethodTimeSet:
		p, err := jsonrpc.Bind[api.TimeSetParams](params)
		if err != nil {
			return nil, err
		}
		return a.timeSet(p)
	case api.MethodMemoryReclaim:
		p, err := jsonrpc.Bind[api.ReclaimParams](params)
		if err != nil {
			return nil, err
		}
		return a.memoryReclaim(p)
	case api.MethodInfo:
		info := api.Info{
			Kernel: a.Kernel, UptimeMS: int64(a.Clock()), CPUs: 2, MemTotalKB: 4 << 20, MemAvailableKB: 3 << 20,
			Interfaces:        []api.Interface{{Name: "lo", Up: true, Addresses: []string{"127.0.0.1/8"}}, {Name: "eth0", Up: true, Addresses: []string{netplan.Default().Guest().String()}}},
			Mounts:            []api.Mount{{Source: "/dev/vda", Target: "/data", FSType: "btrfs"}},
			CgroupControllers: []string{"cpu", "cpuset", "io", "memory", "pids"},
		}
		if a.Share != nil {
			a.mu.Lock()
			info.Mounts = append(info.Mounts, api.Mount{Source: "mac", Target: "/mnt/mac", FSType: "virtiofs"})
			info.FileShare = a.shareNow()
			a.mu.Unlock()
		}
		return info, nil
	case api.MethodBootTrace:
		p, err := jsonrpc.Bind[api.BootTraceParams](params)
		if err != nil {
			return nil, err
		}
		if p.Wait {
			select {
			case <-a.ready:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		stages := append([]api.Stage{}, a.stages...)
		return api.BootTrace{InitStartMS: a.initStart, ReadyMS: a.readyMS, EngineStartMS: a.firstStart, EngineReadyMS: a.firstReady, Stages: stages, NowMS: a.Clock()}, nil
	case api.MethodShutdown:
		p, err := jsonrpc.Bind[api.ShutdownParams](params)
		if err != nil {
			return nil, err
		}
		if p.DeadlineMS < 0 || p.DeadlineMS > api.MaxShutdownDeadlineMS {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "deadline_ms must be between 0 and %d", api.MaxShutdownDeadlineMS)
		}
		if p.DeadlineMS == 0 {
			p.DeadlineMS = api.DefaultShutdownDeadlineMS
		}
		jsonrpc.AfterReply(ctx, func() {
			for _, phase := range []string{api.ShutdownBegin, api.ShutdownEngine, api.ShutdownSessions, api.ShutdownSync, api.ShutdownUnmount, api.ShutdownOff} {
				if phase == api.ShutdownEngine {
					a.StopAll()
					a.stopEngine()
				}
				c.Notify(api.NotifyShutdownProgress, api.ShutdownProgress{Phase: phase})
			}
			if a.OnShutdown != nil {
				a.OnShutdown(p.Reboot, time.Duration(p.DeadlineMS)*time.Millisecond)
			}
		})
		return api.Empty{}, nil
	case api.MethodLogSubscribe:
		p, err := jsonrpc.Bind[api.LogSubscribeParams](params)
		if err != nil {
			return nil, err
		}
		var machineLines []api.LogEntry
		for _, src := range p.Sources {
			name, ok := strings.CutPrefix(src, api.LogMachinePrefix)
			if !ok {
				continue
			}
			runtime := strings.HasSuffix(name, ":runtime")
			name = strings.TrimSuffix(name, ":runtime")
			m := a.machines()
			m.mu.Lock()
			_, have := m.byName[name]
			m.mu.Unlock()
			if !have {
				return nil, jsonrpc.Errorf(api.CodeNoMachine, "no machine %q", name)
			}
			text := []string{"fev-standin: the console of " + name, name + " login:"}
			if runtime {
				text = []string{"machine " + name + ": made by the stand-in", "machine " + name + ": started"}
			}
			for _, t := range text {
				machineLines = append(machineLines, api.LogEntry{Source: src, TimeMS: a.Clock(), Level: 6, Text: t})
			}
		}
		s := newStream(ctx, c)
		s.sources = map[string]bool{}
		if len(p.Sources) == 0 {
			p.Sources = []string{api.LogKernel, api.LogInit, api.LogEngine}
		}
		for _, src := range p.Sources {
			s.sources[src] = true
		}
		a.mu.Lock()
		a.nextID++
		id := a.nextID
		if p.Backlog {
			for _, e := range append(append([]api.LogEntry{}, a.backlog...), machineLines...) {
				if s.sources[e.Source] {
					e.Subscription = id
					s.send(api.NotifyLogEntry, e)
				}
			}
			s.send(api.NotifyLogBacklogEnd, api.Subscription{Subscription: id})
		}
		a.logSubs[id] = s
		a.mu.Unlock()
		return api.Subscription{Subscription: id}, nil
	case api.MethodLogUnsubscribe:
		p, err := jsonrpc.Bind[api.Subscription](params)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		if s := a.logSubs[p.Subscription]; s != nil && s.conn == c {
			delete(a.logSubs, p.Subscription)
		}
		a.mu.Unlock()
		return api.Empty{}, nil
	case api.MethodFetch:
		p, err := jsonrpc.Bind[api.FetchParams](params)
		if err != nil {
			return nil, err
		}
		if p.URL == "" {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "url is required")
		}
		return api.FetchResult{Status: 200, Bytes: 1256, ElapsedMS: 12.5, RemoteAddr: "198.51.100.7:80"}, nil
	case api.MethodPingHost:
		p, err := jsonrpc.Bind[api.PingParams](params)
		if err != nil {
			return nil, err
		}
		if p.Address == "" {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "address is required")
		}
		n := max(p.Count, 1)
		return api.PingResult{Sent: n, Received: n, From: p.Address, AvgMS: 0.4}, nil

	case api.MethodEngineStatus:
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.engine, nil
	case api.MethodEngineStart:
		return a.startEngine(), nil
	case api.MethodEngineStop, api.MethodEngineRestart:
		p, err := jsonrpc.Bind[api.EngineStopParams](params)
		if err != nil {
			return nil, err
		}
		if p.GraceMS < 0 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "grace_ms must not be negative")
		}
		st := a.stopEngine()
		if method == api.MethodEngineRestart {
			st = a.startEngine()
		}
		return st, nil
	case api.MethodEngineSubscribe:
		s := newStream(ctx, c)
		a.mu.Lock()
		a.nextID++
		id := a.nextID
		s.send(api.NotifyEngineState, api.EngineState{Subscription: id, EngineStatus: a.engine})
		a.engineSubs[id] = s
		a.mu.Unlock()
		return api.Subscription{Subscription: id}, nil
	case api.MethodEngineUnsubscribe:
		p, err := jsonrpc.Bind[api.Subscription](params)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		if s := a.engineSubs[p.Subscription]; s != nil && s.conn == c {
			delete(a.engineSubs, p.Subscription)
		}
		a.mu.Unlock()
		return api.Empty{}, nil

	case api.MethodEngineConfigure:
		p, err := jsonrpc.Bind[api.EngineConfigureParams](params)
		if err != nil {
			return nil, err
		}
		var config map[string]json.RawMessage
		if err := json.Unmarshal(p.Config, &config); err != nil || config == nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "config must be a JSON object; {} means none")
		}
		return a.configure(config), nil

	case api.MethodFileShareWatch:
		p, err := jsonrpc.Bind[api.WatchParams](params)
		if err != nil {
			return nil, err
		}
		if _, err := fsevents.Append(nil, fsevents.Watch{Seq: 1, Paths: p.Paths}); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
		return a.watch(p.Paths)

	case api.MethodStreamRefuse:
		p, err := jsonrpc.Bind[api.StreamRefuseParams](params)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		waiting := a.streams[p.Token]
		delete(a.streams, p.Token)
		a.mu.Unlock()
		if waiting != nil {
			waiting <- streamAnswer{refused: p.Reason}
		}
		return api.Empty{}, nil

	case api.MethodStorageCheck:
		return a.storageCheck(ctx, c)
	case api.MethodStorageUsage:
		p, err := jsonrpc.Bind[api.UsageParams](params)
		if err != nil {
			return nil, err
		}
		return a.storageUsage(p)
	case api.MethodStorageSalvage:
		p, err := jsonrpc.Bind[api.SalvageParams](params)
		if err != nil {
			return nil, err
		}
		return a.storageSalvage(ctx, c, p)

	case api.MethodMachinePorts:
		return a.machinePorts(), nil

	case api.MethodEnginePorts:
		return api.EnginePorts{Address: "127.0.0.1", Seq: 1, Ports: []api.MachinePort{}}, nil

	case api.MethodMacAddressesSet:
		p, err := jsonrpc.Bind[api.MacAddresses](params)
		if err != nil {
			return nil, err
		}
		if err := api.CheckMacAddresses(p); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
		return api.Empty{}, nil

	case api.MethodTrustSet:
		return a.trustSet(params)

	case api.MethodDomainsSet:
		return a.domainsSet(params)

	case api.MethodViewStart, api.MethodViewStop, api.MethodViewStatus, api.MethodViewObjects:
		return a.viewCall(method, params)

	case api.MethodSSHAgentSet:
		return a.sshAgentSet(params)

	case api.MethodEmulationStatus, api.MethodEmulationSet:
		return a.emulationCall(method, params)

	case api.MethodMachineList, api.MethodMachineInfo, api.MethodMachineStart, api.MethodMachineStop, api.MethodMachineRestart,
		api.MethodMachineCreate, api.MethodMachineDelete, api.MethodMachineClone, api.MethodMachineRename,
		api.MethodMachineExport, api.MethodMachineImport, api.MethodOperationCancel:
		if method != api.MethodOperationCancel {
			if a.NoData != "" {
				return nil, jsonrpc.Errorf(api.CodeFailed, "machines are not available: %s: %s", api.NoDataFilesystem, a.NoData)
			}
			if a.MountRung >= 4 {
				return nil, jsonrpc.Errorf(api.CodeFailed, "machines are not available: %s", api.DataReadOnly)
			}
		}
		return a.machineCall(ctx, c, method, params)

	case api.MethodVolumeClone, api.MethodVolumeExport, api.MethodVolumeImport:
		return a.volumeCall(ctx, c, method, params)

	case api.MethodStorageInfo, api.MethodStorageBudget, api.MethodStorageTrim, api.MethodStorageSync:
		if a.NoData != "" {
			return nil, jsonrpc.Errorf(api.CodeFailed, "%s: %s", api.NoDataFilesystem, a.NoData)
		}
	}
	switch method {
	case api.MethodStorageInfo:
		a.mu.Lock()
		q := api.StorageQuota{ReferencedBytes: standinReferenced}
		if n := len(a.budgets); n > 0 && !a.budgets[n-1].Unlimited {
			q.Limited = true
			q.LimitBytes = standinReferenced + api.BudgetBytes(a.budgets[n-1].AvailableBytes, a.budgets[n-1].MarginBytes, a.budgets[n-1].ImageBytes, standinUsed, standinReserved)
		}
		a.mu.Unlock()
		rung, steps := mountSteps(a.MountRung)
		return api.StorageInfo{Device: "/dev/vda", FSType: "btrfs", FSID: "6665766f-6d00-4000-8000-0000000000fa", Layout: 2,
			SizeBytes: 1 << 40, UsedBytes: standinUsed, MountRung: rung, ReadOnly: rung >= 4, Steps: steps, Quota: &q, Check: a.checkState(rung)}, nil
	case api.MethodStorageBudget:
		p, err := jsonrpc.Bind[api.BudgetParams](params)
		if err != nil {
			return nil, err
		}
		if p.AvailableBytes < 0 || p.MarginBytes < 0 || p.ImageBytes < 0 || p.ReclaimBytes < 0 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "storage.budget: a figure is negative: %+v", p)
		}
		if !p.Unlimited && p.ReclaimBytes > 0 && api.ReclaimableBytes(p.ImageBytes, standinUsed, standinReserved) >= p.ReclaimBytes {
			a.Logf(api.LogInit, 7, "storage: no budget set: the image holds %d bytes to give back first", api.ReclaimableBytes(p.ImageBytes, standinUsed, standinReserved))
			return api.Budget{Deferred: true, ReferencedBytes: standinReferenced, ReservedBytes: standinReserved, UsedBytes: standinUsed, DurationMS: 0.1}, nil
		}
		a.mu.Lock()
		a.budgetCalls++
		refuse := a.BudgetFail[0] != 0 && a.budgetCalls >= a.BudgetFail[0] && a.budgetCalls <= a.BudgetFail[1]
		if !refuse {
			a.budgets = append(a.budgets, p)
		}
		a.mu.Unlock()
		if refuse {
			return nil, jsonrpc.Errorf(api.CodeFailed, "read the quota group: open /sys/fs/btrfs/6665766f/qgroups/1_0/referenced: no such file or directory")
		}
		r := api.Budget{ReferencedBytes: standinReferenced, ReservedBytes: standinReserved, UsedBytes: standinUsed, DurationMS: 0.3}
		if !p.Unlimited {
			r.Limited = true
			r.BudgetBytes = api.BudgetBytes(p.AvailableBytes, p.MarginBytes, p.ImageBytes, standinUsed, standinReserved)
			r.LimitBytes = r.ReferencedBytes + r.BudgetBytes
		}
		a.Logf(api.LogInit, 7, "storage: budget %d bytes: limit %d", r.BudgetBytes, r.LimitBytes)
		return r, nil
	case api.MethodStorageTrim:
		if _, err := jsonrpc.Bind[api.TrimParams](params); err != nil {
			return nil, err
		}
		a.Logf(api.LogInit, 7, "storage: trimmed")
		return api.TrimResult{TrimmedBytes: 1 << 30, DurationMS: 3.5}, nil
	case api.MethodStorageSync:
		time.Sleep(a.SyncDelay)
		a.mu.Lock()
		a.syncs++
		n := a.syncs
		a.mu.Unlock()
		a.Logf(api.LogInit, 7, "storage: sync %d: the data disk has what was written", n)
		return api.Synced{DurationMS: float64(a.SyncDelay.Microseconds()) / 1000}, nil

	case api.MethodFileShareBind:
		if a.Share == nil {
			return nil, jsonrpc.Errorf(api.CodeFailed, "this VM has no file share")
		}
		bound, unbound := a.bindShare(false)
		a.Logf(api.LogInit, 6, "fileshare: bound %d of the engine's Mac paths late (%s); %d have no directory on the Mac", len(bound), strings.Join(bound, " "), len(unbound))
		return api.ShareBound{Bound: bound, Unbound: unbound}, nil

	case api.MethodStatsSubscribe:
		p, err := jsonrpc.Bind[api.StatsSubscribeParams](params)
		if err != nil {
			return nil, err
		}
		return a.subscribeStats(ctx, c, p)
	case api.MethodStatsUnsubscribe:
		p, err := jsonrpc.Bind[api.Subscription](params)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		sub := a.statSubs[p.Subscription]
		if sub != nil && sub.conn == c {
			delete(a.statSubs, p.Subscription)
			close(sub.stop)
		}
		a.mu.Unlock()
		if sub == nil || sub.conn != c {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "no subscription %d on this connection", p.Subscription)
		}
		return api.Empty{}, nil

	case api.MethodExecStart:
		p, err := jsonrpc.Bind[api.ExecStartParams](params)
		if err != nil {
			return nil, err
		}
		return a.execStart(c, p)
	case api.MethodExecResize, api.MethodExecSignal, api.MethodExecClose:
		return a.execCall(c, method, params)

	case api.MethodDebugStatus:
		return a.debugStatus()
	case api.MethodDebugReset:
		p, err := jsonrpc.Bind[api.DebugResetParams](params)
		if err != nil {
			return nil, err
		}
		return a.debugReset(p)

	case api.MethodK8sStart, api.MethodK8sStop, api.MethodK8sReset, api.MethodK8sStatus, api.MethodK8sConfigure, api.MethodK8sImagePull, api.MethodK8sCRI:
		return a.handleK8s(ctx, c, method, params)
	}
	return nil, jsonrpc.Errorf(api.CodeNotImplemented, "%s is not implemented by the stand-in agent", method)
}

func (a *Agent) feed() string {
	switch {
	case a.NoDevice:
		return api.FeedNone
	case a.Feed != "":
		return a.Feed
	}
	return api.FeedConnected
}

func (a *Agent) watch(paths []string) (any, error) {
	if a.Share == nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "this VM has no file share")
	}
	if state := a.feed(); state != api.FeedConnected {
		return nil, jsonrpc.Errorf(api.CodeFailed, "the change feed is %s", state)
	}
	inside := func(dir, p string) bool {
		return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
	}
	r := api.WatchResult{}
	for _, p := range paths {
		reason := ""
		switch {
		case !inside(filepath.Clean(a.Share.Root), p):
			reason = api.WatchOutside
		default:
			for _, d := range a.Share.Deny {
				if inside(filepath.Clean(d), p) {
					reason = api.WatchOutside
				}
			}
			if _, err := os.Lstat(p); err != nil && reason == "" {
				reason = api.WatchMissing
			}
		}
		if reason == "" {
			r.Watching++
		} else {
			r.Rejected = append(r.Rejected, api.WatchRejected{Path: p, Reason: reason})
		}
	}
	a.mu.Lock()
	a.watching = r.Watching
	a.watches++
	a.mu.Unlock()
	a.Logf(api.LogInit, 7, "fileshare: watch set %d: %d paths, %d watched", a.watches, len(paths), r.Watching)
	return r, nil
}

func canonical(config map[string]json.RawMessage) string {
	if len(config) == 0 {
		return "{}"
	}
	var generic map[string]any
	data, _ := json.Marshal(config)
	json.Unmarshal(data, &generic)
	out, _ := json.Marshal(generic)
	return string(out)
}

func (a *Agent) keptConfig() string {
	if a.ConfigFile != "" {
		if data, err := os.ReadFile(a.ConfigFile); err == nil {
			return strings.TrimSpace(string(data))
		}
		return "{}"
	}
	if a.kept == "" {
		return "{}"
	}
	return a.kept
}

func (a *Agent) configure(config map[string]json.RawMessage) api.EngineConfigured {
	keys := make([]string, 0, len(config))
	for k := range config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "hosts", "data-root", "exec-root", "pidfile", "init-path", "userland-proxy-path", "host-gateway-ip", "host-gateway-ips":
			return api.EngineConfigured{State: api.EngineConfigRefused, Error: k + " is set by fev and cannot be changed in docker.json"}
		}
		if strings.HasPrefix(k, "bogus") {
			return api.EngineConfigured{State: api.EngineConfigRefused, Error: "the following directives don't match any configuration option: " + k}
		}
	}
	want := canonical(config)
	a.mu.Lock()
	defer a.mu.Unlock()
	restart := func() bool {
		return (a.engine.State == api.EngineStarting || a.engine.State == api.EngineReady) && a.running != want
	}
	if want == a.keptConfig() {
		return api.EngineConfigured{State: api.EngineConfigUnchanged, RestartNeeded: restart()}
	}
	if a.ConfigFile != "" {
		if want == "{}" {
			os.Remove(a.ConfigFile)
		} else {
			os.WriteFile(a.ConfigFile, []byte(want+"\n"), 0o600)
		}
	}
	a.kept = want
	return api.EngineConfigured{State: api.EngineConfigAccepted, RestartNeeded: restart()}
}

func (a *Agent) SetHost(c *jsonrpc.Conn) {
	a.mu.Lock()
	a.host = c
	a.mu.Unlock()
	if _, hello := c.Get(helloKey{}); hello {
		a.firstShare()
	}
}

func (a *Agent) firstShare() {
	a.mu.Lock()
	sent := a.shareSent
	a.shareSent = true
	a.mu.Unlock()
	if !sent {
		a.sendShare()
	}
}

func (a *Agent) HostStream(service string, payload []byte, framed bool) ([]byte, error) {
	var raw [relay.TokenLen / 2]byte
	rand.Read(raw[:])
	token := hex.EncodeToString(raw[:])
	answer := make(chan streamAnswer, 1)
	a.mu.Lock()
	host := a.host
	if host != nil {
		a.streams[token] = answer
	}
	a.mu.Unlock()
	if host == nil {
		return nil, errors.New("closed at once: there is no connection to the host")
	}
	if err := host.Notify(api.NotifyStreamRequest, api.StreamRequest{Token: token, Service: service}); err != nil {
		return nil, err
	}
	select {
	case got := <-answer:
		if got.conn == nil {
			return nil, fmt.Errorf("refused: %s", got.refused)
		}
		defer close(got.done)
		if _, err := got.conn.Write(payload); err != nil {
			return nil, err
		}
		if framed {
			head := make([]byte, 4)
			if _, err := io.ReadFull(got.conn, head); err != nil {
				return nil, fmt.Errorf("the service closed the stream before answering: %w", err)
			}
			n := int(head[0])<<24 | int(head[1])<<16 | int(head[2])<<8 | int(head[3])
			if n > 1<<20 {
				return nil, fmt.Errorf("an answer of %d bytes", n)
			}
			body := make([]byte, n)
			if _, err := io.ReadFull(got.conn, body); err != nil {
				return nil, err
			}
			return append(head, body...), nil
		}
		if hc, ok := got.conn.(interface{ CloseWrite() error }); ok {
			hc.CloseWrite()
		}
		return io.ReadAll(io.LimitReader(got.conn, 1<<20))
	case <-time.After(10 * time.Second):
		a.mu.Lock()
		delete(a.streams, token)
		a.mu.Unlock()
		return nil, errors.New("dropped: the host did not answer in 10s")
	}
}

func (a *Agent) AttachStream(token string, conn net.Conn) bool {
	if a.attachOpStream(token, conn) {
		return true
	}
	a.mu.Lock()
	waiting := a.streams[token]
	delete(a.streams, token)
	a.mu.Unlock()
	if waiting == nil {
		return false
	}
	if relay.WriteOK(conn) != nil {
		waiting <- streamAnswer{refused: "the host's stream ended as it was attached"}
		return true
	}
	done := make(chan struct{})
	waiting <- streamAnswer{conn: conn, done: done}
	<-done
	return true
}

const RepairLine = "BTRFS info (device vda): read error corrected: ino 0 off 31145984 (dev /dev/vda sector 77216)"

func mountSteps(rung int) (int, []api.MountStep) {
	if rung < 1 || rung > 5 {
		rung = 1
	}
	all := []api.MountStep{
		{Rung: 1, Name: "mount", Error: "mount -o discard=async,nodatasum,nodatacow: input/output error (the kernel log has the reason)"},
		{Rung: 2, Name: "btrfs rescue super-recover", Skipped: "the first superblock is valid"},
		{Rung: 3, Name: "btrfs rescue zero-log", Skipped: "the filesystem has no fsync log to replay"},
		{Rung: 4, Name: "read-only rescue=usebackuproot", Error: "mount -o rescue=usebackuproot,rescue=nologreplay: input/output error (the kernel log has the reason)"},
		{Rung: 5, Name: "read-only rescue=all"},
	}
	steps := append([]api.MountStep{}, all[:rung]...)
	last := &steps[rung-1]
	last.Error, last.Skipped = "", ""
	if rung == 3 {
		last.Lost = "the stand-in's fsync log was dropped"
	}
	return rung, steps
}

const standinScrubbed = 3305111552

func (a *Agent) checkState(rung int) *api.StorageCheck {
	if rung < 2 || rung > 4 {
		return nil
	}
	c := &api.StorageCheck{State: api.OperationDone, Auto: true, ReadOnly: rung == 4, StartedMS: 0, ScrubbedBytes: standinScrubbed, Uncorrectable: a.CheckUncorrectable}
	if left := a.checkLeft(); left > 0 {
		c.State, c.ScrubbedBytes, c.Uncorrectable = api.OperationRunning, standinScrubbed/2, 0
	} else {
		c.EndedMS = float64(a.CheckTime.Microseconds()) / 1000
	}
	return c
}

func (a *Agent) checkLeft() time.Duration {
	return a.CheckTime - time.Duration(a.Clock()*float64(time.Millisecond))
}

func (a *Agent) storageCheck(ctx context.Context, c *jsonrpc.Conn) (any, error) {
	check := a.checkState(a.MountRung)
	if check == nil || check.State != api.OperationRunning {
		const id = 8
		now := a.Clock()
		done := api.StorageCheck{State: api.OperationDone, StartedMS: now, EndedMS: now, ScrubbedBytes: standinScrubbed}
		result, _ := json.Marshal(done)
		jsonrpc.AfterReply(ctx, func() {
			c.Notify(api.NotifyOperationProgress, api.OperationProgress{ID: id, Method: api.MethodStorageCheck, State: api.OperationDone, Phase: api.PhaseScrub,
				DoneBytes: done.ScrubbedBytes, Result: result})
		})
		return api.Operation{ID: id}, nil
	}
	const id = 7
	jsonrpc.AfterReply(ctx, func() {
		time.AfterFunc(a.checkLeft(), func() {
			done := a.checkState(a.MountRung)
			result, _ := json.Marshal(done)
			c.Notify(api.NotifyOperationProgress, api.OperationProgress{ID: id, Method: api.MethodStorageCheck, State: api.OperationDone, Phase: api.PhaseScrub,
				DoneBytes: done.ScrubbedBytes, TotalBytes: done.ScrubbedBytes, Result: result})
		})
	})
	return api.Operation{ID: id}, nil
}
