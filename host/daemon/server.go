package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"fev/host/daemon/config"
	"fev/host/daemon/logs"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/version"
)

type client struct {
	name string

	mu      sync.Mutex
	subs    map[uint64]func()
	gone    bool
	closers []func()
}

func (cl *client) onGone(fn func()) {
	cl.mu.Lock()
	gone := cl.gone
	if !gone {
		cl.closers = append(cl.closers, fn)
	}
	cl.mu.Unlock()
	if gone {
		go fn()
	}
}

type clientKey struct{}

func rpcError(code int, data any, format string, args ...any) *jsonrpc.Error {
	e := jsonrpc.Errorf(code, format, args...)
	if data != nil {
		e.Data, _ = json.Marshal(data)
	}
	return e
}

func (d *Daemon) serve(conn net.Conn) {
	c := jsonrpc.NewConn(conn, jsonrpc.HandlerFunc(d.handle))
	d.mu.Lock()
	d.conns[c] = struct{}{}
	d.mu.Unlock()
	go func() {
		<-c.Done()
		d.mu.Lock()
		delete(d.conns, c)
		d.mu.Unlock()
		if v, ok := c.Get(clientKey{}); ok {
			cl := v.(*client)
			cl.mu.Lock()
			cl.gone = true
			for id, end := range cl.subs {
				end()
				delete(cl.subs, id)
			}
			closers := cl.closers
			cl.closers = nil
			cl.mu.Unlock()
			for _, fn := range closers {
				fn()
			}
		}
	}()
}

type caller struct {
	c  *jsonrpc.Conn
	cl *client
}

func (c caller) Name() string                           { return c.cl.name }
func (c caller) Notify(method string, params any) error { return c.c.Notify(method, params) }
func (c caller) OnGone(fn func())                       { c.cl.onGone(fn) }

func (cl *client) add(id uint64, end func()) {
	cl.mu.Lock()
	gone := cl.gone
	if !gone {
		cl.subs[id] = end
	}
	cl.mu.Unlock()
	if gone {
		end()
	}
}

func (cl *client) end(id uint64) {
	cl.mu.Lock()
	end := cl.subs[id]
	delete(cl.subs, id)
	cl.mu.Unlock()
	if end != nil {
		end()
	}
}

