package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	api "fev/proto/agent"
	"fev/proto/fsevents"
	"fev/proto/jsonrpc"
	"fev/proto/version"

	"fev/guest/internal/logbuf"
)

type System interface {
	Kernel() string
	Info() (*api.Info, error)
	BootTrace() api.BootTrace
	Ready() <-chan struct{}
	Shutdown(reboot bool, deadline time.Duration, progress func(api.ShutdownProgress)) error
	KernelLog(ctx context.Context, backlog bool, emit func(timeMS float64, level int, text string), caughtUp func()) error

	Engine
	Storage
	Exec
	Host
	Clock
	Memory
	Stats
}

type Stats interface {
	StatsSample() (api.StatsSample, error)
}

type Engine interface {
	EngineStatus() (api.EngineStatus, error)
	EngineStart() (api.EngineStatus, error)
	EngineStop(grace time.Duration) (api.EngineStatus, error)
	EngineRestart(grace time.Duration) (api.EngineStatus, error)
	EngineSubscribe(emit func(api.EngineStatus)) (stop func(), err error)
	EngineConfigure(config map[string]json.RawMessage) (api.EngineConfigured, error)
}

type Host interface {
	FileShareWatch(ctx context.Context, paths []string) ([]fsevents.Status, error)
	StreamRefuse(token, reason string)
	FileShareBind() (bound, unbound []string, err error)
	FileShareFollow(emit func(api.FileShare)) (stop func())
}

type Storage interface {
	StorageInfo() (*api.StorageInfo, error)
	StorageTrim(minLen uint64) (*api.TrimResult, error)
	StorageBudget(p api.BudgetParams) (*api.Budget, error)
	StorageSync() error
}

type Exec interface {
	ExecStart(p api.ExecStartParams, exited func(api.ExecExit)) (*api.ExecStarted, error)
	ExecResize(id uint64, rows, cols int) error
	ExecSignal(id uint64, signal int) error
	ExecClose(id uint64) error
}

type Clock interface {
	WallClock() (int64, error)
	SetWallClock(unixNS int64) error
}

type Memory interface {
	IdleCache() (idle, own, live uint64, err error)
	MemoryPressure() (float64, error)
	ReclaimIdle(n uint64) error
	CompactMemory() error
	Unreportable() (uint64, error)
	SetReportingOrder(order int) (was int, err error)
	RunningCache() ([]RunningCache, error)
	TrimRunning(dir string, n uint64) error
}

type RunningCache struct {
	Dir                                                      string
	InactiveBytes, ActiveBytes, SlabBytes, Refaults, CPUUsec uint64
}

var ErrNotImplemented = errors.New("not implemented in this build of the agent")

var ErrNoMachine = errors.New("no such machine")

type Machines interface {
	MachineList() (*api.MachineList, error)
	MachineInfo(name string) (*api.MachineInfo, error)
	MachineStart(name string, cfg api.MachineConfig) (*api.MachineInfo, error)
	MachineStop(name string, grace time.Duration, force bool) (*api.MachineInfo, error)
	MachineRestart(name string, grace time.Duration, cfg api.MachineConfig) (*api.MachineInfo, error)
}

type MachinePorts interface {
	MachinePorts() api.MachinePortsList
}

type Unimplemented struct{}

func (Unimplemented) EngineStatus() (api.EngineStatus, error) {
	return api.EngineStatus{}, ErrNotImplemented
}
func (Unimplemented) EngineStart() (api.EngineStatus, error) {
	return api.EngineStatus{}, ErrNotImplemented
}
func (Unimplemented) EngineStop(time.Duration) (api.EngineStatus, error) {
	return api.EngineStatus{}, ErrNotImplemented
}
func (Unimplemented) EngineRestart(time.Duration) (api.EngineStatus, error) {
	return api.EngineStatus{}, ErrNotImplemented
}
func (Unimplemented) EngineSubscribe(func(api.EngineStatus)) (func(), error) {
	return nil, ErrNotImplemented
}
func (Unimplemented) EngineConfigure(map[string]json.RawMessage) (api.EngineConfigured, error) {
	return api.EngineConfigured{}, ErrNotImplemented
}
func (Unimplemented) FileShareWatch(context.Context, []string) ([]fsevents.Status, error) {
	return nil, ErrNotImplemented
}
func (Unimplemented) StreamRefuse(string, string)                {}
func (Unimplemented) FileShareBind() ([]string, []string, error) { return nil, nil, ErrNotImplemented }
func (Unimplemented) FileShareFollow(func(api.FileShare)) func() { return func() {} }
func (Unimplemented) StorageSync() error                         { return ErrNotImplemented }
func (Unimplemented) StatsSample() (api.StatsSample, error) {
	return api.StatsSample{}, ErrNotImplemented
}
func (Unimplemented) StorageInfo() (*api.StorageInfo, error)      { return nil, ErrNotImplemented }
func (Unimplemented) StorageTrim(uint64) (*api.TrimResult, error) { return nil, ErrNotImplemented }
func (Unimplemented) StorageBudget(api.BudgetParams) (*api.Budget, error) {
	return nil, ErrNotImplemented
}
func (Unimplemented) ExecStart(api.ExecStartParams, func(api.ExecExit)) (*api.ExecStarted, error) {
	return nil, ErrNotImplemented
}
func (Unimplemented) ExecResize(uint64, int, int) error          { return ErrNotImplemented }
func (Unimplemented) ExecSignal(uint64, int) error               { return ErrNotImplemented }
func (Unimplemented) ExecClose(uint64) error                     { return ErrNotImplemented }
func (Unimplemented) WallClock() (int64, error)                  { return 0, ErrNotImplemented }
func (Unimplemented) SetWallClock(int64) error                   { return ErrNotImplemented }
func (Unimplemented) IdleCache() (uint64, uint64, uint64, error) { return 0, 0, 0, ErrNotImplemented }
func (Unimplemented) MemoryPressure() (float64, error)           { return 0, ErrNotImplemented }
func (Unimplemented) ReclaimIdle(uint64) error                   { return ErrNotImplemented }
func (Unimplemented) CompactMemory() error                       { return ErrNotImplemented }
func (Unimplemented) Unreportable() (uint64, error)              { return 0, ErrNotImplemented }
func (Unimplemented) SetReportingOrder(int) (int, error)         { return 0, ErrNotImplemented }
func (Unimplemented) RunningCache() ([]RunningCache, error) {
	return nil, ErrNotImplemented
}
func (Unimplemented) TrimRunning(string, uint64) error { return ErrNotImplemented }

