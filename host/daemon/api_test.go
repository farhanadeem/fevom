package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"fev/host/daemon/boottrace"
	"fev/host/daemon/config"
	"fev/host/daemon/launch"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/version"
)

type stub struct {
	mu      sync.Mutex
	host    Host
	states  []api.Status
	configs []api.ConfigChange
	marks   []string
	closed  bool
	fail    error
	onState func(api.Status)
}

func (m *stub) Name() string    { return "stub" }
func (m *stub) Marks() []string { return m.marks }
func (m *stub) Start(h Host) error {
	if m.fail != nil {
		return m.fail
	}
	m.host = h
	fixtures, _ := api.Fixtures()
	for _, fx := range fixtures {
		switch fx.Method {
		case api.MethodDockerStatus, api.MethodForwardsList, api.MethodForwardsRetry, api.MethodDockerRestart, api.MethodDataReset, api.MethodFilesStatus,
			api.MethodMachinesList, api.MethodMachinesInfo, api.MethodMachinesCreate, api.MethodMachinesStart,
			api.MethodMachinesStop, api.MethodMachinesRestart, api.MethodMachinesDelete, api.MethodMachinesClone,
			api.MethodMachinesRename, api.MethodMachinesExport, api.MethodMachinesImport, api.MethodMachinesSetDefault,
			api.MethodSessionStart, api.MethodSessionResize, api.MethodSessionSignal, api.MethodSessionClose,
			api.MethodOperationGet, api.MethodOperationCancel,
			api.MethodVolumesClone, api.MethodVolumesExport, api.MethodVolumesImport,
			api.MethodStorageUsage, api.MethodStorageSalvage, api.MethodStorageCheck, api.MethodMemoryReclaim,
			api.MethodDebugStart, api.MethodDebugStatus, api.MethodDebugReset,
			api.MethodContextsList, api.MethodContextContainers, api.MethodContextImages,
			api.MethodSSHInfo,
			api.MethodHTTPSTrust,
			api.MethodK8sStatus, api.MethodK8sEnable, api.MethodK8sDisable, api.MethodK8sReset,
			api.MethodK8sWatch, api.MethodK8sUnwatch, api.MethodK8sGet, api.MethodK8sApply, api.MethodK8sUpdate,
			api.MethodK8sScale, api.MethodK8sRestart, api.MethodK8sDelete, api.MethodK8sLogs, api.MethodK8sExec:
			result := fx.Result
			h.Handle(fx.Method, func(context.Context, json.RawMessage) (any, error) { return result, nil })
		}
	}
	return nil
}
func (m *stub) StateChanged(st api.Status) {
	m.mu.Lock()
	m.states = append(m.states, st)
	fn := m.onState
	m.mu.Unlock()
	if fn != nil {
		fn(st)
	}
}
func (m *stub) ConfigChanged(ch api.ConfigChange) {
	m.mu.Lock()
	m.configs = append(m.configs, ch)
	m.mu.Unlock()
}
func (m *stub) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
}

