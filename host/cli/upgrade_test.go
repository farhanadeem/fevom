package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fev/host/daemon/launch"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/version"
)

func otherBuild(t *testing.T, f *fake, build string, data api.BuildMismatch) (started string, stopped *[]int) {
	t.Helper()
	data.DaemonBuild, data.ClientBuild = "aaaaaaa", version.Build
	f.on(api.MethodHello, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		raw, _ := json.Marshal(data)
		return nil, &jsonrpc.Error{Code: api.CodeBuildMismatch, Message: "build mismatch", Data: raw}
	})
	dir := t.TempDir()
	started = filepath.Join(dir, "started")
	script := filepath.Join(dir, "fevd")
	text := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = --version ]; then echo %s; exit 0; fi\necho \"$@\" >>%s\nexit 1\n", build, started)
	if err := os.WriteFile(script, []byte(text), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(launch.DaemonEnv, script)
	stopped = &[]int{}
	old := terminate
	t.Cleanup(func() { terminate = old })
	terminate = func(pid int) error {
		*stopped = append(*stopped, pid)
		go f.exit()
		return nil
	}
	return started, stopped
}

func TestACommandRestartsADaemonOfAnotherBuildWhenNothingRuns(t *testing.T) {
	none := 0
	for name, data := range map[string]api.BuildMismatch{
		"the VM stopped":            {PID: 7001, State: api.StateStopped},
		"running with no container": {PID: 7002, State: api.StateRunning, RunningContainers: &none},
	} {
		f := newFake(t)
		started, stopped := otherBuild(t, f, version.Build, data)
		r := f.run("start")
		if len(*stopped) != 1 || (*stopped)[0] != data.PID {
			t.Fatalf("%s: asked %v to stop, want the daemon's pid %d\n%s", name, *stopped, data.PID, r.err)
		}
		args, _ := os.ReadFile(started)
		if !strings.Contains(string(args), "--foreground") || strings.Contains(string(args), "--no-autostart") {
			t.Errorf("%s: the fevd beside the CLI was started with %q", name, args)
		}
		if !strings.Contains(r.err, `the running daemon is build "aaaaaaa"; restarting it into build "`+version.Build+`"`) {
			t.Errorf("%s: stderr %q does not say what was done", name, r.err)
		}
	}
	f := newFake(t)
	started, _ := otherBuild(t, f, version.Build, api.BuildMismatch{PID: 7003, State: api.StateRunning, RunningContainers: &none})
	f.run("config", "set", "cpu", "2")
	if args, _ := os.ReadFile(started); !strings.Contains(string(args), "--foreground") || strings.Contains(string(args), "--no-autostart") {
		t.Errorf("after a restart under a running VM fevd was started with %q", args)
	}
}

func TestACommandRefusesToRestartADaemonThatRunsContainers(t *testing.T) {
	two, none := 2, 0
	for name, c := range map[string]struct {
		data  api.BuildMismatch
		build string
		want  string
	}{
		"two containers":             {api.BuildMismatch{PID: 7001, State: api.StateRunning, RunningContainers: &two}, version.Build, "2 containers are running"},
		"containers unknown":         {api.BuildMismatch{PID: 7001, State: api.StateRunning}, version.Build, "may be running containers"},
		"starting":                   {api.BuildMismatch{PID: 7001, State: api.StateStarting}, version.Build, "may be running containers"},
		"a daemon of an older build": {api.BuildMismatch{}, version.Build, `"aaaaaaa"`},
		"a half-built fevd":          {api.BuildMismatch{PID: 7001, State: api.StateStopped, RunningContainers: &none}, "ccccccc", `the fevd beside this fev is build "ccccccc"`},
	} {
		f := newFake(t)
		started, stopped := otherBuild(t, f, c.build, c.data)
		r := f.run("start").want(t, ExitUnreachable)
		if len(*stopped) != 0 {
			t.Errorf("%s: the daemon was stopped (%v)", name, *stopped)
		}
		if args, _ := os.ReadFile(started); len(args) != 0 {
			t.Errorf("%s: a daemon was started: %q", name, args)
		}
		for _, want := range []string{`"aaaaaaa"`, `"` + version.Build + `"`, c.want} {
			if !strings.Contains(r.err, want) {
				t.Errorf("%s: stderr %q lacks %s", name, r.err, want)
			}
		}
	}
}

func TestStopStopsADaemonOfAnotherBuild(t *testing.T) {
	two := 2
	f := newFake(t)
	started, stopped := otherBuild(t, f, version.Build, api.BuildMismatch{PID: 7001, State: api.StateRunning, RunningContainers: &two})
	r := f.run("stop").want(t, 0)
	if len(*stopped) != 1 || (*stopped)[0] != 7001 {
		t.Fatalf("asked %v to stop", *stopped)
	}
	if args, _ := os.ReadFile(started); len(args) != 0 {
		t.Errorf("a daemon was started by fev stop: %q", args)
	}
	if _, err := os.Stat(api.StopMarkPath(f.home)); err != nil {
		t.Errorf("no stop mark: %v", err)
	}
	if !strings.HasPrefix(r.out, "state: stopped\n") {
		t.Errorf("stdout %q", r.out)
	}
}