type Server struct {
	Sys       System
	Log       *logbuf.Buffer
	Version   string
	EngineLog *logbuf.Buffer

	Fetch func(ctx context.Context, p api.FetchParams) (*api.FetchResult, error)
	Ping  func(ctx context.Context, p api.PingParams) (*api.PingResult, error)

	clock        sync.Mutex
	reclaim      sync.Mutex
	seen         map[string]cpuSeen
	rereads      map[string]bool
	lowered      bool
	restoreOrder int
	lowerings    uint64
	stats        sync.Mutex
	samplers     map[int]*sampler
	check        checks
	salvaging    atomic.Bool
	ops          operations
}

func NewServer(sys System, log *logbuf.Buffer, version string) *Server {
	return &Server{Sys: sys, Log: log, Version: version, Fetch: Fetch, Ping: Ping}
}

func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		s.ServeConn(conn)
	}
}

func (s *Server) ServeConn(rw net.Conn) *jsonrpc.Conn {
	return s.serveConn(rw, nil)
}

func (s *Server) serveConn(rw net.Conn, helloed func()) *jsonrpc.Conn {
	st := &connState{subs: map[uint64]context.CancelFunc{}, execs: map[uint64]bool{}, helloed: helloed}
	c := jsonrpc.NewConn(rw, jsonrpc.HandlerFunc(func(ctx context.Context, c *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
		slow := time.AfterFunc(slowRequest, func() { s.logStacks(method) })
		defer slow.Stop()
		return s.handle(ctx, c, st, method, params)
	}))
	context.AfterFunc(c.Context(), func() {
		st.mu.Lock()
		st.gone = true
		ids := st.execs
		st.execs = nil
		st.mu.Unlock()
		for id := range ids {
			s.Sys.ExecClose(id)
		}
	})
	return c
}

type connState struct {
	mu      sync.Mutex
	hello   bool
	client  string
	nextSub uint64
	subs    map[uint64]context.CancelFunc
	execs   map[uint64]bool
	gone    bool
	helloed func()
}

const (
	defaultDiagTimeout = 10 * time.Second
	maxDiagTimeout     = 60 * time.Second
	maxPingCount       = 20
)

func diagTimeout(ms int) time.Duration {
	d := time.Duration(ms) * time.Millisecond
	if d <= 0 {
		return defaultDiagTimeout
	}
	return min(d, maxDiagTimeout)
}

func failed(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotImplemented) {
		return jsonrpc.Errorf(api.CodeNotImplemented, "%v", err)
	}
	if errors.Is(err, ErrNoMachine) {
		return jsonrpc.Errorf(api.CodeNoMachine, "%v", err)
	}
	if errors.Is(err, api.ErrDebugToolsUnavailable) {
		return jsonrpc.Errorf(api.CodeDebugToolsUnavailable, "%v", err)
	}
	return jsonrpc.Errorf(api.CodeFailed, "%v", err)
}

const (
	maxEngineGrace = 10 * time.Minute
	maxExecArgs    = 4096
	maxExecBytes   = 256 << 10
	maxWindow      = 1<<16 - 1
	maxSignal      = 64
)

const progressWait = 100 * time.Millisecond

type queue struct {
	ch chan func()
}