func TestTheDaemonAnswersEveryFixtureWithASchemaResult(t *testing.T) {
	fixtures, err := api.Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	f.opts.Modules = []Module{&stub{}}
	f.start()
	c := f.raw()
	if err := c.Call(bg, api.MethodVMStart, nil, nil); err != nil {
		t.Fatal(err)
	}
	answered := map[string]bool{}
	for _, fx := range fixtures {
		if fx.Notification || fx.Method == api.MethodShutdown {
			continue
		}
		answered[fx.Method] = true
		t.Run(fx.Method, func(t *testing.T) {
			params := fx.Params
			switch fx.Method {
			case api.MethodHello:
				var hello api.HelloParams
				json.Unmarshal(fx.Params, &hello)
				hello.Build = version.Build
				params, _ = json.Marshal(hello)
			case api.MethodEventsUnsubscribe, api.MethodLogUnsubscribe, api.MethodStatsUnsubscribe:
				var sub api.Subscription
				if err := c.Call(bg, strings.Replace(fx.Method, "unsubscribe", "subscribe", 1), nil, &sub); err != nil {
					t.Fatal(err)
				}
				params, _ = json.Marshal(sub)
			case api.MethodConfigRemove:
				var p api.ConfigListParams
				json.Unmarshal(fx.Params, &p)
				if rest, ok := strings.CutPrefix(p.Key, config.MachinePrefix); ok {
					name, short, _ := strings.Cut(rest, ".")
					f.d.cfg.RemoveScope(config.MachinePrefix, name)
					if err := f.d.cfg.AddScope(config.MachinePrefix, name, config.MachineKeys("ada"), map[string]any{short: append([]string{"/Users/ada/src/project"}, p.Values...)}); err != nil {
						t.Fatal(err)
					}
					f.d.cfg.MachineStarted(name, f.d.cfg.MachineValues(name))
				}
			}
			var result json.RawMessage
			if err := c.Call(bg, fx.Method, params, &result); err != nil {
				t.Fatal(err)
			}
			if err := api.Conforms(result, api.Schema[fx.Method].Result); err != nil {
				t.Fatalf("result %s does not fit the schema: %v", result, err)
			}
			switch fx.Method {
			case api.MethodVMStop, api.MethodVMStart, api.MethodVMRestart, api.MethodStatus:
				if !strings.Contains(string(result), `"pending_restart":[`) {
					t.Errorf("status without a pending_restart list: %s", result)
				}
			case api.MethodConfigDescribe:
				var got, want struct {
					Keys []any `json:"keys"`
				}
				json.Unmarshal(result, &got)
				json.Unmarshal(fx.Result, &want)
				if len(got.Keys) != 33 || !reflect.DeepEqual(got.Keys, want.Keys) {
					t.Errorf("config.describe returned %s, the fixture says %s", result, fx.Result)
				}
			}
		})
	}
	first := true
	for _, fx := range fixtures {
		if fx.Method != api.MethodShutdown {
			continue
		}
		answered[fx.Method] = true
		d := f
		if !first {
			d = newFixture(t)
			d.opts.Modules = []Module{&stub{}}
			d.start()
		}
		first = false
		superseded := false
		if r, _ := jsonrpc.Bind[api.ShutdownResult](fx.Result); r.Superseded {
			superseded = true
			if _, err := d.client().VMStart(bg, api.StartParams{NoWait: true}); err != nil {
				t.Fatal(err)
			}
		}
		var result json.RawMessage
		if err := d.raw().Call(bg, fx.Method, fx.Params, &result); err != nil || api.Conforms(result, api.ShutdownResult{}) != nil {
			t.Errorf("daemon.shutdown %s: %s %v", fx.Params, result, err)
		}
		if superseded {
			if got, _ := jsonrpc.Bind[api.ShutdownResult](result); !got.Superseded {
				t.Errorf("daemon.shutdown %s after a start: %s", fx.Params, result)
			}
			if _, err := d.client().Status(bg); err != nil {
				t.Errorf("the daemon after a superseded shutdown: %v", err)
			}
			d.d.Shutdown(false)
		}
		<-d.d.Done()
	}
	for method, types := range api.Schema {
		if types.Result != nil && !answered[method] {
			t.Errorf("%s is in the schema and the daemon was not held to a fixture for it", method)
		}
	}
}

