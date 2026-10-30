package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func TestConfigShowGetSetReset(t *testing.T) {
	f := newFake(t)
	golden(t, "config.show", f.run("config", "show").want(t, 0).out)
	sameJSON(t, f.run("config", "show", "--format", "json").out, fixtureResult(t, "config.list"))
	golden(t, "config.describe", f.run("config", "show", "--describe").want(t, 0).out)
	sameJSON(t, f.run("config", "show", "--describe", "--format", "json").out, fixtureResult(t, "config.describe"))

	get := fixtureFile(t, "config.get")
	var keys api.ConfigGetParams
	json.Unmarshal(get.Params, &keys)
	r := f.run(append([]string{"config", "get"}, keys.Keys...)...).want(t, 0)
	golden(t, "config.get", r.out)
	if got := f.params(api.MethodConfigGet); got != compact(get.Params) {
		t.Errorf("config.get params: %s, want %s", got, compact(get.Params))
	}
	sameJSON(t, f.run(append([]string{"config", "get", "--format", "json"}, keys.Keys...)...).out, get.Result)
	f.on(api.MethodConfigGet, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return api.ConfigValues{Values: map[string]api.ConfigValue{"cpu": {Value: 4}}}, nil
	})
	if out := f.run("config", "get", "-q", "cpu").want(t, 0).out; out != "4\n" {
		t.Errorf("config get -q: %q", out)
	}

	set := fixtureFile(t, "config.set")
	r = f.run("config", "set", "memory_mib=4096", "data_allow_backup=true").want(t, 0)
	golden(t, "config.set", r.out)
	sameParams(t, f.params(api.MethodConfigSet), set.Params)
	f2 := newFake(t)
	sameJSON(t, f2.run("config", "set", "--format", "json", "cpu", "4").want(t, 0).out, set.Result)
	if got := f2.params(api.MethodConfigSet); got != `{"values":{"cpu":4}}` {
		t.Errorf("config set KEY VALUE: %s", got)
	}

	reset := fixtureFile(t, "config.reset")
	var rk api.ConfigResetParams
	json.Unmarshal(reset.Params, &rk)
	f3 := newFake(t)
	r = f3.run(append([]string{"config", "reset"}, rk.Keys...)...).want(t, 0)
	golden(t, "config.reset", r.out)
	if got := f3.params(api.MethodConfigReset); got != compact(reset.Params) {
		t.Errorf("config.reset params: %s, want %s", got, compact(reset.Params))
	}
	f4 := newFake(t)
	f4.run("config", "reset", "--all").want(t, 0)
	if got := f4.params(api.MethodConfigReset); got != `{}` {
		t.Errorf("config reset --all params: %s", got)
	}
	if r := f4.run("config", "reset", "--all", "cpu"); r.code != ExitUsage {
		t.Errorf("config reset --all cpu: exit %d", r.code)
	}
}