func newQueue(ctx context.Context, depth int) *queue {
	q := &queue{ch: make(chan func(), depth)}
	go func() {
		for {
			select {
			case send := <-q.ch:
				send()
			case <-ctx.Done():
				return
			}
		}
	}()
	return q
}

func (q *queue) put(send func()) bool {
	select {
	case q.ch <- send:
		return true
	default:
		return false
	}
}

func graceOf(ms int) (time.Duration, error) {
	d := time.Duration(ms) * time.Millisecond
	if ms < 0 || d > maxEngineGrace {
		return 0, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "grace_ms must be between 0 and %d", maxEngineGrace.Milliseconds())
	}
	if d == 0 {
		d = api.DefaultEngineGraceMS * time.Millisecond
	}
	return d, nil
}

func checkExec(p api.ExecStartParams) error {
	invalid := func(format string, args ...any) error {
		return jsonrpc.Errorf(jsonrpc.CodeInvalidParams, format, args...)
	}
	if err := api.CheckDebugStart(p); err != nil {
		return invalid("%v", err)
	}
	switch p.Context {
	case api.ExecInit, api.ExecEngine:
		if p.Machine != "" || p.User != "" || p.Login || p.Shell || p.Subsystem != "" || p.Copy != "" {
			return invalid("machine, user, login, shell, subsystem and copy are for context %s", api.ExecMachine)
		}
	case api.ExecDebug:
		if p.Machine != "" || p.User != "" || p.Login || p.Shell || p.Subsystem != "" || p.Copy != "" {
			return invalid("machine, user, login, shell, subsystem and copy are for context %s", api.ExecMachine)
		}
	case api.ExecMachine:
		if err := api.CheckMachineName(p.Machine); err != nil {
			return invalid("%v", err)
		}
		if strings.ContainsAny(p.User, ":\n/") {
			return invalid("user %q is not a name", p.User)
		}
		if p.Login && p.Shell {
			if len(p.Argv) == 0 {
				return invalid("login and shell together need a command line")
			}
		} else if p.Login {
			if len(p.Argv) != 0 {
				return invalid("a login has no argv: the user's shell is its program")
			}
			p.Argv = []string{"-"}
		}
		if p.Subsystem != "" {
			if p.Subsystem != api.SubsystemSFTP {
				return invalid("no subsystem %q (have: %s)", p.Subsystem, api.SubsystemSFTP)
			}
			if len(p.Argv) != 0 || p.Login || p.Shell || p.TTY || p.StdinTTY {
				return invalid("a subsystem has no argv, login, shell or terminal")
			}
			p.Argv = []string{p.Subsystem}
		}
		if err := api.CheckExecCopy(p); err != nil {
			return invalid("%v", err)
		}
	default:
		return invalid("unknown context %q (have: %s, %s, %s, %s)", p.Context, api.ExecInit, api.ExecEngine, api.ExecMachine, api.ExecDebug)
	}
	if p.Context != api.ExecDebug && (len(p.Argv) == 0 || p.Argv[0] == "") {
		return invalid("argv needs a program")
	}
	if len(p.Argv) > 0 && p.Argv[0] == "" {
		return invalid("argv needs a program")
	}
	if len(p.Argv)+len(p.Env) > maxExecArgs {
		return invalid("more than %d arguments and environment entries", maxExecArgs)
	}
	size := len(p.Dir)
	for _, list := range [][]string{p.Argv, p.Env, {p.Dir}} {
		for _, v := range list {
			size += len(v) + 1
			if strings.IndexByte(v, 0) >= 0 {
				return invalid("argv, env, and dir must not contain NUL")
			}
		}
	}
	if size > maxExecBytes {
		return invalid("argv and env together exceed %d bytes", maxExecBytes)
	}
	for _, e := range p.Env {
		if i := strings.IndexByte(e, '='); i <= 0 {
			return invalid("env entry %q is not NAME=value", e)
		}
	}
	if p.Dir != "" && !strings.HasPrefix(p.Dir, "/") {
		return invalid("dir must be an absolute path")
	}
	if p.Rows < 0 || p.Rows > maxWindow || p.Cols < 0 || p.Cols > maxWindow {
		return invalid("rows and cols must be between 0 and %d", maxWindow)
	}
	if p.TTY && p.StdinTTY {
		return invalid("tty and stdin_tty together")
	}
	if !p.TTY && !p.StdinTTY && (p.Rows != 0 || p.Cols != 0) {
		return invalid("rows and cols need tty")
	}
	if err := api.CheckExecTerminfo(p); err != nil {
		return invalid("%v", err)
	}
	return nil
}