func TestEveryNotificationFitsTheSchema(t *testing.T) {
	m := &stub{}
	f := newFixture(t)
	f.opts.Modules = []Module{m}
	f.start()
	type note struct {
		method string
		params json.RawMessage
	}
	notes := make(chan note, 256)
	c := jsonrpc.NewConn(f.conn(), jsonrpc.HandlerFunc(func(_ context.Context, _ *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
		notes <- note{method, append(json.RawMessage(nil), params...)}
		return nil, nil
	}))
	c.Call(bg, api.MethodHello, api.HelloParams{APIVersion: api.APIVersion, Build: version.Build, Client: "raw"}, nil)
	var events, logs api.Subscription
	if err := c.Call(bg, api.MethodEventsSubscribe, nil, &events); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(bg, api.MethodLogSubscribe, api.LogSubscribeParams{Backlog: 10}, &logs); err != nil {
		t.Fatal(err)
	}
	if events.Subscription == logs.Subscription {
		t.Error("two subscriptions share an id")
	}
	if err := c.Call(bg, api.MethodVMStart, nil, nil); err != nil {
		t.Fatal(err)
	}
	c.Call(bg, api.MethodConfigSet, api.ConfigSetParams{Values: map[string]any{api.KeyCPU: 2}}, nil)
	fixtures, _ := api.Fixtures()
	for _, fx := range fixtures {
		if fx.Method == api.NotifyFilesChanged {
			change, _ := jsonrpc.Bind[api.FilesChanged](fx.Params)
			m.host.PublishFiles(change.FilesStatus)
		}
	}
	for _, fx := range fixtures {
		if fx.Method == api.NotifyForwardsChanged {
			change, _ := jsonrpc.Bind[api.ForwardsChanged](fx.Params)
			m.host.PublishForward(change)
		}
	}
	seen := map[string]int{}
	deadline := time.After(10 * time.Second)
	for seen[api.NotifyForwardsChanged] == 0 {
		select {
		case n := <-notes:
			seen[n.method]++
			types, ok := api.Schema[n.method]
			if !ok || types.Result != nil {
				t.Fatalf("the daemon sent %s, which is not a notification of the schema", n.method)
			}
			if err := api.Conforms(n.params, types.Params); err != nil {
				t.Errorf("%s %s does not fit the schema: %v", n.method, n.params, err)
			}
			var sub api.Subscription
			json.Unmarshal(n.params, &sub)
			want := events.Subscription
			if n.method == api.NotifyLogEntry || n.method == api.NotifyLogBacklogEnd {
				want = logs.Subscription
			}
			if sub.Subscription != want {
				t.Errorf("%s carries subscription %d, want %d", n.method, sub.Subscription, want)
			}
		case <-deadline:
			t.Fatalf("notifications seen: %v", seen)
		}
	}
	for _, method := range []string{api.NotifyStateChanged, api.NotifyConfigChanged, api.NotifyLogEntry, api.NotifyForwardsChanged, api.NotifyFilesChanged} {
		if seen[method] == 0 {
			t.Errorf("no %s notification was sent", method)
		}
	}
}

