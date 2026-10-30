package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func bufApp() (*app, *bytes.Buffer, *bytes.Buffer) {
	var out, errw bytes.Buffer
	return &app{Env: Env{Name: "fev", Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errw}}, &out, &errw
}

func TestDebugWithNoTargetSaysHowToChoose(t *testing.T) {
	f := newFake(t)
	r := f.run("debug").want(t, ExitUsage)
	if !strings.Contains(r.err, "name a container or image") || !strings.Contains(r.err, "docker ps") {
		t.Errorf("stderr: %s", r.err)
	}
	for _, c := range f.made() {
		if c.Method == api.MethodDebugStart {
			t.Error("debug.start was called with no target")
		}
	}
}

func TestDebugResetDeletesTheStore(t *testing.T) {
	f := newFake(t)
	r := f.run("debug", "--reset").want(t, 0)
	if p := f.params(api.MethodDebugReset); !strings.Contains(p, "force") && p != "{}" {
		t.Errorf("debug.reset params: %s", p)
	}
	if !strings.Contains(r.out, "gone") || !strings.Contains(r.out, "freed") {
		t.Errorf("stdout: %s", r.out)
	}
}

func TestDebugResetWhileBusyIsRefused(t *testing.T) {
	f := newFake(t)
	f.on(api.MethodDebugReset, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return nil, jsonrpc.Errorf(api.CodeBusy, "1 debug session(s) are open; close them or reset --force")
	})
	r := f.run("debug", "--reset").want(t, ExitFailed)
	if !strings.Contains(r.err, "session") {
		t.Errorf("stderr: %s", r.err)
	}
}

func TestDebugStartFailureIsSurfaced(t *testing.T) {
	f := newFake(t)
	f.on(api.MethodDebugStart, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return nil, jsonrpc.Errorf(api.CodeFailed, `no container or image "nope" here (docker pull nope first)`)
	})
	r := f.run("debug", "-f", "nope").want(t, ExitFailed)
	if !strings.Contains(r.err, "no container or image") {
		t.Errorf("stderr: %s", r.err)
	}
	if strings.Contains(r.err, "docker exec") || strings.Contains(r.err, "userland") {
		t.Errorf("fell back for a missing target: %s", r.err)
	}
}

func TestDebugRemoteContextIsWired(t *testing.T) {
	f := newFake(t)
	r := f.run("debug", "-c", "builder", "web").want(t, ExitFailed)
	if strings.Contains(r.err, "not built in this binary") {
		t.Errorf("-c is the stub: %s", r.err)
	}
	if !strings.Contains(r.err, "docker CLI") {
		t.Errorf("stderr: %s", r.err)
	}
	if len(f.made()) != 0 {
		t.Errorf("the local daemon was called for -c: %v", f.made())
	}
}

func TestDebugBannerNamesTheTarget(t *testing.T) {
	a, _, errw := bufApp()
	a.debugBanner(&api.DebugStarted{Name: "web", Image: "nginx:1.29", Kind: "running",
		Notes: []string{"tools in /nix; galley install NAME adds a package", "a note"}})
	s := errw.String()
	if !strings.Contains(s, "fev debug: web (nginx:1.29), running") || !strings.Contains(s, "tools in /nix") {
		t.Errorf("banner: %q", s)
	}
	if !strings.Contains(s, "fev debug: a note") {
		t.Errorf("the second note is not on its own line: %q", s)
	}
}

func TestDebugBannerIsQuietWithQ(t *testing.T) {
	a, _, errw := bufApp()
	a.quiet = true
	a.debugBanner(&api.DebugStarted{Name: "web", Kind: "running", Notes: []string{"x"}})
	if errw.String() != "" {
		t.Errorf("a quiet banner printed %q", errw.String())
	}
}

func TestDebugExitFallsBackOnUnavailableTools(t *testing.T) {
	a, _, errw := bufApp()
	ran := false
	fallback := func() (int, error) { ran = true; return 7, nil }
	e := api.SessionExit{Reason: api.DebugReasonToolsUnavailable, ExitCode: -1}
	code, err := a.debugExit(context.Background(), debugFlags{fallback: true}, e, fallback)
	if err != nil || code != 7 || !ran {
		t.Errorf("with -f: code %d, ran %v, err %v", code, ran, err)
	}
	if !strings.Contains(errw.String(), "container's own userland") {
		t.Errorf("no line said why it fell back: %q", errw.String())
	}
	_, err = a.debugExit(context.Background(), debugFlags{fallback: false}, e, fallback)
	if err == nil || !strings.Contains(err.Error(), "-f") {
		t.Errorf("without -f, want a failure naming -f: %v", err)
	}
}

func TestDebugExitCarriesTheCommandStatus(t *testing.T) {
	a, _, _ := bufApp()
	code, err := a.debugExit(context.Background(), debugFlags{}, api.SessionExit{ExitCode: 3}, nil)
	if err != nil || code != 3 {
		t.Errorf("a plain exit: code %d, err %v", code, err)
	}
	_, err = a.debugExit(context.Background(), debugFlags{}, api.SessionExit{Reason: api.DebugReasonTargetGone, ExitCode: -1}, nil)
	if err == nil || !strings.Contains(err.Error(), "ended") {
		t.Errorf("target gone: %v", err)
	}
}

func TestDebugParamsCarryTheFlags(t *testing.T) {
	a, _, _ := bufApp()
	t.Setenv("LANG", "en_US.UTF-8")
	p := a.debugParams(debugFlags{workdir: "/etc", readOnly: true}, "web", []string{"ls", "-l"})
	if p.Target != "web" || p.Dir != "/etc" || !p.ReadOnly || len(p.Argv) != 2 || p.Argv[0] != "ls" {
		t.Errorf("params: %+v", p)
	}
	found := false
	for _, e := range p.Env {
		if e == "LANG=en_US.UTF-8" {
			found = true
		}
	}
	if !found {
		t.Errorf("LANG was not passed: %v", p.Env)
	}
}

func TestDebugFallbackIsByCode(t *testing.T) {
	if !isToolStoreError(jsonrpc.Errorf(api.CodeDebugToolsUnavailable, "offline")) {
		t.Error("CodeDebugToolsUnavailable is not a tool-store error")
	}
	if isToolStoreError(jsonrpc.Errorf(api.CodeFailed, "the debug tools could not be prepared")) {
		t.Error("a plain failure whose words say so was taken for one")
	}
}

func TestDebugFallbackKeepsTheWorkdir(t *testing.T) {
	got := strings.Join(fallbackArgs(debugFlags{workdir: "/srv"}, "web", []string{"ls"}, false), " ")
	if got != "exec -i -w /srv web ls" {
		t.Errorf("docker %s", got)
	}
	if got := strings.Join(fallbackArgs(debugFlags{}, "web", nil, true), " "); got != "exec -it web sh" {
		t.Errorf("docker %s", got)
	}
}

func TestDebugExitPrintsTheNote(t *testing.T) {
	a, _, errw := bufApp()
	a.debugExit(context.Background(), debugFlags{}, api.SessionExit{ExitCode: 0, StreamEnded: true,
		Note: "db was started during the session; the session's changes were not committed and are kept in the VM at /data/@debug/kept/db"}, nil)
	if !strings.Contains(errw.String(), "fev debug: db was started during the session") {
		t.Errorf("stderr: %s", errw.String())
	}
}