func (s *Server) handle(ctx context.Context, c *jsonrpc.Conn, st *connState, method string, params json.RawMessage) (any, error) {
	if method == api.MethodHello {
		p, err := jsonrpc.Bind[api.HelloParams](params)
		if err != nil {
			return nil, err
		}
		if p.APIVersion != api.APIVersion {
			jsonrpc.AfterReply(ctx, func() { c.Close() })
			return nil, jsonrpc.Errorf(api.CodeVersionMismatch,
				"agent speaks API version %d, client speaks %d", api.APIVersion, p.APIVersion)
		}
		if p.Build != "" && p.Build != s.Version {
			s.Log.Logf(logbuf.Warning, "agent: refused client %q from build %q; this is %q", p.Client, p.Build, s.Version)
			jsonrpc.AfterReply(ctx, func() { c.Close() })
			data, _ := json.Marshal(api.BuildMismatch{AgentBuild: s.Version, ClientBuild: p.Build})
			return nil, &jsonrpc.Error{Code: api.CodeBuildMismatch, Data: data, Message: (&version.Mismatch{
				Ours: s.Version, Theirs: p.Build, Peer: "client " + p.Client}).Error()}
		}
		st.mu.Lock()
		first := !st.hello
		st.hello, st.client = true, p.Client
		st.mu.Unlock()
		if first {
			s.Log.Logf(logbuf.Debug, "agent: client %q connected", p.Client)
			if st.helloed != nil {
				jsonrpc.AfterReply(ctx, func() { go st.helloed() })
			}
		}
		return api.HelloResult{APIVersion: api.APIVersion, AgentVersion: s.Version, Kernel: s.Sys.Kernel()}, nil
	}

	st.mu.Lock()
	hello := st.hello
	st.mu.Unlock()
	if !hello {
		return nil, jsonrpc.Errorf(api.CodeHelloRequired, "%s must be the first call", api.MethodHello)
	}

	switch method {
	case api.MethodPing:
		return api.Empty{}, nil

	case api.MethodInfo:
		info, err := s.Sys.Info()
		return info, failed(err)

	case api.MethodBootTrace:
		p, err := jsonrpc.Bind[api.BootTraceParams](params)
		if err != nil {
			return nil, err
		}
		if p.Wait {
			select {
			case <-s.Sys.Ready():
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return s.Sys.BootTrace(), nil

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
		deadline := time.Duration(p.DeadlineMS) * time.Millisecond
		s.Log.Logf(logbuf.Info, "agent: shutdown requested by %q (reboot=%v, deadline %v)", st.client, p.Reboot, deadline)
		q := newQueue(c.Context(), 16)
		var stalled atomic.Bool
		progress := func(ev api.ShutdownProgress) {
			sent := make(chan struct{})
			if !q.put(func() { c.Notify(api.NotifyShutdownProgress, ev); close(sent) }) || stalled.Load() {
				return
			}
			t := time.NewTimer(progressWait)
			defer t.Stop()
			select {
			case <-sent:
			case <-t.C:
				stalled.Store(true)
			}
		}
		jsonrpc.AfterReply(ctx, func() {
			go func() {
				if err := s.Sys.Shutdown(p.Reboot, deadline, progress); err != nil {
					s.Log.Logf(logbuf.Error, "agent: shutdown failed: %v", err)
				}
			}()
		})
		return api.Empty{}, nil

	case api.MethodEngineStatus:
		status, err := s.Sys.EngineStatus()
		return status, failed(err)

	case api.MethodEngineStart:
		s.Log.Logf(logbuf.Info, "agent: engine start requested by %q", st.client)
		status, err := s.Sys.EngineStart()
		return status, failed(err)

	case api.MethodEngineStop, api.MethodEngineRestart:
		p, err := jsonrpc.Bind[api.EngineStopParams](params)
		if err != nil {
			return nil, err
		}
		grace, err := graceOf(p.GraceMS)
		if err != nil {
			return nil, err
		}
		s.Log.Logf(logbuf.Info, "agent: %s requested by %q (grace %v)", method, st.client, grace)
		if method == api.MethodEngineStop {
			status, err := s.Sys.EngineStop(grace)
			return status, failed(err)
		}
		status, err := s.Sys.EngineRestart(grace)
		return status, failed(err)

	case api.MethodEngineSubscribe:
		return s.subscribeEngine(ctx, c, st)

	case api.MethodEngineConfigure:
		p, err := jsonrpc.Bind[api.EngineConfigureParams](params)
		if err != nil {
			return nil, err
		}
		var config map[string]json.RawMessage
		if err := json.Unmarshal(p.Config, &config); err != nil || config == nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "config must be a JSON object; {} means none")
		}
		r, err := s.Sys.EngineConfigure(config)
		if err != nil {
			return nil, failed(err)
		}
		s.Log.Logf(logbuf.Info, "agent: engine configuration from %q: %s %s", st.client, r.State, r.Error)
		return r, nil

	case api.MethodFileShareWatch:
		p, err := jsonrpc.Bind[api.WatchParams](params)
		if err != nil {
			return nil, err
		}
		if p.Paths == nil {
			p.Paths = []string{}
		}
		if _, err := fsevents.Append(nil, fsevents.Watch{Seq: 1, Paths: p.Paths}); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
		status, err := s.Sys.FileShareWatch(ctx, p.Paths)
		if err != nil {
			return nil, failed(err)
		}
		if len(status) != len(p.Paths) {
			return nil, jsonrpc.Errorf(api.CodeFailed, "the VMM answered for %d paths of %d", len(status), len(p.Paths))
		}
		r := api.WatchResult{}
		for i, st := range status {
			switch st {
			case fsevents.StatusOK:
				r.Watching++
			case fsevents.StatusOutside:
				r.Rejected = append(r.Rejected, api.WatchRejected{Path: p.Paths[i], Reason: api.WatchOutside})
			case fsevents.StatusMissing:
				r.Rejected = append(r.Rejected, api.WatchRejected{Path: p.Paths[i], Reason: api.WatchMissing})
			default:
				r.Rejected = append(r.Rejected, api.WatchRejected{Path: p.Paths[i], Reason: api.WatchFailed})
			}
		}
		return r, nil

	case api.MethodStreamRefuse:
		p, err := jsonrpc.Bind[api.StreamRefuseParams](params)
		if err != nil {
			return nil, err
		}
		if len(p.Token) > 64 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "token is too long")
		}
		s.Sys.StreamRefuse(p.Token, clip(p.Reason, 200))
		return api.Empty{}, nil

	case api.MethodEngineUnsubscribe:
		p, err := jsonrpc.Bind[api.Subscription](params)
		if err != nil {
			return nil, err
		}
		return api.Empty{}, st.cancel(p.Subscription)

	case api.MethodTimeSet:
		p, err := jsonrpc.Bind[api.TimeSetParams](params)
		if err != nil {
			return nil, err
		}
		return s.timeSet(st, p)

	case api.MethodMemoryReclaim:
		p, err := jsonrpc.Bind[api.ReclaimParams](params)
		if err != nil {
			return nil, err
		}
		return s.memoryReclaim(st, p)

	case api.MethodStorageInfo:
		info, err := s.Sys.StorageInfo()
		return info, failed(err)

	case api.MethodStorageTrim:
		p, err := jsonrpc.Bind[api.TrimParams](params)
		if err != nil {
			return nil, err
		}
		r, err := s.Sys.StorageTrim(p.MinLenBytes)
		return r, failed(err)

	case api.MethodStorageBudget:
		p, err := jsonrpc.Bind[api.BudgetParams](params)
		if err != nil {
			return nil, err
		}
		if p.AvailableBytes < 0 || p.MarginBytes < 0 || p.ImageBytes < 0 || p.ReclaimBytes < 0 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "storage.budget: a figure is negative: %+v", p)
		}
		r, err := s.Sys.StorageBudget(p)
		return r, failed(err)

	case api.MethodStorageCheck:
		p, err := jsonrpc.Bind[api.CheckParams](params)
		if err != nil {
			return nil, err
		}
		return s.storageCheck(ctx, c, st, p.Follow)

	case api.MethodStorageSalvage:
		p, err := jsonrpc.Bind[api.SalvageParams](params)
		if err != nil {
			return nil, err
		}
		return s.storageSalvage(ctx, c, st, p)

	case api.MethodStorageUsage:
		p, err := jsonrpc.Bind[api.UsageParams](params)
		if err != nil {
			return nil, err
		}
		return s.storageUsage(p)

	case api.MethodStorageSync:
		began := time.Now()
		if err := s.Sys.StorageSync(); err != nil {
			return nil, failed(err)
		}
		r := api.Synced{DurationMS: float64(time.Since(began).Microseconds()) / 1000}
		s.Log.Logf(logbuf.Debug, "agent: the data disk was synced for %q in %.1f ms", st.client, r.DurationMS)
		return r, nil

	case api.MethodFileShareBind:
		var p api.ShareBindParams
		if len(params) > 0 && string(params) != "null" {
			bound, err := jsonrpc.Bind[api.ShareBindParams](params)
			if err != nil {
				return nil, err
			}
			p = bound
		}
		return s.bindShare(p)

	case api.MethodStatsSubscribe:
		p, err := jsonrpc.Bind[api.StatsSubscribeParams](params)
		if err != nil {
			return nil, err
		}
		return s.subscribeStats(ctx, c, st, p)

	case api.MethodStatsUnsubscribe:
		p, err := jsonrpc.Bind[api.Subscription](params)
		if err != nil {
			return nil, err
		}
		return api.Empty{}, st.cancel(p.Subscription)

	case api.MethodExecStart:
		p, err := jsonrpc.Bind[api.ExecStartParams](params)
		if err != nil {
			return nil, err
		}
		if err := checkExec(p); err != nil {
			return nil, err
		}
		return s.startExec(c, st, p)

	case api.MethodExecResize:
		p, err := jsonrpc.Bind[api.ExecResizeParams](params)
		if err != nil {
			return nil, err
		}
		if p.Rows < 1 || p.Rows > maxWindow || p.Cols < 1 || p.Cols > maxWindow {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "rows and cols must be between 1 and %d", maxWindow)
		}
		if err := st.owns(p.ID); err != nil {
			return nil, err
		}
		return api.Empty{}, failed(s.Sys.ExecResize(p.ID, p.Rows, p.Cols))

	case api.MethodExecSignal:
		p, err := jsonrpc.Bind[api.ExecSignalParams](params)
		if err != nil {
			return nil, err
		}
		if p.Signal < 1 || p.Signal > maxSignal {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "signal must be between 1 and %d", maxSignal)
		}
		if err := st.owns(p.ID); err != nil {
			return nil, err
		}
		return api.Empty{}, failed(s.Sys.ExecSignal(p.ID, p.Signal))

	case api.MethodExecClose:
		p, err := jsonrpc.Bind[api.ExecID](params)
		if err != nil {
			return nil, err
		}
		if err := st.owns(p.ID); err != nil {
			return nil, err
		}
		st.mu.Lock()
		delete(st.execs, p.ID)
		st.mu.Unlock()
		return api.Empty{}, failed(s.Sys.ExecClose(p.ID))

	case api.MethodLogSubscribe:
		p, err := jsonrpc.Bind[api.LogSubscribeParams](params)
		if err != nil {
			return nil, err
		}
		return s.subscribeLogs(ctx, c, st, p)

	case api.MethodLogUnsubscribe:
		p, err := jsonrpc.Bind[api.Subscription](params)
		if err != nil {
			return nil, err
		}
		return api.Empty{}, st.cancel(p.Subscription)

	case api.MethodFetch:
		p, err := jsonrpc.Bind[api.FetchParams](params)
		if err != nil {
			return nil, err
		}
		fctx, cancel := context.WithTimeout(ctx, diagTimeout(p.TimeoutMS))
		defer cancel()
		r, err := s.Fetch(fctx, p)
		return r, failed(err)

	case api.MethodPingHost:
		p, err := jsonrpc.Bind[api.PingParams](params)
		if err != nil {
			return nil, err
		}
		if p.Count <= 0 {
			p.Count = 1
		}
		if p.Count > maxPingCount || p.TTL < 0 || p.TTL > 255 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "count must be at most %d and ttl between 0 and 255", maxPingCount)
		}
		pctx, cancel := context.WithTimeout(ctx, diagTimeout(p.TimeoutMS))
		defer cancel()
		r, err := s.Ping(pctx, p)
		return r, failed(err)
	}
	if r, done, err := s.handleDomains(method, params); done {
		return r, err
	}
	if r, done, err := s.handleView(method, params); done {
		return r, err
	}
	if r, done, err := s.handleEmulation(method, params); done {
		return r, err
	}
	if r, done, err := s.handleK8s(ctx, c, method, params); done {
		return r, err
	}
	if r, done, err := s.handleK8sCRI(method, params); done {
		return r, err
	}
	if r, done, err := s.handleDebug(method, params); done {
		return r, err
	}
	if r, done, err := s.handleMachineOps(ctx, c, method, params, st); done {
		return r, err
	}
	if r, done, err := s.handleVolumes(ctx, c, method, params, st); done {
		return r, err
	}
	if r, done, err := s.handleMachines(method, params, st); done {
		return r, err
	}
	if types, ok := api.Pending[method]; ok && types.Result != nil {
		return nil, failed(ErrNotImplemented)
	}
	return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method %q not found", method)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

