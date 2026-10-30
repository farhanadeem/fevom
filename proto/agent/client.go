package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"fev/proto/jsonrpc"
	"fev/proto/version"
)

type Client struct {
	conn *jsonrpc.Conn

	mu             sync.Mutex
	logs           map[uint64]func(LogEntry)
	logEnds        map[uint64]func()
	engine         map[uint64]func(EngineStatus)
	execs          map[uint64]func(ExecExit)
	progress       func(ShutdownProgress)
	streams        func(StreamRequest)
	machineStreams func(StreamRequest)
	tlsStreams     func(StreamRequest)
	stats          map[uint64]func(StatsSample)
	ops            map[uint64]func(OperationProgress)
	machines       func(MachineState)
	k8sState       func(K8sStatus)
	views          func(ViewStatus)
	viewRemoved    func(ViewRemoved)
	ports          func(MachinePorts)
	enginePorts    func(EnginePorts)

	shareMu sync.Mutex
	share   func(FileShare)
	early   *FileShare
}

func NewClient(ctx context.Context, rw io.ReadWriteCloser, name string) (*Client, *HelloResult, error) {
	c := &Client{
		logs:    make(map[uint64]func(LogEntry)),
		logEnds: make(map[uint64]func()),
		engine:  make(map[uint64]func(EngineStatus)),
		execs:   make(map[uint64]func(ExecExit)),
		stats:   make(map[uint64]func(StatsSample)),
		ops:     make(map[uint64]func(OperationProgress)),
	}
	c.conn = jsonrpc.NewConn(rw, jsonrpc.HandlerFunc(c.handle))
	var hello HelloResult
	err := c.conn.Call(ctx, MethodHello, HelloParams{APIVersion: APIVersion, Build: version.Build, Client: name}, &hello)
	if err == nil {
		err = version.Check(version.Build, hello.AgentVersion, "guest agent")
	} else if m := mismatch(err); m != nil {
		err = m
	}
	if err != nil {
		c.conn.Close()
		return nil, nil, fmt.Errorf("agent hello: %w", err)
	}
	return c, &hello, nil
}

func mismatch(err error) error {
	var e *jsonrpc.Error
	if !errors.As(err, &e) || e.Code != CodeBuildMismatch {
		return nil
	}
	var data BuildMismatch
	if json.Unmarshal(e.Data, &data) != nil || data.AgentBuild == "" {
		return nil
	}
	return &version.Mismatch{Ours: data.ClientBuild, Theirs: data.AgentBuild, Peer: "guest agent"}
}

