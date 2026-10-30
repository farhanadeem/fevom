package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"fev/proto/jsonrpc"
	"fev/proto/version"
)

type Client struct {
	conn *jsonrpc.Conn

	mu         sync.Mutex
	events     map[uint64]*eventSub
	logs       map[uint64]func(LogEntry)
	logEnds    map[uint64]func()
	stats      map[uint64]func(StatsSample)
	sessions   map[uint64]func(SessionExit)
	k8sWatches map[uint64]func(K8sChanged)
	k8sLogs    map[uint64]*k8sLogSub
}

type k8sLogSub struct {
	text func(string)
	end  func(error)
}

type eventSub struct {
	Events
	seq uint64
}

type Events struct {
	State     func(Status)
	Gap       func(missed uint64)
	Config    func(ConfigChange)
	Forward   func(f Forward, removed bool)
	Docker    func(DockerEvent)
	Files     func(FilesStatus)
	Sleep     func(PowerSleep)
	Wake      func(PowerWake)
	Machine   func(m Machine, removed bool)
	Operation func(Operation)
}

type ClientOption func(*HelloParams)

func WithSSHAuthSock(path string) ClientOption {
	return func(p *HelloParams) { p.SSHAuthSock = path }
}

func WithTerminfo(terminfo, dirs string) ClientOption {
	return func(p *HelloParams) { p.Terminfo, p.TerminfoDirs = terminfo, dirs }
}

func NewClient(ctx context.Context, rw io.ReadWriteCloser, name string, opts ...ClientOption) (*Client, *HelloResult, error) {
	c := &Client{
		events:  make(map[uint64]*eventSub),
		logs:    make(map[uint64]func(LogEntry)),
		logEnds: make(map[uint64]func()),
		stats:   make(map[uint64]func(StatsSample)),

		sessions:   make(map[uint64]func(SessionExit)),
		k8sWatches: make(map[uint64]func(K8sChanged)),
		k8sLogs:    make(map[uint64]*k8sLogSub),
	}
	c.conn = jsonrpc.NewConn(rw, jsonrpc.HandlerFunc(c.handle))
	var hello HelloResult
	hp := HelloParams{APIVersion: APIVersion, Build: version.Build, Client: name}
	for _, o := range opts {
		o(&hp)
	}
	err := c.conn.Call(ctx, MethodHello, hp, &hello)
	if err == nil {
		err = version.Check(version.Build, hello.Build, "daemon")
	} else if m := mismatch(err); m != nil {
		err = m
	}
	if err != nil {
		c.conn.Close()
		return nil, nil, fmt.Errorf("daemon hello: %w", err)
	}
	return c, &hello, nil
}

type MismatchError struct {
	*version.Mismatch
	Data BuildMismatch
}

func (e *MismatchError) Unwrap() error { return e.Mismatch }

func mismatch(err error) error {
	var e *jsonrpc.Error
	if !errors.As(err, &e) || e.Code != CodeBuildMismatch {
		return nil
	}
	var data BuildMismatch
	if json.Unmarshal(e.Data, &data) != nil || data.DaemonBuild == "" {
		return nil
	}
	return &MismatchError{Mismatch: &version.Mismatch{Ours: data.ClientBuild, Theirs: data.DaemonBuild, Peer: "daemon"}, Data: data}
}

func AsBuildMismatch(err error) (BuildMismatch, bool) {
	var m *MismatchError
	if errors.As(err, &m) {
		return m.Data, true
	}
	if m, ok := mismatch(err).(*MismatchError); ok {
		return m.Data, true
	}
	return BuildMismatch{}, false
}

func IsShuttingDown(err error) bool {
	var e *jsonrpc.Error
	return errors.As(err, &e) && e.Code == CodeShuttingDown
}