type HostLink struct {
	Conn *jsonrpc.Conn
	q    *queue
}

const hostLinkDepth = api.MaxStreamRequests

func (s *Server) ServeHost(rw net.Conn) *HostLink {
	h := &HostLink{}
	set := make(chan struct{})
	c := s.serveConn(rw, func() {
		<-set
		stop := s.Sys.FileShareFollow(func(share api.FileShare) {
			h.Notify(api.NotifyFileShareState, api.FileShareState{FileShare: share})
		})
		context.AfterFunc(h.Conn.Context(), stop)
	})
	h.Conn, h.q = c, newQueue(c.Context(), hostLinkDepth)
	close(set)
	return h
}

func (h *HostLink) Notify(method string, params any) bool {
	if h.Conn.Context().Err() != nil {
		return false
	}
	return h.q.put(func() { h.Conn.Notify(method, params) })
}

func (st *connState) cancel(id uint64) error {
	st.mu.Lock()
	cancel := st.subs[id]
	delete(st.subs, id)
	st.mu.Unlock()
	if cancel == nil {
		return jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "no subscription %d on this connection", id)
	}
	cancel()
	return nil
}

func (st *connState) owns(id uint64) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.execs[id] {
		return jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "no session %d on this connection", id)
	}
	return nil
}

const engineQueueDepth = 64