func (c *Client) handle(ctx context.Context, _ *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
	switch method {
	case NotifyLogEntry:
		entry, err := jsonrpc.Bind[LogEntry](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		deliver := c.logs[entry.Subscription]
		c.mu.Unlock()
		if deliver != nil {
			deliver(entry)
		}
	case NotifyLogBacklogEnd:
		sub, err := jsonrpc.Bind[Subscription](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		end := c.logEnds[sub.Subscription]
		delete(c.logEnds, sub.Subscription)
		c.mu.Unlock()
		if end != nil {
			end()
		}
	case NotifyEngineState:
		state, err := jsonrpc.Bind[EngineState](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		deliver := c.engine[state.Subscription]
		c.mu.Unlock()
		if deliver != nil {
			deliver(state.EngineStatus)
		}
	case NotifyExecExit:
		exit, err := jsonrpc.Bind[ExecExit](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		deliver := c.execs[exit.ID]
		delete(c.execs, exit.ID)
		c.mu.Unlock()
		if deliver != nil {
			deliver(exit)
		}
	case NotifyShutdownProgress:
		p, err := jsonrpc.Bind[ShutdownProgress](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		deliver := c.progress
		c.mu.Unlock()
		if deliver != nil {
			deliver(p)
		}
	case NotifyStreamRequest:
		req, err := jsonrpc.Bind[StreamRequest](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		deliver := c.streams
		switch {
		case req.Machine != "":
			deliver = c.machineStreams
		case req.Service == StreamTLSIssue:
			deliver = c.tlsStreams
		}
		c.mu.Unlock()
		if deliver != nil {
			deliver(req)
		}
	case NotifyStatsSample:
		sample, err := jsonrpc.Bind[StatsSample](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		deliver := c.stats[sample.Subscription]
		c.mu.Unlock()
		if deliver != nil {
			deliver(sample)
		}
	case NotifyOperationProgress:
		p, err := jsonrpc.Bind[OperationProgress](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		deliver := c.ops[p.ID]
		if p.State != OperationRunning {
			delete(c.ops, p.ID)
		}
		c.mu.Unlock()
		if deliver != nil {
			deliver(p)
		}
	case NotifyK8sState:
		st, err := jsonrpc.Bind[K8sStatus](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		deliver := c.k8sState
		c.mu.Unlock()
		if deliver != nil {
			deliver(st)
		}
	case NotifyMachineState:
		st, err := jsonrpc.Bind[MachineState](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		deliver := c.machines
		c.mu.Unlock()
		if deliver != nil {
			deliver(st)
		}
	case NotifyViewChanged:
		st, err := jsonrpc.Bind[ViewStatus](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		deliver := c.views
		c.mu.Unlock()
		if deliver != nil {
			deliver(st)
		}
	case NotifyViewRemoved:
		r, err := jsonrpc.Bind[ViewRemoved](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		deliver := c.viewRemoved
		c.mu.Unlock()
		if deliver != nil {
			deliver(r)
		}
	case NotifyMachinePorts:
		p, err := jsonrpc.Bind[MachinePorts](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		deliver := c.ports
		c.mu.Unlock()
		if deliver != nil {
			deliver(p)
		}
	case NotifyEnginePorts:
		p, err := jsonrpc.Bind[EnginePorts](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		deliver := c.enginePorts
		c.mu.Unlock()
		if deliver != nil {
			deliver(p)
		}
	case NotifyFileShareState:
		state, err := jsonrpc.Bind[FileShareState](params)
		if err != nil {
			return nil, err
		}
		c.shareMu.Lock()
		if c.share != nil {
			c.share(state.FileShare)
		} else {
			c.early = &state.FileShare
		}
		c.shareMu.Unlock()
	default:
		return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method %q not found", method)
	}
	return nil, nil
}

func (c *Client) OnFileShareState(deliver func(FileShare)) {
	c.shareMu.Lock()
	defer c.shareMu.Unlock()
	c.share = deliver
	if c.early != nil && deliver != nil {
		deliver(*c.early)
		c.early = nil
	}
}

func (c *Client) TimeSet(ctx context.Context, unixNS int64) (*TimeSet, error) {
	var r TimeSet
	return &r, c.conn.Call(ctx, MethodTimeSet, TimeSetParams{UnixNS: unixNS}, &r)
}

func (c *Client) TimeSetWithin(ctx context.Context, unixNS int64, thresholdMS int) (*TimeSet, error) {
	var r TimeSet
	return &r, c.conn.Call(ctx, MethodTimeSet, TimeSetParams{UnixNS: unixNS, ThresholdMS: thresholdMS}, &r)
}

func (c *Client) MemoryReclaim(ctx context.Context, p ReclaimParams) (*Reclaimed, error) {
	var r Reclaimed
	return &r, c.conn.Call(ctx, MethodMemoryReclaim, p, &r)
}

func (c *Client) SubscribeStats(ctx context.Context, intervalMS int, deliver func(StatsSample)) (func(), error) {
	var sub Subscription
	err := c.conn.CallThen(ctx, MethodStatsSubscribe, StatsSubscribeParams{IntervalMS: intervalMS}, &sub, func() {
		c.mu.Lock()
		c.stats[sub.Subscription] = deliver
		c.mu.Unlock()
	})
	if err != nil {
		return nil, err
	}
	return func() {
		c.mu.Lock()
		delete(c.stats, sub.Subscription)
		c.mu.Unlock()
		c.conn.Call(context.Background(), MethodStatsUnsubscribe, sub, nil)
	}, nil
}

func IsNotImplemented(err error) bool {
	var e *jsonrpc.Error
	return errors.As(err, &e) && e.Code == CodeNotImplemented
}

func IsNoDataFilesystem(err error) bool {
	var e *jsonrpc.Error
	return errors.As(err, &e) && e.Code == CodeFailed && strings.HasPrefix(e.Message, NoDataFilesystem)
}

func IsDataReadOnly(err error) bool {
	var e *jsonrpc.Error
	return errors.As(err, &e) && e.Code == CodeFailed && strings.HasPrefix(e.Message, DataReadOnly)
}

func (c *Client) OnStreamRequest(deliver func(StreamRequest)) {
	c.mu.Lock()
	c.streams = deliver
	c.mu.Unlock()
}

func (c *Client) OnMachineStreamRequest(deliver func(StreamRequest)) {
	c.mu.Lock()
	c.machineStreams = deliver
	c.mu.Unlock()
}

func (c *Client) OnTLSRequest(deliver func(StreamRequest)) {
	c.mu.Lock()
	c.tlsStreams = deliver
	c.mu.Unlock()
}

func (c *Client) DomainsSet(ctx context.Context, p DomainsParams) (*DomainsApplied, error) {
	var r DomainsApplied
	return &r, c.conn.Call(ctx, MethodDomainsSet, p, &r)
}

func (c *Client) MacAddressesSet(ctx context.Context, addresses []string) error {
	if addresses == nil {
		addresses = []string{}
	}
	return c.conn.Call(ctx, MethodMacAddressesSet, MacAddresses{Addresses: addresses}, nil)
}

func (c *Client) StreamRefuse(ctx context.Context, token, reason string) error {
	return c.conn.Call(ctx, MethodStreamRefuse, StreamRefuseParams{Token: token, Reason: reason}, nil)
}

func (c *Client) FileShareBind(ctx context.Context) (*ShareBound, error) {
	return c.FileShareBindIn(ctx, nil)
}

func (c *Client) FileShareBindIn(ctx context.Context, targets []BindTarget) (*ShareBound, error) {
	var r ShareBound
	return &r, c.conn.Call(ctx, MethodFileShareBind, ShareBindParams{Targets: targets}, &r)
}

func (c *Client) FileShareWatch(ctx context.Context, paths []string) (*WatchResult, error) {
	if paths == nil {
		paths = []string{}
	}
	var r WatchResult
	return &r, c.conn.Call(ctx, MethodFileShareWatch, WatchParams{Paths: paths}, &r)
}

func (c *Client) EngineConfigure(ctx context.Context, config json.RawMessage) (*EngineConfigured, error) {
	if len(config) == 0 {
		config = json.RawMessage("{}")
	}
	var r EngineConfigured
	return &r, c.conn.Call(ctx, MethodEngineConfigure, EngineConfigureParams{Config: config}, &r)
}

func (c *Client) Close() error          { return c.conn.Close() }
func (c *Client) Done() <-chan struct{} { return c.conn.Done() }

func (c *Client) Ping(ctx context.Context) error {
	return c.conn.Call(ctx, MethodPing, nil, nil)
}

func (c *Client) Info(ctx context.Context) (*Info, error) {
	var r Info
	return &r, c.conn.Call(ctx, MethodInfo, nil, &r)
}

func (c *Client) BootTrace(ctx context.Context, wait bool) (*BootTrace, error) {
	var r BootTrace
	return &r, c.conn.Call(ctx, MethodBootTrace, BootTraceParams{Wait: wait}, &r)
}

func (c *Client) Shutdown(ctx context.Context, p ShutdownParams, progress func(ShutdownProgress)) error {
	c.mu.Lock()
	c.progress = progress
	c.mu.Unlock()
	return c.conn.Call(ctx, MethodShutdown, p, nil)
}

func (c *Client) EngineStatus(ctx context.Context) (*EngineStatus, error) {
	var r EngineStatus
	return &r, c.conn.Call(ctx, MethodEngineStatus, nil, &r)
}

func (c *Client) EngineStart(ctx context.Context) (*EngineStatus, error) {
	var r EngineStatus
	return &r, c.conn.Call(ctx, MethodEngineStart, nil, &r)
}

func (c *Client) EngineStop(ctx context.Context, p EngineStopParams) (*EngineStatus, error) {
	var r EngineStatus
	return &r, c.conn.Call(ctx, MethodEngineStop, p, &r)
}

func (c *Client) EngineRestart(ctx context.Context, p EngineStopParams) (*EngineStatus, error) {
	var r EngineStatus
	return &r, c.conn.Call(ctx, MethodEngineRestart, p, &r)
}

func (c *Client) SubscribeEngine(ctx context.Context, deliver func(EngineStatus)) (func(), error) {
	var sub Subscription
	err := c.conn.CallThen(ctx, MethodEngineSubscribe, nil, &sub, func() {
		c.mu.Lock()
		c.engine[sub.Subscription] = deliver
		c.mu.Unlock()
	})
	if err != nil {
		return nil, err
	}
	return func() {
		c.mu.Lock()
		delete(c.engine, sub.Subscription)
		c.mu.Unlock()
		c.conn.Call(context.Background(), MethodEngineUnsubscribe, sub, nil)
	}, nil
}

func (c *Client) StorageInfo(ctx context.Context) (*StorageInfo, error) {
	var r StorageInfo
	return &r, c.conn.Call(ctx, MethodStorageInfo, nil, &r)
}

func (c *Client) StorageSync(ctx context.Context) (*Synced, error) {
	var r Synced
	return &r, c.conn.Call(ctx, MethodStorageSync, Empty{}, &r)
}

func (c *Client) StorageBudget(ctx context.Context, p BudgetParams) (*Budget, error) {
	var r Budget
	return &r, c.conn.Call(ctx, MethodStorageBudget, p, &r)
}

func (c *Client) StorageTrim(ctx context.Context, p TrimParams) (*TrimResult, error) {
	var r TrimResult
	return &r, c.conn.Call(ctx, MethodStorageTrim, p, &r)
}

func (c *Client) ExecStart(ctx context.Context, p ExecStartParams, exited func(ExecExit)) (*ExecStarted, error) {
	var r ExecStarted
	err := c.conn.CallThen(ctx, MethodExecStart, p, &r, func() {
		if exited != nil {
			c.mu.Lock()
			c.execs[r.ID] = exited
			c.mu.Unlock()
		}
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) ExecResize(ctx context.Context, id uint64, rows, cols int) error {
	return c.conn.Call(ctx, MethodExecResize, ExecResizeParams{ID: id, Rows: rows, Cols: cols}, nil)
}

func (c *Client) ExecSignal(ctx context.Context, id uint64, signal int) error {
	return c.conn.Call(ctx, MethodExecSignal, ExecSignalParams{ID: id, Signal: signal}, nil)
}

func (c *Client) ExecClose(ctx context.Context, id uint64) error {
	c.mu.Lock()
	delete(c.execs, id)
	c.mu.Unlock()
	return c.conn.Call(ctx, MethodExecClose, ExecID{ID: id}, nil)
}

func (c *Client) Fetch(ctx context.Context, p FetchParams) (*FetchResult, error) {
	var r FetchResult
	return &r, c.conn.Call(ctx, MethodFetch, p, &r)
}

func (c *Client) PingHost(ctx context.Context, p PingParams) (*PingResult, error) {
	var r PingResult
	return &r, c.conn.Call(ctx, MethodPingHost, p, &r)
}

func (c *Client) SubscribeLogs(ctx context.Context, p LogSubscribeParams, deliver func(LogEntry)) (func(), error) {
	return c.SubscribeLogsMarked(ctx, p, deliver, nil)
}

func (c *Client) SubscribeLogsMarked(ctx context.Context, p LogSubscribeParams, deliver func(LogEntry), backlogEnd func()) (func(), error) {
	var sub Subscription
	err := c.conn.CallThen(ctx, MethodLogSubscribe, p, &sub, func() {
		c.mu.Lock()
		c.logs[sub.Subscription] = deliver
		if backlogEnd != nil {
			c.logEnds[sub.Subscription] = backlogEnd
		}
		c.mu.Unlock()
	})
	if err != nil {
		return nil, err
	}
	return func() {
		c.mu.Lock()
		delete(c.logs, sub.Subscription)
		delete(c.logEnds, sub.Subscription)
		c.mu.Unlock()
		c.conn.Call(context.Background(), MethodLogUnsubscribe, sub, nil)
	}, nil
}

func (c *Client) startOperation(ctx context.Context, method string, params any, progress func(OperationProgress)) (*Operation, error) {
	var r Operation
	err := c.conn.CallThen(ctx, method, params, &r, func() {
		if progress != nil {
			c.mu.Lock()
			c.ops[r.ID] = progress
			c.mu.Unlock()
		}
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) OnMachineState(deliver func(MachineState)) {
	c.mu.Lock()
	c.machines = deliver
	c.mu.Unlock()
}

func (c *Client) MachineCreate(ctx context.Context, p MachineCreateParams, progress func(OperationProgress)) (*Operation, error) {
	return c.startOperation(ctx, MethodMachineCreate, p, progress)
}

func (c *Client) MachineStart(ctx context.Context, name string, config MachineConfig) (*MachineInfo, error) {
	var r MachineInfo
	return &r, c.conn.Call(ctx, MethodMachineStart, MachineStartParams{Name: name, Config: config}, &r)
}

func (c *Client) MachineStop(ctx context.Context, p MachineStopParams) (*MachineInfo, error) {
	var r MachineInfo
	return &r, c.conn.Call(ctx, MethodMachineStop, p, &r)
}

func (c *Client) MachineRestart(ctx context.Context, p MachineRestartParams) (*MachineInfo, error) {
	var r MachineInfo
	return &r, c.conn.Call(ctx, MethodMachineRestart, p, &r)
}

func (c *Client) MachineDelete(ctx context.Context, p MachineStopParams) error {
	return c.conn.Call(ctx, MethodMachineDelete, p, nil)
}

func (c *Client) MachineClone(ctx context.Context, p MachineCloneParams) (*MachineInfo, error) {
	var r MachineInfo
	return &r, c.conn.Call(ctx, MethodMachineClone, p, &r)
}

func (c *Client) MachineRename(ctx context.Context, name, newName string) (*MachineInfo, error) {
	var r MachineInfo
	return &r, c.conn.Call(ctx, MethodMachineRename, MachineRenameParams{Name: name, NewName: newName}, &r)
}

func (c *Client) MachineExport(ctx context.Context, name string, progress func(OperationProgress)) (*Operation, error) {
	return c.startOperation(ctx, MethodMachineExport, MachineNameParams{Name: name}, progress)
}

func (c *Client) MachineImport(ctx context.Context, p MachineImportParams, progress func(OperationProgress)) (*Operation, error) {
	return c.startOperation(ctx, MethodMachineImport, p, progress)
}

func (c *Client) Machine(ctx context.Context, name string) (*MachineInfo, error) {
	var r MachineInfo
	return &r, c.conn.Call(ctx, MethodMachineInfo, MachineNameParams{Name: name}, &r)
}

func (c *Client) MachineList(ctx context.Context) (*MachineList, error) {
	var r MachineList
	return &r, c.conn.Call(ctx, MethodMachineList, Empty{}, &r)
}

func (c *Client) OperationCancel(ctx context.Context, id uint64) error {
	return c.conn.Call(ctx, MethodOperationCancel, OperationID{ID: id}, nil)
}

func (c *Client) TrustSet(ctx context.Context, roots []string) (*Trusted, error) {
	if roots == nil {
		roots = []string{}
	}
	var r Trusted
	return &r, c.conn.Call(ctx, MethodTrustSet, TrustParams{Roots: roots}, &r)
}

func (c *Client) SSHAgentSet(ctx context.Context, path string) (*SSHAgentBound, error) {
	var r SSHAgentBound
	return &r, c.conn.Call(ctx, MethodSSHAgentSet, SSHAgentParams{Path: path}, &r)
}

func (c *Client) StorageCheck(ctx context.Context, progress func(OperationProgress)) (*Operation, error) {
	return c.startOperation(ctx, MethodStorageCheck, CheckParams{}, progress)
}

func (c *Client) StorageCheckFollow(ctx context.Context, progress func(OperationProgress)) (*Operation, error) {
	return c.startOperation(ctx, MethodStorageCheck, CheckParams{Follow: true}, progress)
}

func (c *Client) StorageSalvage(ctx context.Context, p SalvageParams, progress func(OperationProgress)) (*Operation, error) {
	return c.startOperation(ctx, MethodStorageSalvage, p, progress)
}

func (c *Client) StorageUsage(ctx context.Context, p UsageParams) (*StorageUsage, error) {
	var r StorageUsage
	return &r, c.conn.Call(ctx, MethodStorageUsage, p, &r)
}

func IsNoMachine(err error) bool {
	var e *jsonrpc.Error
	return errors.As(err, &e) && e.Code == CodeNoMachine
}

func IsBusy(err error) bool {
	var e *jsonrpc.Error
	return errors.As(err, &e) && e.Code == CodeBusy
}

func (c *Client) VolumeClone(ctx context.Context, name, newName string) (*VolumeInfo, error) {
	var r VolumeInfo
	return &r, c.conn.Call(ctx, MethodVolumeClone, VolumeCloneParams{Name: name, NewName: newName}, &r)
}

func (c *Client) VolumeExport(ctx context.Context, name string, progress func(OperationProgress)) (*Operation, error) {
	return c.startOperation(ctx, MethodVolumeExport, VolumeNameParams{Name: name}, progress)
}

func (c *Client) VolumeImport(ctx context.Context, name string, progress func(OperationProgress)) (*Operation, error) {
	return c.startOperation(ctx, MethodVolumeImport, VolumeNameParams{Name: name}, progress)
}
