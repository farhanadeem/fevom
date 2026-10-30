package standin

import (
	"context"
	"encoding/json"
	"errors"
	"fev/host/docker/fakeguest"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
	"fev/proto/netplan"
	"fev/proto/version"
)

var bg = context.Background()

func booted(t *testing.T) *Agent {
	t.Helper()
	start := time.Now()
	a := NewAgent(version.Build, func() float64 { return float64(time.Since(start).Microseconds())/1000 + 10 })
	a.EngineDelay = time.Millisecond
	a.Boot(8)
	return a
}

func connect(t *testing.T, a *Agent) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	a.ServeConn(server)
	t.Cleanup(func() { client.Close() })
	return client
}

func code(err error) int {
	var e *jsonrpc.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

func TestTheFakeAgentAnswersEveryFixtureWithASchemaResult(t *testing.T) {
	fixtures, err := api.Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	a := booted(t)
	a.OnShutdown = func(bool, time.Duration) {}
	c := jsonrpc.NewConn(connect(t, a), jsonrpc.HandlerFunc(func(context.Context, *jsonrpc.Conn, string, json.RawMessage) (any, error) {
		return nil, nil
	}))
	defer c.Close()
	if err := c.Call(bg, api.MethodHello, api.HelloParams{APIVersion: api.APIVersion}, nil); err != nil {
		t.Fatal(err)
	}
	answered := map[string]bool{}
	for _, fx := range fixtures {
		if fx.Notification {
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
			case api.MethodLogUnsubscribe, api.MethodEngineUnsubscribe, api.MethodStatsUnsubscribe:
				var sub api.Subscription
				if err := c.Call(bg, strings.Replace(fx.Method, "unsubscribe", "subscribe", 1), nil, &sub); err != nil {
					t.Fatal(err)
				}
				params, _ = json.Marshal(sub)
			case api.MethodExecStart:
				a.setEngine(func(e *api.EngineStatus) { e.State = api.EngineReady })
				seedMachines(t, c, a, fx)
			case api.MethodMachineInfo, api.MethodMachineStart, api.MethodMachineStop, api.MethodMachineRestart,
				api.MethodMachineDelete, api.MethodMachineClone, api.MethodMachineRename, api.MethodMachineExport,
				api.MethodMachineCreate, api.MethodMachineImport:
				seedMachines(t, c, a, fx)
			case api.MethodVolumeClone, api.MethodVolumeExport, api.MethodVolumeImport:
				e := fakeguest.NewEngine()
				if fx.Method != api.MethodVolumeImport {
					e.MakeVolume(fakeguest.VolumeData{Name: "pgdata", Labels: map[string]string{"com.docker.compose.project": "shop"}, Files: map[string]string{"PG_VERSION": "17\n"}})
				}
				a.Volumes = e
			case api.MethodK8sImagePull:
				if err := a.SetK8s(K8sOptions{Wanted: true, Cached: true, Plan: netplan.Default()}); err != nil {
					t.Fatal(err)
				}
			case api.MethodFileShareWatch, api.MethodFileShareBind:
				a.Share = &Share{Root: "/"}
			case api.MethodExecResize, api.MethodExecSignal, api.MethodExecClose:
				var started api.ExecStarted
				if err := c.Call(bg, api.MethodExecStart, api.ExecStartParams{Context: api.ExecInit, Argv: []string{"/bin/true"}}, &started); err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				json.Unmarshal(fx.Params, &fields)
				fields["id"] = started.ID
				params, _ = json.Marshal(fields)
			}
			var result json.RawMessage
			if err := c.Call(bg, fx.Method, params, &result); err != nil {
				t.Fatal(err)
			}
			if err := api.Conforms(result, api.Schema[fx.Method].Result); err != nil {
				t.Fatalf("result %s does not fit the schema: %v", result, err)
			}
		})
	}
	for method, types := range api.Schema {
		if types.Result != nil && !answered[method] {
			t.Errorf("%s is in the schema and has no fixture the fake was held to", method)
		}
	}
}

func TestTheFakeAgentEnforcesTheHandshakeAsTheRealOneDoes(t *testing.T) {
	a := booted(t)
	c := jsonrpc.NewConn(connect(t, a), nil)
	if err := c.Call(bg, api.MethodPing, nil, nil); code(err) != api.CodeHelloRequired {
		t.Errorf("a call before hello: %v", err)
	}
	if err := c.Call(bg, "no.such", nil, nil); code(err) != jsonrpc.CodeMethodNotFound {
		t.Errorf("an unknown method: %v", err)
	}
	c = jsonrpc.NewConn(connect(t, a), nil)
	if err := c.Call(bg, api.MethodHello, api.HelloParams{APIVersion: api.APIVersion + 1}, nil); code(err) != api.CodeVersionMismatch {
		t.Errorf("another API version: %v", err)
	}
	a.Build = "abc1234"
	_, _, err := api.NewClient(bg, connect(t, a), "test")
	var m *version.Mismatch
	if !errors.As(err, &m) || m.Theirs != "abc1234" || m.Ours != version.Build {
		t.Errorf("a fake of another build: %v", err)
	}
}