func (s *Server) subscribeEngine(ctx context.Context, c *jsonrpc.Conn, st *connState) (any, error) {
	subCtx, cancel := context.WithCancel(c.Context())
	st.mu.Lock()
	st.nextSub++
	id := st.nextSub
	st.mu.Unlock()

	q := newQueue(subCtx, engineQueueDepth)
	replied := make(chan struct{})
	q.put(func() {
		select {
		case <-replied:
		case <-subCtx.Done():
		}
	})
	stop, err := s.Sys.EngineSubscribe(func(status api.EngineStatus) {
		ok := q.put(func() {
			c.Notify(api.NotifyEngineState, api.EngineState{Subscription: id, EngineStatus: status})
		})
		if !ok && subCtx.Err() == nil {
			s.Log.Logf(logbuf.Warning, "agent: client %q is not reading engine states; disconnecting it", st.client)
			c.Close()
		}
	})
	if err != nil {
		cancel()
		return nil, failed(err)
	}
	st.mu.Lock()
	st.subs[id] = cancel
	st.mu.Unlock()
	context.AfterFunc(subCtx, stop)
	jsonrpc.AfterReply(ctx, func() { close(replied) })
	return api.Subscription{Subscription: id}, nil
}

type sampler struct {
	stop chan struct{}
	subs map[*statsSub]bool
}