func TestHelloComesFirstAndTheAPIVersionMustMatch(t *testing.T) {
	f := started(t)
	c := jsonrpc.NewConn(f.conn(), nil)
	for method, types := range api.Schema {
		if types.Result == nil || method == api.MethodHello {
			continue
		}
		if err := c.Call(bg, method, nil, nil); code(err) != api.CodeHelloRequired {
			t.Errorf("%s before hello: %v", method, err)
		}
	}
	if err := c.Call(bg, "no.such.method", nil, nil); code(err) != jsonrpc.CodeMethodNotFound {
		t.Errorf("a method that does not exist: %v", err)
	}
	if err := c.Call(bg, api.NotifyStateChanged, nil, nil); code(err) != jsonrpc.CodeMethodNotFound {
		t.Errorf("calling a notification: %v", err)
	}
	var hello api.HelloResult
	if err := c.Call(bg, api.MethodHello, api.HelloParams{APIVersion: api.APIVersion, Build: version.Build, Client: "raw"}, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.APIVersion != api.APIVersion || hello.Build != version.Build || hello.PID != os.Getpid() || hello.Home != f.layout.Home {
		t.Errorf("hello: %+v", hello)
	}
	if err := c.Call(bg, api.MethodStatus, nil, nil); err != nil {
		t.Errorf("a call after hello: %v", err)
	}
	if err := c.Call(bg, api.MethodVMStop, json.RawMessage(`{"timeout_ms":"soon"}`), nil); code(err) != jsonrpc.CodeInvalidParams {
		t.Errorf("malformed parameters: %v", err)
	}
	if err := c.Call(bg, api.MethodLogSubscribe, api.LogSubscribeParams{Sources: []string{"nonsense"}}, nil); code(err) != jsonrpc.CodeInvalidParams {
		t.Errorf("an unknown log source: %v", err)
	}

	other := jsonrpc.NewConn(f.conn(), nil)
	err := other.Call(bg, api.MethodHello, api.HelloParams{APIVersion: api.APIVersion + 1, Client: "future"}, nil)
	if code(err) != api.CodeVersionMismatch {
		t.Errorf("hello from another API version: %v", err)
	}
	select {
	case <-other.Done():
	case <-time.After(5 * time.Second):
		t.Error("the connection was left open after a version mismatch")
	}
	if info, _ := os.Stat(f.layout.Socket()); info.Mode().Perm() != 0o600 {
		t.Errorf("the socket's mode is %v", info.Mode().Perm())
	}
}

func TestAClientThatSendsNoBuildIsRefused(t *testing.T) {
	f := started(t)
	c := jsonrpc.NewConn(f.conn(), nil)
	err := c.Call(bg, api.MethodHello, api.HelloParams{APIVersion: api.APIVersion, Client: "hand-written"}, nil)
	var e *jsonrpc.Error
	if !errors.As(err, &e) || e.Code != api.CodeBuildMismatch {
		t.Fatalf("hello with no build: %v", err)
	}
	var data api.BuildMismatch
	json.Unmarshal(e.Data, &data)
	if data.DaemonBuild != version.Build || data.ClientBuild != "" {
		t.Errorf("error data: %+v", data)
	}
	if !strings.Contains(e.Message, "sent no build identifier") {
		t.Errorf("the message does not say why: %q", e.Message)
	}
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Error("the connection was left open after the refusal")
	}
	if _, err := f.client().Status(bg); err != nil {
		t.Errorf("the typed client, which sends its build: %v", err)
	}
}

func TestAClientFromAnotherBuildIsRefusedAtEveryConnection(t *testing.T) {
	f := started(t)
	for attempt := 0; attempt < 2; attempt++ {
		c := jsonrpc.NewConn(f.conn(), nil)
		err := c.Call(bg, api.MethodHello, api.HelloParams{APIVersion: api.APIVersion, Build: "0ld8u1d-dirty.deadbeef", Client: "stale"}, nil)
		var e *jsonrpc.Error
		if !errors.As(err, &e) || e.Code != api.CodeBuildMismatch {
			t.Fatalf("hello from another build: %v", err)
		}
		var data api.BuildMismatch
		json.Unmarshal(e.Data, &data)
		if data.DaemonBuild != version.Build || data.ClientBuild != "0ld8u1d-dirty.deadbeef" {
			t.Errorf("error data: %+v", data)
		}
		for _, want := range []string{`"` + version.Build + `"`, `"0ld8u1d-dirty.deadbeef"`, "client stale"} {
			if !strings.Contains(e.Message, want) {
				t.Errorf("the message %q lacks %s", e.Message, want)
			}
		}
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Error("the connection was left open after a build mismatch")
		}
	}
	if st, err := f.client().Status(bg); err != nil || st.State != api.StateStopped {
		t.Errorf("status afterwards: %+v %v", st, err)
	}
}