func TestTheFakeAgentBehavesAsADaemonNeeds(t *testing.T) {
	start := time.Now()
	a := NewAgent(version.Build, func() float64 { return float64(time.Since(start).Microseconds())/1000 + 10 })
	a.EngineDelay = 30 * time.Millisecond
	var consoleLines []string
	a.Console = func(text string) { consoleLines = append(consoleLines, text) }
	stopped := make(chan time.Duration, 1)
	a.OnShutdown = func(reboot bool, deadline time.Duration) { stopped <- deadline }
	c, hello, err := api.NewClient(bg, connect(t, a), "test")
	if err != nil {
		t.Fatal(err)
	}
	if hello.AgentVersion != version.Build || hello.Kernel == "" {
		t.Errorf("hello: %+v", hello)
	}

	early, err := c.BootTrace(bg, false)
	if err != nil || early.ReadyMS != 0 || len(early.Stages) != 0 || early.NowMS < 10 {
		t.Errorf("early trace: %+v %v", early, err)
	}
	waited := make(chan *api.BootTrace, 1)
	go func() {
		bt, _ := c.BootTrace(bg, true)
		waited <- bt
	}()
	select {
	case <-waited:
		t.Fatal("boot_trace with wait returned before init was ready")
	case <-time.After(30 * time.Millisecond):
	}
	a.Boot(12)
	bt := <-waited
	if bt == nil || bt.InitStartMS != 12 || bt.ReadyMS <= 12 || len(bt.Stages) != 5 || bt.NowMS < bt.ReadyMS {
		t.Fatalf("trace: %+v", bt)
	}
	if !reflect.DeepEqual(consoleLines, []string{consoleLines[0]}) || !strings.HasPrefix(consoleLines[0], "init: ready") {
		t.Errorf("init's lines on the console: %q", consoleLines)
	}

	states := make(chan api.EngineStatus, 8)
	if _, err := c.SubscribeEngine(bg, func(s api.EngineStatus) { states <- s }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{api.EngineStarting, api.EngineReady} {
		select {
		case s := <-states:
			if s.State != want || s.Starts != 1 || s.StartedMS == 0 || (want == api.EngineReady) != (s.ReadyMS > 0) {
				t.Errorf("engine state %+v, want %s", s, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no engine state %s", want)
		}
	}
	if st, err := c.EngineRestart(bg, api.EngineStopParams{}); err != nil || st.State != api.EngineStarting || st.Starts != 2 {
		t.Errorf("restart: %+v %v", st, err)
	}
	for _, want := range []string{api.EngineStopped, api.EngineStarting, api.EngineReady} {
		if s := <-states; s.State != want {
			t.Errorf("after a restart: %s, want %s", s.State, want)
		}
	}
	if again, _ := c.BootTrace(bg, false); again.EngineStartMS == 0 || again.EngineReadyMS <= again.EngineStartMS || again.EngineReadyMS > bt.NowMS+100 {
		t.Errorf("engine marks in the trace: %+v", again)
	}

	lines := make(chan api.LogEntry, 64)
	if _, err := c.SubscribeLogs(bg, api.LogSubscribeParams{Backlog: true, Sources: []string{api.LogKernel, api.LogInit}}, func(e api.LogEntry) { lines <- e }); err != nil {
		t.Fatal(err)
	}
	var texts []string
	for len(texts) < 3 {
		e := <-lines
		if e.Source == api.LogEngine {
			t.Errorf("a source that was not asked for: %+v", e)
		}
		texts = append(texts, e.Source+": "+e.Text)
	}
	if texts[1] != "kernel: Run /init as init process" || !strings.HasPrefix(texts[2], "init: init: ready") {
		t.Errorf("backlog: %q", texts)
	}
	a.Logf(api.LogInit, 4, "live line")
	if e := <-lines; e.Text != "live line" || e.Level != 4 {
		t.Errorf("live: %+v", e)
	}

	var phases []string
	done := make(chan struct{})
	err = c.Shutdown(bg, api.ShutdownParams{DeadlineMS: 2500}, func(p api.ShutdownProgress) {
		phases = append(phases, p.Phase)
		if p.Phase == api.ShutdownOff {
			close(done)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-done
	want := []string{api.ShutdownBegin, api.ShutdownEngine, api.ShutdownSessions, api.ShutdownSync, api.ShutdownUnmount, api.ShutdownOff}
	if !reflect.DeepEqual(phases, want) || <-stopped != 2500*time.Millisecond {
		t.Errorf("phases: %v", phases)
	}
	if err := c.Shutdown(bg, api.ShutdownParams{DeadlineMS: api.MaxShutdownDeadlineMS + 1}, nil); code(err) != jsonrpc.CodeInvalidParams {
		t.Errorf("a deadline over the limit: %v", err)
	}
}

func TestTheStandInReadsTheVMMsCommandLine(t *testing.T) {
	o, err := parse(strings.Fields("--kernel K --initrd I --cpus 4 --mem 2048 --disk d.img --disk s.img,ro --net n.sock --vsock-dir /tmp/v --vsock-port 1024 --vsock-port 2375 --console stdio --stamp --no-reboot --verbose --trace-fd 3 --lifeline-fd 4 --standin stop-ms=7 --standin vmm-build=x"))
	if err != nil {
		t.Fatal(err)
	}
	if o.cpus != 4 || o.mem != 2048 || o.vsockDir != "/tmp/v" || !reflect.DeepEqual(o.ports, []uint32{1024, 2375}) || o.traceFD != 3 || o.lifelineFD != 4 {
		t.Errorf("parsed: %+v", o)
	}
	if o.ms("stop-ms", 5) != 7*time.Millisecond || o.ms("engine-ms", 20) != 20*time.Millisecond || o.set["vmm-build"] != "x" {
		t.Errorf("instructions: %v", o.set)
	}
	for _, bad := range []string{"--cpus", "--cpus four", "--bogus 1", "--standin novalue", "--vsock-port -1"} {
		if _, err := parse(strings.Fields(bad)); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if _, err := parse([]string{"--cmdline", "console=hvc0 quiet", "--cpus", "1"}); err != nil {
		t.Error(err)
	}
}

func TestTheFakeAgentKeepsTheEngineConfiguration(t *testing.T) {
	file := t.TempDir() + "/engine.json"
	a := booted(t)
	a.ConfigFile = file
	client, _, err := api.NewClient(bg, connect(t, a), "test")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		if st, _ := client.EngineStatus(bg); st.State == api.EngineReady {
			break
		}
		time.Sleep(time.Millisecond)
	}
	configure := func(config string) *api.EngineConfigured {
		t.Helper()
		r, err := client.EngineConfigure(bg, json.RawMessage(config))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := configure(`{}`); r.State != api.EngineConfigUnchanged || r.RestartNeeded {
		t.Fatalf("none, with none kept: %+v", r)
	}
	if r := configure(`{"debug": true, "a": {"y": 1, "x": 2}}`); r.State != api.EngineConfigAccepted || !r.RestartNeeded {
		t.Fatalf("a new one: %+v", r)
	}
	if r := configure(`{"a":{"x":2,"y":1},"debug":true}`); r.State != api.EngineConfigUnchanged || !r.RestartNeeded {
		t.Fatalf("the same again: %+v", r)
	}
	if r := configure(`{"data-root":"/x"}`); r.State != api.EngineConfigRefused || !strings.Contains(r.Error, "data-root is set by fev") {
		t.Fatalf("a reserved key: %+v", r)
	}
	if r := configure(`{"bogus-key":1}`); r.State != api.EngineConfigRefused || !strings.Contains(r.Error, "bogus-key") {
		t.Fatalf("a key dockerd does not know: %+v", r)
	}
	if _, err := client.EngineRestart(bg, api.EngineStopParams{}); err != nil {
		t.Fatal(err)
	}
	if r := configure(`{"a":{"x":2,"y":1},"debug":true}`); r.State != api.EngineConfigUnchanged || r.RestartNeeded {
		t.Fatalf("after the restart: %+v", r)
	}
	b := booted(t)
	b.ConfigFile = file
	other, _, err := api.NewClient(bg, connect(t, b), "test")
	if err != nil {
		t.Fatal(err)
	}
	if r, err := other.EngineConfigure(bg, json.RawMessage(`{"debug":true,"a":{"x":2,"y":1}}`)); err != nil || r.State != api.EngineConfigUnchanged {
		t.Fatalf("after a new boot: %+v, %v", r, err)
	}
	if r, err := other.EngineConfigure(bg, nil); err != nil || r.State != api.EngineConfigAccepted {
		t.Fatalf("back to none: %+v, %v", r, err)
	}
}

var standinConfig = api.MachineConfig{User: "ada", MAC: "02:42:c6:13:00:0a", Address: "198.19.0.10", Share: true}

func seedMachines(t *testing.T, c *jsonrpc.Conn, a *Agent, fx api.Fixture) {
	t.Helper()
	var p struct {
		Name    string `json:"name"`
		NewName string `json:"new_name"`
		Machine string `json:"machine"`
	}
	json.Unmarshal(fx.Params, &p)
	switch fx.Method {
	case api.MethodMachineCreate, api.MethodMachineImport:
		a.RemoveMachine(p.Name)
		return
	case api.MethodExecStart:
		if p.Machine == "" {
			return
		}
		p.Name = p.Machine
	}
	a.RemoveMachine(p.Name)
	a.AddMachine(p.Name, standinConfig)
	if p.NewName != "" {
		a.RemoveMachine(p.NewName)
	}
	if fx.Method == api.MethodExecStart || fx.Method == api.MethodMachineStop {
		if err := c.Call(bg, api.MethodMachineStart, api.MachineStartParams{Name: p.Name, Config: standinConfig}, nil); err != nil {
			t.Fatal(err)
		}
	}
}