func (d *Daemon) handle(ctx context.Context, c *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
	if types, ok := api.Schema[method]; !ok || types.Result == nil {
		if types, ok := api.Pending[method]; !ok || types.Result == nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method %q not found", method)
		}
	}
	if d.shuttingDown.Load() {
		return nil, jsonrpc.Errorf(api.CodeShuttingDown, "the daemon is shutting down")
	}
	if method == api.MethodHello {
		p, err := jsonrpc.Bind[api.HelloParams](params)
		if err != nil {
			return nil, err
		}
		if p.APIVersion != api.APIVersion {
			jsonrpc.AfterReply(ctx, func() { c.Close() })
			return nil, jsonrpc.Errorf(api.CodeVersionMismatch, "the daemon speaks API version %d, the client %d", api.APIVersion, p.APIVersion)
		}
		if p.Build != version.Build {
			jsonrpc.AfterReply(ctx, func() { c.Close() })
			m := &version.Mismatch{Ours: version.Build, Theirs: p.Build, Peer: "client " + clip(p.Client, 60)}
			d.log.Warn("refused a client from another build", "client", clip(p.Client, 60), "client_build", clip(p.Build, 80))
			data := api.BuildMismatch{DaemonBuild: version.Build, ClientBuild: p.Build, PID: os.Getpid(), State: d.hub.Status().State}
			if data.State != api.StateStopped {
				for _, mod := range d.opts.Modules {
					if c, ok := mod.(ContainerCounter); ok {
						if n, known := c.RunningContainers(); known {
							data.RunningContainers = &n
						}
					}
				}
			}
			return nil, rpcError(api.CodeBuildMismatch, data, "%s", m.Error())
		}
		c.Set(clientKey{}, &client{name: clip(p.Client, 60), subs: map[uint64]func(){}})
		for _, mod := range d.opts.Modules {
			if g, ok := mod.(Greeter); ok {
				g.Hello(p)
			}
		}
		return api.HelloResult{APIVersion: api.APIVersion, Build: version.Build, PID: os.Getpid(), Home: d.layout.Home}, nil
	}
	v, ok := c.Get(clientKey{})
	if !ok {
		return nil, jsonrpc.Errorf(api.CodeHelloRequired, "call daemon.hello first")
	}
	cl := v.(*client)
	if _, ok := api.Pending[method]; ok && d.methods[method] == nil {
		return nil, jsonrpc.Errorf(api.CodeNotImplemented, "%s is not implemented in this build of the daemon", method)
	}

	d.sup.CatchUp()

	switch method {
	case api.MethodStatus:
		d.sup.CheckAlive()
		d.rouse(false)
		if d.hub.Status().State == api.StateRunning {
			for _, mod := range d.opts.Modules {
				if a, ok := mod.(StatusAsker); ok {
					a.StatusAsked(ctx)
				}
			}
		}
		return d.hub.Status(), nil

	case api.MethodShutdown:
		p, err := jsonrpc.Bind[api.ShutdownParams](params)
		if err != nil {
			return nil, err
		}
		d.log.Info("shutdown asked for", "client", cl.name, "force", p.Force, "stop_requested", p.StopRequested, "stop_began_ns", p.StopBeganNS)
		if p.StopBeganNS != 0 {
			switch d.beginIfStopStands(p.StopBeganNS, p.StopRequested) {
			case shutdownSuperseded:
				d.log.Info("the shutdown is not made: fev was started on the user's word since the stop began", "client", cl.name)
				return api.ShutdownResult{Superseded: true}, nil
			case shutdownBegun:
				if p.StopRequested {
					d.writeStopMark()
				}
				finish := func() { go d.finish(p.Force, p.StopRequested) }
				if !jsonrpc.AfterReply(ctx, finish) {
					finish()
				}
				return api.ShutdownResult{}, nil
			}
		}
		if p.StopRequested {
			d.writeStopMark()
		}
		jsonrpc.AfterReply(ctx, func() { go d.shutdown(p.Force, p.StopRequested) })
		return api.ShutdownResult{}, nil

	case api.MethodCaffeinateHold:
		return d.caffeinateHold(ctx, cl, params)

	case api.MethodVMStart:
		p, err := jsonrpc.Bind[api.StartParams](params)
		if err != nil {
			return nil, err
		}
		if err := d.startAsked(); err != nil {
			return nil, shutting(err)
		}
		d.rouse(true)
		return d.waitUp(ctx, p.NoWait)

	case api.MethodVMRestart:
		p, err := jsonrpc.Bind[api.StartParams](params)
		if err != nil {
			return nil, err
		}
		if err := d.restartAsked(); err != nil {
			return nil, shutting(err)
		}
		return d.waitUp(ctx, p.NoWait)

	case api.MethodVMStop:
		p, err := jsonrpc.Bind[api.StopParams](params)
		if err != nil {
			return nil, err
		}
		if p.TimeoutMS < 0 || p.TimeoutMS > MaxStopTimeoutMS {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "timeout_ms must be between 0 and %d", MaxStopTimeoutMS)
		}
		d.askMu.Lock()
		if p.StopBeganNS != 0 && d.startAskedSince(p.StopBeganNS) {
			d.askMu.Unlock()
			d.log.Info("vm.stop is not made: fev was started on the user's word since the stop began", "client", cl.name)
			return d.hub.Status(), nil
		}
		d.writeStopMark()
		boot := d.sup.Stop(p.Force, time.Duration(p.TimeoutMS)*time.Millisecond, true)
		d.askMu.Unlock()
		if p.NoWait {
			return d.hub.Status(), nil
		}
		return d.hub.Wait(ctx, func(st api.Status) bool {
			return st.State == api.StateStopped || (boot != "" && st.BootID != boot)
		})

	case api.MethodEventsSubscribe:
		id := d.nextSub.Add(1)
		sub, snapshot := d.hub.Subscribe(func() {
			d.log.Warn("a client stopped reading its events and was disconnected", "client", cl.name)
			c.Close()
		})
		cl.add(id, sub.Close)
		d.pumps.Add(1)
		jsonrpc.AfterReply(ctx, func() {
			go func() {
				defer d.pumps.Done()
				pumpEvents(c, id, sub)
			}()
		})
		return api.EventsSubscribed{Subscription: id, Status: snapshot}, nil

	case api.MethodStatsSubscribe:
		return d.statsSubscribe(ctx, c, cl, params)

	case api.MethodEventsUnsubscribe, api.MethodLogUnsubscribe, api.MethodStatsUnsubscribe:
		p, err := jsonrpc.Bind[api.Subscription](params)
		if err != nil {
			return nil, err
		}
		cl.end(p.Subscription)
		return api.Empty{}, nil

	case api.MethodLogSubscribe:
		p, err := jsonrpc.Bind[api.LogSubscribeParams](params)
		if err != nil {
			return nil, err
		}
		if p.Backlog < 0 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "backlog must not be negative")
		}
		if machineSources(p.Sources) {
			return d.subscribeMachineLogs(ctx, c, cl, p)
		}
		for _, source := range p.Sources {
			known := false
			for _, s := range logs.Sources {
				known = known || s == source
			}
			if !known {
				return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "no log source %q", clip(source, 40))
			}
		}
		if p.Backlog < 0 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "backlog must not be negative")
		}
		id := d.nextSub.Add(1)
		sub := d.logs.Subscribe(p.Sources, min(p.Backlog, logs.RingLines))
		cl.add(id, sub.Close)
		jsonrpc.AfterReply(ctx, func() { go pumpLogs(c, id, sub) })
		return api.Subscription{Subscription: id}, nil

	case api.MethodConfigDescribe:
		return api.ConfigKeys{Keys: d.cfg.Describe()}, nil
	case api.MethodConfigList:
		values, err := d.cfg.Get()
		return api.ConfigValues{Values: values}, configError(err)
	case api.MethodConfigGet:
		p, err := jsonrpc.Bind[api.ConfigGetParams](params)
		if err != nil {
			return nil, err
		}
		if len(p.Keys) == 0 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "config.get needs keys; config.list returns them all")
		}
		values, err := d.cfg.Get(p.Keys...)
		return api.ConfigValues{Values: values}, configError(err)
	case api.MethodConfigSet:
		p, err := jsonrpc.Bind[api.ConfigSetParams](params)
		if err != nil {
			return nil, err
		}
		return d.changeConfig(cl.name, func() ([]api.ConfigChange, error) { return d.cfg.Set(p.Values) })
	case api.MethodConfigReset:
		p, err := jsonrpc.Bind[api.ConfigResetParams](params)
		if err != nil {
			return nil, err
		}
		return d.changeConfig(cl.name, func() ([]api.ConfigChange, error) { return d.cfg.Reset(p.Keys...) })
	case api.MethodConfigAdd, api.MethodConfigRemove:
		p, err := jsonrpc.Bind[api.ConfigListParams](params)
		if err != nil {
			return nil, err
		}
		if len(p.Values) == 0 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%s needs values", method)
		}
		edit := d.cfg.Add
		if method == api.MethodConfigRemove {
			edit = d.cfg.Remove
		}
		return d.changeConfig(cl.name, func() ([]api.ConfigChange, error) { return edit(p.Key, p.Values) })

	case api.MethodBootTrace:
		if t, ok := d.sup.Trace(); ok {
			return t, nil
		}
		return nil, jsonrpc.Errorf(api.CodeFailed, "no boot has been recorded")
	}
	if fn := d.methods[method]; fn != nil {
		return fn(context.WithValue(ctx, callerKey{}, caller{c, cl}), params)
	}
	return nil, jsonrpc.Errorf(api.CodeFailed, "%s is not available: no module of this daemon provides it", method)
}