func TestConfigurationThroughTheAPI(t *testing.T) {
	m := &stub{}
	f := newFixture(t)
	f.opts.Modules = []Module{m}
	f.start()
	c := f.client()
	watcher := f.client()
	h := f.watch(watcher)

	values, err := c.ConfigList(bg)
	if err != nil || len(values) != 33 || !values[api.KeyCPU].IsDefault || values[api.KeyMemoryMiB].Value != float64(8192) {
		t.Fatalf("config.list: %+v %v", values, err)
	}
	_, err = c.ConfigSet(bg, map[string]any{api.KeyCPU: 64})
	if e := api.ConfigErrorOf(err); e == nil || e.Key != api.KeyCPU || e.Reason != "at most 10 on this Mac" || code(err) != api.CodeInvalidValue {
		t.Errorf("an invalid value: %v (%+v)", err, e)
	}
	_, err = c.ConfigSet(bg, map[string]any{api.KeyDataAllowBackup: true, "rosetta": true})
	if e := api.ConfigErrorOf(err); e == nil || e.Key != "rosetta" || code(err) != api.CodeUnknownKey {
		t.Errorf("an unknown key: %v (%+v)", err, e)
	}
	if _, err = c.ConfigGet(bg, "rosetta"); code(err) != api.CodeUnknownKey {
		t.Errorf("get of an unknown key: %v", err)
	}
	if _, err = c.ConfigReset(bg, "rosetta"); code(err) != api.CodeUnknownKey {
		t.Errorf("reset of an unknown key: %v", err)
	}
	if _, err = c.ConfigGet(bg); code(err) != jsonrpc.CodeInvalidParams {
		t.Errorf("config.get with no keys: %v", err)
	}

	changed, err := c.ConfigSet(bg, map[string]any{api.KeyMemoryMiB: 4096, api.KeyDataAllowBackup: true})
	want := []api.ConfigChange{
		{Key: api.KeyDataAllowBackup, Value: true, Apply: api.ApplyLive},
		{Key: api.KeyMemoryMiB, Value: float64(4096), Apply: api.ApplyVMRestart},
	}
	if err != nil || !reflect.DeepEqual(changed, want) {
		t.Fatalf("config.set: %+v %v", changed, err)
	}
	got, _ := c.ConfigGet(bg, api.KeyMemoryMiB)
	if v := got[api.KeyMemoryMiB]; v.Value != float64(4096) || v.IsDefault || v.Pending || len(got) != 1 {
		t.Errorf("config.get: %+v", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.configs)
		h.mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	h.mu.Lock()
	if !reflect.DeepEqual(h.configs, want) {
		t.Errorf("config.changed notifications: %+v", h.configs)
	}
	h.mu.Unlock()
	if st, _ := c.Status(bg); len(st.PendingRestart) != 0 || st.Seq != 1 {
		t.Errorf("status: %+v", st)
	}

	f.d.Close()
	m.mu.Lock()
	if len(m.configs) != 2 || m.configs[0].Key != api.KeyDataAllowBackup || m.configs[0].Value != true ||
		m.configs[1].Key != api.KeyMemoryMiB || m.configs[1].Apply != api.ApplyVMRestart || !m.closed {
		t.Errorf("the module saw %+v, closed=%v", m.configs, m.closed)
	}
	m.mu.Unlock()
	f.opts.Modules = nil
	f.start()
	c = f.client()
	values, _ = c.ConfigList(bg)
	if values[api.KeyMemoryMiB].Value != float64(4096) || values[api.KeyDataAllowBackup].Value != true || !values[api.KeyCPU].IsDefault {
		t.Errorf("after a restart of the daemon: %+v", values)
	}
	st, _ := c.VMStart(bg, api.StartParams{})
	if f.launch.spec(0).MemMiB != 4096 || len(st.PendingRestart) != 0 {
		t.Errorf("the VM was started with %+v", f.launch.spec(0))
	}
	changed, _ = c.ConfigReset(bg, api.KeyMemoryMiB)
	if len(changed) != 1 || !changed[0].Pending || changed[0].Value != float64(8192) {
		t.Errorf("config.reset: %+v", changed)
	}
	if now, _ := c.Status(bg); !reflect.DeepEqual(now.PendingRestart, []string{api.KeyMemoryMiB}) || now.Seq != st.Seq+1 {
		t.Errorf("status after the reset: %+v", now)
	}
	c.ConfigSet(bg, map[string]any{api.KeyMemoryMiB: 4096})
	if now, _ := c.Status(bg); len(now.PendingRestart) != 0 || now.Seq != st.Seq+2 {
		t.Errorf("status after setting it back: %+v", now)
	}
}

func TestAListKeyAndTheProxyThroughTheAPI(t *testing.T) {
	m := &stub{}
	f := newFixture(t)
	f.opts.Modules = []Module{m}
	f.start()
	c := f.client()
	h := f.watch(f.client())
	key := api.KeyNetworkProxyExclude
	changed, err := c.ConfigAdd(bg, key, "registry.local", ".corp.example")
	if err != nil || len(changed) != 1 || !reflect.DeepEqual(changed[0].Value, []any{"registry.local", ".corp.example"}) {
		t.Fatalf("config.add: %+v %v", changed, err)
	}
	if changed, err := c.ConfigAdd(bg, key, "Registry.Local"); err != nil || len(changed) != 0 {
		t.Errorf("config.add of what is there: %+v %v", changed, err)
	}
	changed, err = c.ConfigRemove(bg, key, "registry.local")
	if err != nil || len(changed) != 1 || !reflect.DeepEqual(changed[0].Value, []any{".corp.example"}) {
		t.Errorf("config.remove: %+v %v", changed, err)
	}
	if _, err := c.ConfigAdd(bg, api.KeyCPU, "4"); code(err) != api.CodeInvalidValue || api.ConfigErrorOf(err).Key != api.KeyCPU {
		t.Errorf("config.add of a key that is not a list: %v", err)
	}
	if _, err := c.ConfigRemove(bg, "nosuch", "x"); code(err) != api.CodeUnknownKey {
		t.Errorf("config.remove of no key: %v", err)
	}
	if _, err := c.ConfigAdd(bg, key); code(err) != jsonrpc.CodeInvalidParams {
		t.Errorf("config.add with no values: %v", err)
	}
	changed, err = c.ConfigSet(bg, map[string]any{api.KeyNetworkProxy: "http://u:hunter2@proxy.example:3128"})
	if err != nil || len(changed) != 1 || changed[0].Value != "http://u:xxxxx@proxy.example:3128" {
		t.Errorf("config.set of the proxy: %+v %v", changed, err)
	}
	got, _ := c.ConfigGet(bg, api.KeyNetworkProxy)
	if got[api.KeyNetworkProxy].Value != "http://u:xxxxx@proxy.example:3128" || f.d.cfg.String(api.KeyNetworkProxy) != "http://u:hunter2@proxy.example:3128" {
		t.Errorf("config.get of the proxy: %+v", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.configs)
		h.mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	h.mu.Lock()
	if len(h.configs) != 3 || h.configs[0].Key != key || h.configs[1].Key != key || h.configs[2].Value != "http://u:xxxxx@proxy.example:3128" {
		t.Errorf("config.changed notifications: %+v", h.configs)
	}
	h.mu.Unlock()
	if st, _ := c.Status(bg); st.Seq != 1 {
		t.Errorf("status: %+v", st)
	}
	f.d.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.configs) != 3 || strings.Contains(fmt.Sprint(m.configs), "hunter2") {
		t.Errorf("the module saw %+v", m.configs)
	}
}

func TestACorruptConfigurationFileIsAWarningInTheStatus(t *testing.T) {
	f := newFixture(t)
	os.MkdirAll(f.layout.Home+"/config", 0o755)
	os.WriteFile(f.layout.ConfigFile(), []byte("{not json"), 0o644)
	f.start()
	st, err := f.client().Status(bg)
	if err != nil || len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "config.json could not be read and was set aside as config.json.corrupt-") {
		t.Errorf("status: %+v %v", st, err)
	}
	if log := f.logFile("fevd.log"); !strings.Contains(log, " daemon 4 config.json could not be read") {
		t.Errorf("the daemon's log:\n%s", log)
	}
}