func TestRefusedConfigurationIsExitFive(t *testing.T) {
	f := newFake(t)
	refuse := func(code int, key, message string) {
		f.on(api.MethodConfigSet, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
			data, _ := json.Marshal(api.ConfigError{Key: key, Reason: "x"})
			return nil, &jsonrpc.Error{Code: code, Message: message, Data: data}
		})
	}
	refuse(api.CodeUnknownKey, "cpus", `no configuration key "cpus"; did you mean "cpu"?`)
	r := f.run("config", "set", "cpus", "2").want(t, ExitConfig)
	if r.err != "fev: no configuration key \"cpus\"; did you mean \"cpu\"?\n" || r.out != "" {
		t.Errorf("stderr %q stdout %q", r.err, r.out)
	}
	r = f.run("config", "set", "--format", "json", "cpus", "2").want(t, ExitConfig)
	if compact([]byte(r.err)) != `{"code":5,"message":"no configuration key \"cpus\"; did you mean \"cpu\"?","key":"cpus"}` {
		t.Errorf("JSON error: %s", r.err)
	}

	f2 := newFake(t)
	f2.on(api.MethodConfigSet, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		data, _ := json.Marshal(api.ConfigError{Key: "cpu"})
		return nil, &jsonrpc.Error{Code: api.CodeInvalidValue, Message: "cpu must be a whole number", Data: data}
	})
	f2.run("config", "set", "cpu=many", "data_allow_backup=yes").want(t, ExitConfig)
	if got := f2.params(api.MethodConfigSet); got != `{"values":{"cpu":"many","data_allow_backup":"yes"}}` {
		t.Errorf("values that do not parse were sent as %s", got)
	}

	for _, method := range []string{api.MethodConfigAdd, api.MethodConfigRemove} {
		f2.on(method, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
			data, _ := json.Marshal(api.ConfigError{Key: "cpu", Reason: "is a int, not a list"})
			return nil, &jsonrpc.Error{Code: api.CodeInvalidValue, Message: "cpu: is a int, not a list", Data: data}
		})
	}
	for _, verb := range []string{"add", "remove"} {
		r := f2.run("config", verb, "cpu", "1").want(t, ExitConfig)
		if !strings.Contains(r.err, "cpu: is a int, not a list") {
			t.Errorf("config %s: %q", verb, r.err)
		}
	}
	if r := f2.run("config", "add", "network.proxy_exclude"); r.code != ExitUsage {
		t.Errorf("config add with no value: exit %d", r.code)
	}
}

func TestConfigAddAndRemoveChangeAListKey(t *testing.T) {
	f := newFake(t)
	add := fixtureFile(t, "config.add")
	r := f.run("config", "add", "network.proxy_exclude", ".corp.example", "10.20.0.0/16").want(t, 0)
	golden(t, "config.add", r.out)
	if got := f.params(api.MethodConfigAdd); got != compact(add.Params) {
		t.Errorf("config.add params: %s, want %s", got, compact(add.Params))
	}
	sameJSON(t, newFake(t).run("config", "add", "--format", "json", "network.proxy_exclude", ".corp.example").want(t, 0).out, add.Result)

	rm := fixtureFile(t, "config.remove")
	f2 := newFake(t)
	r = f2.run("config", "remove", "network.proxy_exclude", "10.20.0.0/16").want(t, 0)
	golden(t, "config.remove", r.out)
	if got := f2.params(api.MethodConfigRemove); got != compact(rm.Params) {
		t.Errorf("config.remove params: %s, want %s", got, compact(rm.Params))
	}

	f3 := newFake(t)
	f3.fixture(api.MethodConfigAdd, "config.add.unchanged")
	if r := f3.run("config", "add", "network.proxy_exclude", "registry.local").want(t, 0); r.out != "" {
		t.Errorf("nothing changed, and it printed %q", r.out)
	}

	f4 := newFake(t)
	f4.run("config", "set", "network.proxy_exclude", `["a.example","b.example"]`).want(t, 0)
	f4.run("config", "set", "network.proxy_exclude=a.example,b.example").want(t, 0)
	var sets []string
	for _, c := range f4.made() {
		if c.Method == api.MethodConfigSet {
			sets = append(sets, c.Params)
		}
	}
	if len(sets) != 2 || sets[0] != `{"values":{"network.proxy_exclude":["a.example","b.example"]}}` || sets[1] != `{"values":{"network.proxy_exclude":"a.example,b.example"}}` {
		t.Errorf("config set of a list sent %v", sets)
	}

	f5 := newFake(t)
	f5.fixture(api.MethodConfigGet, "config.get.proxy")
	r = f5.run("config", "get", "network.proxy", "network.proxy_exclude").want(t, 0)
	golden(t, "config.get.proxy", r.out)
	if strings.Contains(r.out, "hunter2") || !strings.Contains(r.out, "xxxxx@") {
		t.Errorf("config get: %q", r.out)
	}
}