func shutting(err error) error {
	if errors.Is(err, ErrShuttingDown) {
		return jsonrpc.Errorf(api.CodeShuttingDown, "the daemon is shutting down")
	}
	return err
}

func configError(err error) error {
	var e *config.Error
	if !errors.As(err, &e) {
		return err
	}
	code := api.CodeInvalidValue
	if e.Unknown {
		code = api.CodeUnknownKey
	}
	return rpcError(code, api.ConfigError{Key: clip(e.Key, 80), Reason: e.Reason}, "%s", clip(e.Error(), 320))
}

func (d *Daemon) changeConfig(who string, change func() ([]api.ConfigChange, error)) (any, error) {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	changed, err := change()
	if err != nil {
		return nil, configError(err)
	}
	for _, ch := range changed {
		d.log.Info("configuration changed", "client", who, "key", ch.Key, "value", ch.Value, "pending", ch.Pending)
		d.hub.Publish(ch)
		if ch.Key == api.KeyStartAtLogin {
			d.applyLoginItem()
		}
	}
	d.sup.Refresh()
	return api.ConfigChanges{Changed: changed}, nil
}

func (d *Daemon) waitUp(ctx context.Context, noWait bool) (any, error) {
	if noWait {
		return d.hub.Status(), nil
	}
	st, err := d.hub.Wait(ctx, func(st api.Status) bool {
		switch st.State {
		case api.StateRunning, api.StatePaused, api.StateStopped:
			return true
		case api.StateStarting:
			return st.Engine == api.EngineFailed
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	switch {
	case st.State == api.StateRunning || st.State == api.StatePaused:
		return st, nil
	case st.Error != "":
		return nil, jsonrpc.Errorf(api.CodeFailed, "%s", st.Error)
	default:
		return nil, jsonrpc.Errorf(api.CodeFailed, "the VM was stopped before it was running")
	}
}

func pumpEvents(c *jsonrpc.Conn, id uint64, sub *EventSub) {
	for {
		event, ok := sub.Next(c.Done())
		if !ok {
			return
		}
		var err error
		switch event := event.(type) {
		case api.Status:
			err = c.Notify(api.NotifyStateChanged, api.StateChanged{Subscription: id, Status: event})
		case api.ConfigChange:
			err = c.Notify(api.NotifyConfigChanged, api.ConfigChanged{Subscription: id, ConfigChange: event})
		case api.ForwardsChanged:
			event.Subscription = id
			err = c.Notify(api.NotifyForwardsChanged, event)
		case api.DockerEvent:
			event.Subscription = id
			err = c.Notify(api.NotifyDockerEvent, event)
		case api.FilesChanged:
			event.Subscription = id
			err = c.Notify(api.NotifyFilesChanged, event)
		case api.PowerSleep:
			event.Subscription = id
			err = c.Notify(api.NotifyPowerSleep, event)
		case api.PowerWake:
			event.Subscription = id
			err = c.Notify(api.NotifyPowerWake, event)
		case api.MachinesChanged:
			event.Subscription = id
			err = c.Notify(api.NotifyMachinesChanged, event)
		case api.OperationProgress:
			event.Subscription = id
			err = c.Notify(api.NotifyOperationProgress, event)
		}
		if err != nil {
			return
		}
	}
}

func pumpLogs(c *jsonrpc.Conn, id uint64, sub *logs.Sub) {
	for {
		e, dropped, ok := sub.Next(c.Done())
		if !ok {
			return
		}
		if e.End {
			if c.Notify(api.NotifyLogBacklogEnd, api.Subscription{Subscription: id}) != nil {
				return
			}
			continue
		}
		err := c.Notify(api.NotifyLogEntry, api.LogEntry{
			Subscription: id, Source: e.Source, Time: stamp(e.Time), GuestMS: e.GuestMS,
			Level: e.Level, Text: e.Text, Dropped: dropped,
		})
		if err != nil {
			return
		}
	}
}