func TestAModuleFollowsTheDaemon(t *testing.T) {
	m := &stub{marks: []string{boottrace.DockerReleased}}
	released := make(chan struct{})
	var vmSeen VM
	m.onState = func(st api.Status) {
		if st.State == api.StateRunning {
			vmSeen, _ = m.host.VM()
			time.Sleep(20 * time.Millisecond)
			m.host.Mark(boottrace.DockerReleased)
			close(released)
		}
	}
	f := newFixture(t)
	f.opts.Modules = []Module{m}
	f.start()
	if m.host == nil || m.host.Layout() != f.layout || !m.host.Fake() || m.host.Config() == nil || m.host.Log() == nil {
		t.Fatal("the module was not started with its host")
	}
	if _, ok := m.host.VM(); ok {
		t.Error("a VM before any was started")
	}
	c := f.client()
	if d, err := c.DockerStatus(bg); err != nil || d.Context.Name != "fev" {
		t.Errorf("docker.status through the module: %+v %v", d, err)
	}
	if fw, err := c.Forwards(bg); err != nil || len(fw) != 2 {
		t.Errorf("forwards.list through the module: %+v %v", fw, err)
	}
	running, _ := c.VMStart(bg, api.StartParams{})
	if traces := f.traces(); len(traces) != 0 {
		t.Errorf("the record was closed before the module's mark: %d lines", len(traces))
	}
	<-released
	if vmSeen == nil || vmSeen.BootID() != running.BootID || vmSeen.Agent() == nil {
		t.Errorf("the VM the module saw: %v", vmSeen)
	}
	tr, err := c.BootTrace(bg)
	if err != nil || !tr.Complete || !tr.OK || len(tr.Stages) != 6 || tr.Stages[5].MS < 15 || tr.TotalMS != tr.Marks[len(tr.Marks)-1].MS {
		t.Errorf("trace: %+v %v", tr, err)
	}
	if traces := f.traces(); len(traces) != 1 || !traces[0].Complete {
		t.Errorf("the boot trace log: %d lines", len(traces))
	}

	f.d.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		t.Error("the module was not closed")
	}
	for i, st := range m.states {
		if st.Seq != uint64(i)+2 {
			t.Fatalf("the module's transition %d has seq %d", i, st.Seq)
		}
	}
	if last := m.states[len(m.states)-1]; last.State != api.StateStopped || last.Seq != f.d.hub.Status().Seq {
		t.Errorf("the module's last transition: %+v", last)
	}
}

