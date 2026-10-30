package daemon

import (
	"context"
	"encoding/json"
	"time"

	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/debugstore"
	"fev/proto/jsonrpc"
)

func (m *Machines) debugCacheURL() string {
	if s := m.cfgString(api.KeyDebugCacheURL); s != "" && s != debugstore.CacheURL {
		return s
	}
	return ""
}

func (m *Machines) debugIndexURL() string { return m.cfgString(api.KeyDebugIndexURL) }

func (m *Machines) cfgString(key string) string {
	v, err := m.cfg.Get(key)
	if err != nil {
		return ""
	}
	s, _ := v[key].Value.(string)
	return s
}

func (t *sessionTable) startDebug(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.DebugStartParams](params)
	if err != nil {
		return nil, err
	}
	caller, ok := CallerOf(ctx)
	if !ok {
		return nil, jsonrpc.Errorf(api.CodeFailed, "debug.start needs a client connection")
	}
	if p.Target == "" {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "debug.start needs a target")
	}
	switch p.Kind {
	case "", agent.DebugRequestContainer, agent.DebugRequestImage:
	default:
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "debug.start: unknown kind %q", p.Kind)
	}
	if p.TTY && p.StdinTTY {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "tty and stdin_tty are not both set")
	}
	if p.Context != "" {
		return t.startRemoteDebug(ctx, caller, p)
	}
	if err := agent.CheckExecTerminfo(agent.ExecStartParams{Context: agent.ExecDebug, TTY: p.TTY, StdinTTY: p.StdinTTY, Env: p.Env, Terminfo: p.Terminfo, Debug: &agent.DebugTarget{Target: p.Target}}); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	m := t.m
	t.mu.Lock()
	resetting := t.resetting
	t.mu.Unlock()
	if resetting {
		return nil, jsonrpc.Errorf(api.CodeBusy, "the debug tool store is being reset; try again when it is done")
	}
	client, err := m.guest(ctx, true)
	if err != nil {
		return nil, err
	}
	vm, ok := m.host.VM()
	if !ok || client == nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "the VM stopped as the session was starting")
	}
	t.mu.Lock()
	t.nextID++
	s := &session{id: t.nextID, caller: caller, client: client, vm: vm, over: make(chan struct{}), debug: true}
	t.byID[s.id] = s
	t.mu.Unlock()
	started, err := client.ExecStart(ctx, agent.ExecStartParams{
		Context: agent.ExecDebug, Argv: p.Argv, Env: p.Env, Dir: p.Dir,
		TTY: p.TTY, StdinTTY: p.StdinTTY, Rows: p.Rows, Cols: p.Cols, Terminfo: p.Terminfo,
		Debug: &agent.DebugTarget{Target: p.Target, Kind: p.Kind, ReadOnly: p.ReadOnly,
			CacheURL: m.debugCacheURL(), IndexURL: m.debugIndexURL()},
	}, t.exited(s))
	if err != nil {
		t.forget(s)
		return nil, guestError(err)
	}
	if started.Debug == nil {
		t.kill(s)
		return nil, jsonrpc.Errorf(api.CodeFailed, "the guest started a debug session but said nothing of the target")
	}
	t.arm(s, started.ID, started.Token)
	if m.bumpDebugSessions(vm.BootID()) {
		go m.refreshDebugLine()
	}
	g := started.Debug
	return api.DebugStarted{
		ID: s.id, Token: s.token,
		Kind: g.Kind, TargetID: g.TargetID, Name: g.Name, Image: g.Image,
		ReadOnly: g.ReadOnly, Notes: g.Notes,
	}, nil
}

func (m *Machines) debugSessionEnded() {
	if vm, ok := m.host.VM(); ok {
		m.bumpDebugSessions(vm.BootID())
	}
	m.host.WorkEnded()
	go m.refreshDebugLine()
}

func (m *Machines) refreshDebugLine() {
	client, err := m.guest(context.Background(), false)
	if err != nil || client == nil {
		return
	}
	vm, ok := m.host.VM()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g, err := client.DebugStatus(ctx)
	if err != nil {
		return
	}
	r := debugStatusFromGuest(g)
	r.Sessions = m.sessions.openDebug()
	m.showDebugLine(vm.BootID(), &r)
}

func (m *Machines) debugStatus(ctx context.Context, _ json.RawMessage) (any, error) {
	client, err := m.guest(ctx, false)
	if err == errVMDown {
		return nil, jsonrpc.Errorf(api.CodeFailed, "the VM is not running: what the debug store holds is the guest's to say")
	}
	if err != nil {
		return nil, err
	}
	g, err := client.DebugStatus(ctx)
	if err != nil {
		return nil, guestError(err)
	}
	r := debugStatusFromGuest(g)
	r.Sessions = m.sessions.openDebug()
	if vm, ok := m.host.VM(); ok {
		m.showDebugLine(vm.BootID(), &r)
	}
	return r, nil
}

func (m *Machines) debugReset(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.DebugResetParams](params)
	if err != nil {
		return nil, err
	}
	t := &m.sessions
	t.mu.Lock()
	var open []*session
	for _, s := range t.byID {
		if s.debug {
			open = append(open, s)
		}
	}
	if len(open) > 0 && !p.Force {
		t.mu.Unlock()
		return nil, jsonrpc.Errorf(api.CodeBusy, "a debug session is open (%d); close it, or reset with force", len(open))
	}
	if t.resetting {
		t.mu.Unlock()
		return nil, jsonrpc.Errorf(api.CodeBusy, "the debug tool store is already being reset")
	}
	t.resetting = true
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.resetting = false
		t.mu.Unlock()
	}()
	for _, s := range open {
		t.kill(s)
		s.exit(api.SessionExit{ExitCode: -1, Error: "the debug tool store was reset"})
	}
	client, err := m.guest(ctx, true)
	if err != nil {
		return nil, err
	}
	r, err := client.DebugReset(ctx, p.Force)
	if err != nil {
		return nil, guestError(err)
	}
	if vm, ok := m.host.VM(); ok {
		m.showDebugLine(vm.BootID(), &api.DebugStatus{Exists: false})
	}
	return api.DebugReset{FreedBytes: r.FreedBytes}, nil
}

func debugStatusFromGuest(g *agent.DebugStatus) api.DebugStatus {
	r := api.DebugStatus{
		Exists: g.Exists, Bytes: g.Bytes, Paths: g.Paths,
		DefaultSet: g.DefaultSet, DefaultWhole: g.DefaultWhole, IndexDate: g.IndexDate, Sessions: g.Sessions,
	}
	for _, pkg := range g.Packages {
		r.Packages = append(r.Packages, api.DebugPackage{
			Name: pkg.Name, Attr: pkg.Attr, StorePath: pkg.StorePath, Commands: pkg.Commands, Default: pkg.Default,
		})
	}
	return r
}

func (m *Machines) bumpDebugSessions(boot string) (unread bool) {
	n := m.sessions.openDebug()
	updateStatus(m.host, func(st *api.Status) {
		if st.BootID != boot {
			return
		}
		if st.Debug == nil {
			st.Debug = &api.DebugLine{Exists: true}
			unread = true
		}
		st.Debug.Sessions = n
	})
	return unread
}

func (m *Machines) showDebugLine(boot string, s *api.DebugStatus) {
	line := &api.DebugLine{Exists: s.Exists, Bytes: s.Bytes, Packages: len(s.Packages), Sessions: s.Sessions}
	updateStatus(m.host, func(st *api.Status) {
		if st.BootID == boot {
			st.Debug = line
		}
	})
}