type statsSub struct {
	id uint64
	q  *queue
	c  *jsonrpc.Conn
}

func (sub *statsSub) send(sample api.StatsSample) {
	sample.Subscription = sub.id
	sub.q.put(func() { sub.c.Notify(api.NotifyStatsSample, sample) })
}

const statsQueueDepth = 5

func (s *Server) subscribeStats(ctx context.Context, c *jsonrpc.Conn, st *connState, p api.StatsSubscribeParams) (any, error) {
	if p.IntervalMS < 0 || (p.IntervalMS != 0 && p.IntervalMS < api.MinStatsIntervalMS) || p.IntervalMS > maxStatsIntervalMS {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "interval_ms must be between %d and %d", api.MinStatsIntervalMS, maxStatsIntervalMS)
	}
	if p.IntervalMS == 0 {
		p.IntervalMS = api.DefaultStatsIntervalMS
	}
	first, err := s.Sys.StatsSample()
	if err != nil {
		return nil, failed(err)
	}
	subCtx, cancel := context.WithCancel(c.Context())
	st.mu.Lock()
	st.nextSub++
	id := st.nextSub
	st.subs[id] = cancel
	st.mu.Unlock()
	sub := &statsSub{id: id, q: newQueue(subCtx, statsQueueDepth), c: c}
	replied := make(chan struct{})
	sub.q.put(func() {
		select {
		case <-replied:
		case <-subCtx.Done():
		}
	})
	jsonrpc.AfterReply(ctx, func() { close(replied) })
	sub.send(first)

	s.stats.Lock()
	if s.samplers == nil {
		s.samplers = map[int]*sampler{}
	}
	sm := s.samplers[p.IntervalMS]
	if sm == nil {
		sm = &sampler{stop: make(chan struct{}), subs: map[*statsSub]bool{}}
		s.samplers[p.IntervalMS] = sm
		go s.sample(sm, time.Duration(p.IntervalMS)*time.Millisecond)
	}
	sm.subs[sub] = true
	s.stats.Unlock()
	context.AfterFunc(subCtx, func() {
		s.stats.Lock()
		delete(sm.subs, sub)
		last := len(sm.subs) == 0 && s.samplers[p.IntervalMS] == sm
		if last {
			delete(s.samplers, p.IntervalMS)
			close(sm.stop)
		}
		s.stats.Unlock()
		if last {
			s.Log.Logf(logbuf.Debug, "agent: nobody is subscribed to stats at %d ms any more; the reading has stopped", p.IntervalMS)
		}
	})
	s.Log.Logf(logbuf.Debug, "agent: stats stream %d for %q, a sample every %d ms", id, st.client, p.IntervalMS)
	return api.Subscription{Subscription: id}, nil
}

const maxStatsIntervalMS = 3600 * 1000

func (s *Server) sample(sm *sampler, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-sm.stop:
			return
		case <-tick.C:
		}
		reading, err := s.Sys.StatsSample()
		if err != nil {
			s.Log.Logf(logbuf.Warning, "agent: a stats sample could not be read: %v", err)
			continue
		}
		s.stats.Lock()
		for sub := range sm.subs {
			sub.send(reading)
		}
		s.stats.Unlock()
	}
}

func (s *Server) startExec(c *jsonrpc.Conn, st *connState, p api.ExecStartParams) (any, error) {
	var id uint64
	known := make(chan struct{})
	started, err := s.Sys.ExecStart(p, func(exit api.ExecExit) {
		go func() {
			<-known
			st.mu.Lock()
			owned := st.execs[id]
			delete(st.execs, id)
			st.mu.Unlock()
			if owned {
				c.Notify(api.NotifyExecExit, exit)
			}
		}()
	})
	if err != nil {
		close(known)
		return nil, failed(err)
	}
	id = started.ID
	st.mu.Lock()
	gone := st.gone
	if !gone {
		st.execs[id] = true
	}
	st.mu.Unlock()
	close(known)
	if gone {
		s.Sys.ExecClose(id)
		return nil, jsonrpc.ErrClosed
	}
	what := "a login shell"
	if len(p.Argv) > 0 {
		what = strconv.Quote(p.Argv[0])
	}
	s.Log.Logf(logbuf.Debug, "agent: session %d for %q in context %s: %s", id, st.client, p.Context, what)
	return started, nil
}