func TestWithoutItsModuleAMethodSaysSoAndAModuleThatCannotStartStopsTheDaemon(t *testing.T) {
	f := started(t)
	c := f.client()
	if _, err := c.DockerStatus(bg); code(err) != api.CodeFailed || !strings.Contains(err.Error(), "no module of this daemon provides it") {
		t.Errorf("docker.status with no module: %v", err)
	}
	if _, err := c.Forwards(bg); code(err) != api.CodeFailed {
		t.Errorf("forwards.list with no module: %v", err)
	}

	g := newFixture(t)
	ready, read := readyPipe(t)
	g.opts.Ready = ready
	g.opts.Modules = []Module{&stub{}, &stub{fail: errors.New("docker.sock: address already in use")}}
	d, err := Start(g.opts)
	if err == nil || d != nil || !strings.Contains(err.Error(), "stub: docker.sock: address already in use") {
		t.Fatalf("Start: %v", err)
	}
	if r := read(); r.Status != launch.StatusError || !strings.Contains(r.Error, "address already in use") {
		t.Errorf("ready line: %+v", r)
	}
	if !g.opts.Modules[0].(*stub).closed {
		t.Error("the module that had started was not closed")
	}
	g.opts.Modules, g.opts.Ready = nil, nil
	g.start()
}