func (c *Client) handle(ctx context.Context, _ *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
	switch method {
	case NotifyStateChanged:
		n, err := jsonrpc.Bind[StateChanged](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		sub := c.events[n.Subscription]
		var missed uint64
		if sub != nil {
			if n.Status.Seq > sub.seq+1 {
				missed = n.Status.Seq - sub.seq - 1
			}
			sub.seq = n.Status.Seq
		}
		c.mu.Unlock()
		if sub == nil {
			break
		}
		if missed > 0 && sub.Gap != nil {
			sub.Gap(missed)
		}
		if sub.State != nil {
			sub.State(n.Status)
		}
	case NotifyConfigChanged:
		n, err := jsonrpc.Bind[ConfigChanged](params)
		if err != nil {
			return nil, err
		}
		if sub := c.sub(n.Subscription); sub != nil && sub.Config != nil {
			sub.Config(n.ConfigChange)
		}
	case NotifyForwardsChanged:
		n, err := jsonrpc.Bind[ForwardsChanged](params)
		if err != nil {
			return nil, err
		}
		if sub := c.sub(n.Subscription); sub != nil && sub.Forward != nil {
			sub.Forward(n.Forward, n.Removed)
		}
	case NotifyDockerEvent:
		n, err := jsonrpc.Bind[DockerEvent](params)
		if err != nil {
			return nil, err
		}
		if sub := c.sub(n.Subscription); sub != nil && sub.Docker != nil {
			sub.Docker(n)
		}
	case NotifyFilesChanged:
		n, err := jsonrpc.Bind[FilesChanged](params)
		if err != nil {
			return nil, err
		}
		if sub := c.sub(n.Subscription); sub != nil && sub.Files != nil {
			sub.Files(n.FilesStatus)
		}
	case NotifyPowerSleep:
		n, err := jsonrpc.Bind[PowerSleep](params)
		if err != nil {
			return nil, err
		}
		if sub := c.sub(n.Subscription); sub != nil && sub.Sleep != nil {
			sub.Sleep(n)
		}
	case NotifyPowerWake:
		n, err := jsonrpc.Bind[PowerWake](params)
		if err != nil {
			return nil, err
		}
		if sub := c.sub(n.Subscription); sub != nil && sub.Wake != nil {
			sub.Wake(n)
		}
	case NotifyMachinesChanged:
		n, err := jsonrpc.Bind[MachinesChanged](params)
		if err != nil {
			return nil, err
		}
		if sub := c.sub(n.Subscription); sub != nil && sub.Machine != nil {
			sub.Machine(n.Machine, n.Removed)
		}
	case NotifyOperationProgress:
		n, err := jsonrpc.Bind[OperationProgress](params)
		if err != nil {
			return nil, err
		}
		if sub := c.sub(n.Subscription); sub != nil && sub.Operation != nil {
			sub.Operation(n.Operation)
		}
	case NotifySessionExit:
		exit, err := jsonrpc.Bind[SessionExit](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		deliver := c.sessions[exit.ID]
		delete(c.sessions, exit.ID)
		c.mu.Unlock()
		if deliver != nil {
			deliver(exit)
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
	case NotifyK8sChanged, NotifyK8sLog, NotifyK8sLogEnd:
		return nil, c.handleK8s(method, params)
	default:
		return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method %q not found", method)
	}
	return nil, nil
}

func (c *Client) sub(id uint64) *eventSub {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.events[id]
}

func (c *Client) Close() error          { return c.conn.Close() }
func (c *Client) Done() <-chan struct{} { return c.conn.Done() }

func (c *Client) Drained() <-chan struct{} { return c.conn.Drained() }

func (c *Client) Status(ctx context.Context) (*Status, error) {
	var r Status
	return &r, c.conn.Call(ctx, MethodStatus, nil, &r)
}

func (c *Client) Shutdown(ctx context.Context, p ShutdownParams) (*ShutdownResult, error) {
	var r ShutdownResult
	return &r, c.conn.Call(ctx, MethodShutdown, p, &r)
}

func (c *Client) VMStart(ctx context.Context, p StartParams) (*Status, error) {
	var r Status
	return &r, c.conn.Call(ctx, MethodVMStart, p, &r)
}

func (c *Client) VMStop(ctx context.Context, p StopParams) (*Status, error) {
	var r Status
	return &r, c.conn.Call(ctx, MethodVMStop, p, &r)
}

func (c *Client) VMRestart(ctx context.Context, p StartParams) (*Status, error) {
	var r Status
	return &r, c.conn.Call(ctx, MethodVMRestart, p, &r)
}

func (c *Client) Subscribe(ctx context.Context, ev Events) (*Status, func(), error) {
	var r EventsSubscribed
	err := c.conn.CallThen(ctx, MethodEventsSubscribe, nil, &r, func() {
		c.mu.Lock()
		c.events[r.Subscription] = &eventSub{Events: ev, seq: r.Status.Seq}
		c.mu.Unlock()
	})
	if err != nil {
		return nil, nil, err
	}
	sub := Subscription{Subscription: r.Subscription}
	return &r.Status, func() {
		c.mu.Lock()
		delete(c.events, sub.Subscription)
		c.mu.Unlock()
		c.conn.Call(context.Background(), MethodEventsUnsubscribe, sub, nil)
	}, nil
}

func (c *Client) ConfigDescribe(ctx context.Context) ([]ConfigKey, error) {
	var r ConfigKeys
	return r.Keys, c.conn.Call(ctx, MethodConfigDescribe, nil, &r)
}

func (c *Client) ConfigList(ctx context.Context) (map[string]ConfigValue, error) {
	var r ConfigValues
	return r.Values, c.conn.Call(ctx, MethodConfigList, nil, &r)
}

func (c *Client) ConfigGet(ctx context.Context, keys ...string) (map[string]ConfigValue, error) {
	var r ConfigValues
	return r.Values, c.conn.Call(ctx, MethodConfigGet, ConfigGetParams{Keys: keys}, &r)
}

func (c *Client) ConfigSet(ctx context.Context, values map[string]any) ([]ConfigChange, error) {
	var r ConfigChanges
	return r.Changed, c.conn.Call(ctx, MethodConfigSet, ConfigSetParams{Values: values}, &r)
}

func (c *Client) ConfigReset(ctx context.Context, keys ...string) ([]ConfigChange, error) {
	var r ConfigChanges
	return r.Changed, c.conn.Call(ctx, MethodConfigReset, ConfigResetParams{Keys: keys}, &r)
}

func ConfigErrorOf(err error) *ConfigError {
	var e *jsonrpc.Error
	if !errors.As(err, &e) || (e.Code != CodeUnknownKey && e.Code != CodeInvalidValue) {
		return nil
	}
	var data ConfigError
	if json.Unmarshal(e.Data, &data) != nil {
		return nil
	}
	return &data
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

func (c *Client) BootTrace(ctx context.Context) (*Trace, error) {
	var r Trace
	return &r, c.conn.Call(ctx, MethodBootTrace, nil, &r)
}

func (c *Client) FilesStatus(ctx context.Context) (*FilesStatus, error) {
	var r FilesStatus
	return &r, c.conn.Call(ctx, MethodFilesStatus, nil, &r)
}

func (c *Client) DockerStatus(ctx context.Context) (*DockerStatus, error) {
	var r DockerStatus
	return &r, c.conn.Call(ctx, MethodDockerStatus, nil, &r)
}

func (c *Client) DockerRestart(ctx context.Context) (*DockerStatus, error) {
	var r DockerStatus
	return &r, c.conn.Call(ctx, MethodDockerRestart, nil, &r)
}

func (c *Client) DataReset(ctx context.Context) (*DataResetResult, error) {
	var r DataResetResult
	return &r, c.conn.Call(ctx, MethodDataReset, nil, &r)
}

func (c *Client) Forwards(ctx context.Context) ([]Forward, error) {
	var r Forwards
	return r.Forwards, c.conn.Call(ctx, MethodForwardsList, nil, &r)
}

func IsNotImplemented(err error) bool {
	var e *jsonrpc.Error
	return errors.As(err, &e) && e.Code == CodeNotImplemented
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

func (c *Client) CaffeinateHold(ctx context.Context, p CaffeinateParams) (*Caffeinated, error) {
	var r Caffeinated
	return &r, c.conn.Call(ctx, MethodCaffeinateHold, p, &r)
}

func (c *Client) ForwardsRetry(ctx context.Context, id string) ([]Forward, error) {
	var r Forwards
	return r.Forwards, c.conn.Call(ctx, MethodForwardsRetry, ForwardsRetryParams{ID: id}, &r)
}

func (c *Client) Machines(ctx context.Context) (*Machines, error) {
	var r Machines
	return &r, c.conn.Call(ctx, MethodMachinesList, nil, &r)
}

func (c *Client) Machine(ctx context.Context, name string) (*Machine, error) {
	var r Machine
	return &r, c.conn.Call(ctx, MethodMachinesInfo, MachineName{Name: name}, &r)
}

func (c *Client) CreateMachine(ctx context.Context, p MachinesCreateParams) (*Operation, error) {
	var r Operation
	return &r, c.conn.Call(ctx, MethodMachinesCreate, p, &r)
}

func (c *Client) StartMachine(ctx context.Context, name string) (*Machine, error) {
	var r Machine
	return &r, c.conn.Call(ctx, MethodMachinesStart, MachineName{Name: name}, &r)
}

func (c *Client) StopMachine(ctx context.Context, p MachinesStopParams) (*Machine, error) {
	var r Machine
	return &r, c.conn.Call(ctx, MethodMachinesStop, p, &r)
}

func (c *Client) RestartMachine(ctx context.Context, p MachinesStopParams) (*Machine, error) {
	var r Machine
	return &r, c.conn.Call(ctx, MethodMachinesRestart, p, &r)
}

func (c *Client) DeleteMachine(ctx context.Context, p MachinesStopParams) error {
	return c.conn.Call(ctx, MethodMachinesDelete, p, nil)
}

func (c *Client) CloneMachine(ctx context.Context, name, newName string) (*Machine, error) {
	var r Machine
	return &r, c.conn.Call(ctx, MethodMachinesClone, MachinesRenameParams{Name: name, NewName: newName}, &r)
}

func (c *Client) RenameMachine(ctx context.Context, name, newName string) (*Machine, error) {
	var r Machine
	return &r, c.conn.Call(ctx, MethodMachinesRename, MachinesRenameParams{Name: name, NewName: newName}, &r)
}

func (c *Client) ExportMachine(ctx context.Context, p MachinesExportParams) (*Operation, error) {
	var r Operation
	return &r, c.conn.Call(ctx, MethodMachinesExport, p, &r)
}

func (c *Client) ImportMachine(ctx context.Context, p MachinesImportParams) (*Operation, error) {
	var r Operation
	return &r, c.conn.Call(ctx, MethodMachinesImport, p, &r)
}

func (c *Client) SetDefaultMachine(ctx context.Context, name string) (*Machines, error) {
	var r Machines
	return &r, c.conn.Call(ctx, MethodMachinesSetDefault, MachineName{Name: name}, &r)
}

func (c *Client) SessionStart(ctx context.Context, p SessionStartParams, exited func(SessionExit)) (*SessionStarted, error) {
	if p.Argv == nil {
		p.Argv = []string{}
	}
	var r SessionStarted
	err := c.conn.CallThen(ctx, MethodSessionStart, p, &r, func() {
		if exited != nil {
			c.mu.Lock()
			c.sessions[r.ID] = exited
			c.mu.Unlock()
		}
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) SessionResize(ctx context.Context, id uint64, rows, cols int) error {
	return c.conn.Call(ctx, MethodSessionResize, SessionResizeParams{ID: id, Rows: rows, Cols: cols}, nil)
}

func (c *Client) SessionSignal(ctx context.Context, id uint64, signal int) error {
	return c.conn.Call(ctx, MethodSessionSignal, SessionSignalParams{ID: id, Signal: signal}, nil)
}

func (c *Client) SessionClose(ctx context.Context, id uint64) error {
	c.mu.Lock()
	delete(c.sessions, id)
	c.mu.Unlock()
	return c.conn.Call(ctx, MethodSessionClose, SessionID{ID: id}, nil)
}

func (c *Client) SSHInfo(ctx context.Context) (*SSHInfo, error) {
	var r SSHInfo
	return &r, c.conn.Call(ctx, MethodSSHInfo, nil, &r)
}

func (c *Client) OperationGet(ctx context.Context, id string) (*Operation, error) {
	var r Operation
	return &r, c.conn.Call(ctx, MethodOperationGet, OperationID{ID: id}, &r)
}

func (c *Client) OperationCancel(ctx context.Context, id string) (*Operation, error) {
	var r Operation
	return &r, c.conn.Call(ctx, MethodOperationCancel, OperationID{ID: id}, &r)
}

func (c *Client) StorageUsage(ctx context.Context, p UsageParams) (*StorageUsage, error) {
	var r StorageUsage
	return &r, c.conn.Call(ctx, MethodStorageUsage, p, &r)
}

func (c *Client) StorageSalvage(ctx context.Context, p SalvageParams) (*Operation, error) {
	var r Operation
	return &r, c.conn.Call(ctx, MethodStorageSalvage, p, &r)
}

func IsNoMachine(err error) bool {
	var e *jsonrpc.Error
	return errors.As(err, &e) && e.Code == CodeNoMachine
}

func (c *Client) DebugStart(ctx context.Context, p DebugStartParams, exited func(SessionExit)) (*DebugStarted, error) {
	var r DebugStarted
	err := c.conn.CallThen(ctx, MethodDebugStart, p, &r, func() {
		if exited != nil {
			c.mu.Lock()
			c.sessions[r.ID] = exited
			c.mu.Unlock()
		}
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) DebugStatus(ctx context.Context) (*DebugStatus, error) {
	var r DebugStatus
	return &r, c.conn.Call(ctx, MethodDebugStatus, Empty{}, &r)
}

func (c *Client) Contexts(ctx context.Context) (*RemoteContexts, error) {
	var r RemoteContexts
	return &r, c.conn.Call(ctx, MethodContextsList, Empty{}, &r)
}

func (c *Client) ContextContainers(ctx context.Context, context string) (*RemoteContainers, error) {
	var r RemoteContainers
	return &r, c.conn.Call(ctx, MethodContextContainers, ContextRef{Context: context}, &r)
}

func (c *Client) ContextImages(ctx context.Context, context string) (*RemoteImages, error) {
	var r RemoteImages
	return &r, c.conn.Call(ctx, MethodContextImages, ContextRef{Context: context}, &r)
}

func (c *Client) DebugReset(ctx context.Context, force bool) (*DebugReset, error) {
	var r DebugReset
	return &r, c.conn.Call(ctx, MethodDebugReset, DebugResetParams{Force: force}, &r)
}