func (s *Server) subscribeLogs(ctx context.Context, c *jsonrpc.Conn, st *connState, p api.LogSubscribeParams) (any, error) {
	want := map[string]bool{}
	type machineLog struct {
		source, name string
		runtime      bool
	}
	var machines []machineLog
	for _, src := range p.Sources {
		if src == api.LogEngine && s.EngineLog == nil {
			return nil, failed(ErrNotImplemented)
		}
		if name, runtime, ok := machineSource(src); ok {
			if err := api.CheckMachineName(name); err != nil {
				return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "log source %q: %v", src, err)
			}
			_, ok1 := s.Sys.(MachineLogs)
			mi, ok2 := s.Sys.(Machines)
			if !ok1 || !ok2 {
				return nil, failed(ErrNotImplemented)
			}
			if _, err := mi.MachineInfo(name); err != nil {
				return nil, failed(err)
			}
			if !want[src] {
				machines = append(machines, machineLog{src, name, runtime})
			}
			want[src] = true
			continue
		}
		if src != api.LogKernel && src != api.LogInit && src != api.LogEngine {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "unknown log source %q", src)
		}
		want[src] = true
	}
	if len(want) == 0 {
		want[api.LogKernel], want[api.LogInit] = true, true
		want[api.LogEngine] = s.EngineLog != nil
	}

	subCtx, cancel := context.WithCancel(c.Context())
	st.mu.Lock()
	st.nextSub++
	id := st.nextSub
	st.subs[id] = cancel
	st.mu.Unlock()

	send := func(source string, timeMS float64, level int, text string) {
		c.Notify(api.NotifyLogEntry, api.LogEntry{Subscription: id, Source: source, TimeMS: timeMS, Level: level, Text: text})
	}
	var markMu sync.Mutex
	pending := 0
	for _, on := range want {
		if on {
			pending++
		}
	}
	sourceDone := func() func() {
		var once sync.Once
		return func() {
			once.Do(func() {
				markMu.Lock()
				pending--
				last := pending == 0
				markMu.Unlock()
				if last && p.Backlog && subCtx.Err() == nil {
					c.Notify(api.NotifyLogBacklogEnd, api.Subscription{Subscription: id})
				}
			})
		}
	}
	stopWith := func(stop func()) {
		go func() {
			<-subCtx.Done()
			stop()
		}()
	}
	jsonrpc.AfterReply(ctx, func() {
		if want[api.LogInit] {
			stopWith(s.Log.SubscribeMarked(p.Backlog, func(e logbuf.Entry) {
				send(api.LogInit, e.TimeMS, e.Level, e.Text)
			}, sourceDone()))
		}
		if want[api.LogEngine] {
			stopWith(s.EngineLog.SubscribeMarked(p.Backlog, func(e logbuf.Entry) {
				send(api.LogEngine, e.TimeMS, e.Level, e.Text)
			}, sourceDone()))
		}
		if want[api.LogKernel] {
			done := sourceDone()
			go func() {
				defer done()
				err := s.Sys.KernelLog(subCtx, p.Backlog, func(timeMS float64, level int, text string) {
					send(api.LogKernel, timeMS, level, text)
				}, done)
				if err != nil && subCtx.Err() == nil {
					s.Log.Logf(logbuf.Warning, "agent: kernel log stream ended: %v", err)
				}
			}()
		}
		for _, m := range machines {
			done := sourceDone()
			stop, err := s.Sys.(MachineLogs).MachineLogSubscribe(m.name, m.runtime, p.Backlog, func(e logbuf.Entry) {
				send(m.source, e.TimeMS, e.Level, e.Text)
			}, done)
			if err != nil {
				s.Log.Logf(logbuf.Warning, "agent: log source %s: %v", m.source, err)
				done()
				continue
			}
			stopWith(stop)
		}
	})
	return api.Subscription{Subscription: id}, nil
}

const (
	slowRequest = 120 * time.Second
	stacksEvery = 10 * time.Minute
)

var lastStacks atomic.Int64

func (s *Server) logStacks(method string) {
	now := time.Now().UnixNano()
	last := lastStacks.Load()
	if last != 0 && now-last < int64(stacksEvery) {
		return
	}
	if !lastStacks.CompareAndSwap(last, now) {
		return
	}
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	s.Log.Logf(logbuf.Warning, "agent: a %s request has run for %v; the guest's goroutines:\n%s", method, slowRequest, buf[:n])
}