func TestADaemonThatIsShuttingDownRefusesWork(t *testing.T) {
	f := started(t, []string{"stop-ms=400"})
	c := f.client()
	other := f.raw()
	wire := f.conn()
	fmt.Fprintf(wire, `{"jsonrpc":"2.0","id":1,"method":"daemon.hello","params":{"api_version":%d,"build":%q,"client":"wire"}}`+"\n", api.APIVersion, version.Build)
	sc := bufio.NewScanner(wire)
	sc.Buffer(nil, 1<<20)
	sc.Scan()
	fmt.Fprintln(wire, `{"jsonrpc":"2.0","id":2,"method":"events.subscribe"}`)
	c.VMStart(bg, api.StartParams{})
	if _, err := c.Shutdown(bg, api.ShutdownParams{}); err != nil {
		t.Fatal(err)
	}
	f.until("stopping", func(st api.Status) bool { return st.State == api.StateStopping })
	for method := range api.Schema {
		if api.Schema[method].Result == nil {
			continue
		}
		if err := other.Call(bg, method, nil, nil); code(err) != api.CodeShuttingDown {
			t.Errorf("%s during shutdown: %v", method, err)
		}
	}
	if _, err := c.VMStart(bg, api.StartParams{}); !api.IsShuttingDown(err) {
		t.Errorf("vm.start during shutdown: %v", err)
	}
	if _, err := net.Dial("unix", f.layout.Socket()); err == nil {
		t.Error("the socket still accepts during shutdown")
	}
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the client's connection did not end with the daemon")
	}
	var last api.StateChanged
	wire.SetReadDeadline(time.Now().Add(10 * time.Second))
	for sc.Scan() {
		var m struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &m); m.Method == api.NotifyStateChanged {
			json.Unmarshal(m.Params, &last)
		}
	}
	if last.Status.State != api.StateStopped || last.Status.LastExit == nil || alive(f.vmmPID0()) {
		t.Errorf("the last status the daemon sent a subscriber: %+v", last.Status)
	}
	for _, name := range []string{f.layout.InfoFile(), f.layout.Socket()} {
		if _, err := os.Stat(name); err == nil {
			t.Errorf("%s is still there after shutdown", name)
		}
	}
	if log := f.logFile("fevd.log"); !strings.HasSuffix(log, "fevd has stopped\n") {
		t.Errorf("the daemon's log ends:\n%s", log[max(0, len(log)-300):])
	}
}

func (f *fixture) vmmPID0() int {
	info, _ := f.layout.ReadInfo()
	return info.VMMPID
}

func TestAForcedShutdownDoesNotWaitForTheGuest(t *testing.T) {
	f := started(t, []string{"ignore-shutdown=1"})
	c := f.client()
	c.VMStart(bg, api.StartParams{})
	pid := f.vmmPID()
	begun := time.Now()
	if _, err := c.Shutdown(bg, api.ShutdownParams{Force: true}); err != nil {
		t.Fatal(err)
	}
	<-f.d.Done()
	if took := time.Since(begun); took > 250*time.Millisecond || alive(pid) {
		t.Errorf("a forced shutdown took %v; VMM alive=%v", took, alive(pid))
	}
	g := started(t, []string{"ignore-shutdown=1"})
	gc := g.client()
	gc.VMStart(bg, api.StartParams{})
	go g.d.Shutdown(false)
	g.until("stopping", func(st api.Status) bool { return st.State == api.StateStopping })
	begun = time.Now()
	g.d.Shutdown(true)
	if took := time.Since(begun); took > 250*time.Millisecond {
		t.Errorf("forcing a shutdown under way took %v", took)
	}
}