func TestConfigDockerEditsRestartsAndReportsTheVerdict(t *testing.T) {
	f := newFake(t)
	file := filepath.Join(t.TempDir(), "config", "docker.json")
	status := func(state, why string) handler {
		return func(*jsonrpc.Conn, json.RawMessage) (any, error) {
			return api.DockerStatus{Engine: api.EngineReady, Socket: "/s", ConfigFile: file, ConfigState: state, ConfigError: why}, nil
		}
	}
	f.on(api.MethodDockerStatus, status(api.EngineConfigNone, ""))
	f.on(api.MethodStatus, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return api.Status{State: api.StateRunning, Engine: api.EngineReady, PendingRestart: []string{}}, nil
	})
	editor := filepath.Join(t.TempDir(), "editor")
	os.WriteFile(editor, []byte("#!/bin/sh\nprintf '%s' \"$NEW\" >\"$1\"\n"), 0o755)
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", editor)

	t.Setenv("NEW", `{"debug": true}`)
	f.on(api.MethodDockerRestart, status(api.EngineConfigApplied, ""))
	r := f.run("config", "docker").want(t, 0)
	if !strings.Contains(r.out, "(applied)") {
		t.Errorf("stdout: %q", r.out)
	}
	if data, _ := os.ReadFile(file); string(data) != `{"debug": true}` {
		t.Errorf("the file holds %q", data)
	}
	f.params(api.MethodDockerRestart)

	f2 := newFake(t)
	f2.on(api.MethodDockerStatus, status(api.EngineConfigApplied, ""))
	r = f2.run("config", "docker").want(t, 0)
	if f2.called(api.MethodDockerRestart) || !strings.Contains(r.err, "unchanged") {
		t.Errorf("an unchanged file: restart called %v, stderr %q", f2.called(api.MethodDockerRestart), r.err)
	}

	t.Setenv("NEW", `{"bogus": 1}`)
	f2.on(api.MethodStatus, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return api.Status{State: api.StateRunning, Engine: api.EngineReady, PendingRestart: []string{}}, nil
	})
	f2.on(api.MethodDockerRestart, status(api.EngineConfigRefused, "the following directives don't match any configuration option: bogus"))
	r = f2.run("config", "docker").want(t, ExitConfig)
	if !strings.Contains(r.err, "directives don't match any configuration option: bogus") || !strings.Contains(r.err, "still in force") {
		t.Errorf("stderr: %q", r.err)
	}

	f3 := newFake(t)
	f3.on(api.MethodDockerStatus, status(api.EngineConfigApplied, ""))
	t.Setenv("EDITOR", "false")
	f3.run("config", "docker").want(t, ExitFailed)
	if f3.called(api.MethodDockerRestart) {
		t.Error("the engine was restarted after the editor failed")
	}
}

func TestConfigDockerOnAStoppedVMChecksTheFileAndStartsNothing(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config", "docker.json")
	editor := filepath.Join(t.TempDir(), "editor")
	os.WriteFile(editor, []byte("#!/bin/sh\nprintf '%s' \"$NEW\" >\"$1\"\n"), 0o755)
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", editor)
	in := func(state string) *fake {
		f := newFake(t)
		f.on(api.MethodDockerStatus, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
			return api.DockerStatus{Engine: api.EngineStopped, Socket: "/s", ConfigFile: file, ConfigState: api.EngineConfigNone}, nil
		})
		f.on(api.MethodStatus, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
			return api.Status{State: state, Engine: api.EngineStopped, PendingRestart: []string{}}, nil
		})
		return f
	}
	startedNothing := func(f *fake, what string) {
		t.Helper()
		for _, method := range []string{api.MethodDockerRestart, api.MethodVMStart, api.MethodVMRestart} {
			if f.called(method) {
				t.Errorf("%s: %s was called", what, method)
			}
		}
	}
	for _, c := range []struct {
		name, text string
		code       int
		says       []string
	}{
		{"valid", `{"debug": true}`, 0, []string{"saved; it is valid JSON. The VM is stopped and nothing was started", "given the file when the VM next starts", "last configuration that passed", `"fev status" says why`}},
		{"empty", "  \n", 0, []string{"valid JSON"}},
		{"a trailing comma", "{\n  \"debug\": true,\n}\n", ExitConfig, []string{file + ": it is not valid JSON: invalid character '}' looking for beginning of object key string (line 3, column 1)",
			"It is saved as you left it", "the VM is stopped, and when the VM next starts the engine is not given this file", "last configuration that passed", `"fev status" warns`, `"fev config docker" opens it again`}},
		{"not an object", `["debug"]`, ExitConfig, []string{"it must hold a JSON object"}},
		{"null", `null`, ExitConfig, []string{"it must hold a JSON object, not null"}},
	} {
		os.Remove(file)
		t.Setenv("NEW", c.text)
		f := in(api.StateStopped)
		r := f.run("config", "docker").want(t, c.code)
		for _, want := range c.says {
			if !strings.Contains(r.err, want) {
				t.Errorf("%s: stderr does not say %q:\n%s", c.name, want, r.err)
			}
		}
		if data, _ := os.ReadFile(file); string(data) != c.text {
			t.Errorf("%s: the file holds %q", c.name, data)
		}
		startedNothing(f, c.name)
	}

	t.Setenv("NEW", `{"debug": false}`)
	f := in(api.StateStarting)
	if r := f.run("config", "docker").want(t, 0); !strings.Contains(r.err, "The VM is starting and nothing was started: the engine is given the file when its engine is ready") {
		t.Errorf("starting: %q", r.err)
	}
	startedNothing(f, "starting")

	t.Setenv("NEW", `{"debug": tru}`)
	f = in(api.StateRunning)
	r := f.run("config", "docker").want(t, ExitConfig)
	if !strings.Contains(r.err, "it is not valid JSON") || !strings.Contains(r.err, "the engine was not restarted and keeps the configuration it runs on") {
		t.Errorf("running, a syntax error: %q", r.err)
	}
	startedNothing(f, "running, a syntax error")

	t.Setenv("NEW", `{"debug": true}`)
	f = in(api.StatePaused)
	f.on(api.MethodDockerRestart, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return api.DockerStatus{Engine: api.EngineReady, Socket: "/s", ConfigFile: file, ConfigState: api.EngineConfigApplied}, nil
	})
	if r := f.run("config", "docker").want(t, 0); !strings.Contains(r.out, "(applied)") {
		t.Errorf("paused: stdout %q, stderr %q", r.out, r.err)
	}
	f.params(api.MethodDockerRestart)

	os.WriteFile(file, []byte(`{"debug": tru}`), 0o644)
	t.Setenv("EDITOR", "true")
	for _, state := range []string{api.StateStopped, api.StateRunning} {
		f = in(state)
		r = f.run("config", "docker").want(t, ExitConfig)
		if !strings.Contains(r.err, file+" is unchanged, and it is not valid JSON") || !strings.Contains(r.err, "keeps the last configuration that passed") {
			t.Errorf("%s, an unchanged invalid file: %q", state, r.err)
		}
		startedNothing(f, state+", an unchanged invalid file")
	}
}

func TestAnEngineConfigurationsSyntaxErrorIsPlaced(t *testing.T) {
	for _, c := range []struct {
		text         string
		line, column int
	}{
		{`{"a": }`, 1, 7},
		{"{\n\"a\": 1,\n}", 3, 1},
		{"{\n  \"a\": 1\n  \"b\": 2\n}", 3, 3},
		{`{"a": 1`, 1, 7},
	} {
		var v map[string]json.RawMessage
		err := json.Unmarshal([]byte(c.text), &v)
		var syntax *json.SyntaxError
		if !errors.As(err, &syntax) {
			t.Fatalf("%q: %v", c.text, err)
		}
		if line, column := position([]byte(c.text), syntax.Offset); line != c.line || column != c.column {
			t.Errorf("%q (%v, offset %d): line %d, column %d; want %d, %d", c.text, err, syntax.Offset, line, column, c.line, c.column)
		}
	}
}
