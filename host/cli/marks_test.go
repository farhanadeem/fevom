package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fev/host/daemon/launch"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func TestADockerCommandIsToldItWaitsForAPausedVM(t *testing.T) {
	c := newEntryCase(t)
	c.listen()
	c.run("docker", "ps")
	if c.execed == nil || strings.Contains(c.stderr.String(), "paused") {
		t.Fatalf("with nothing paused: became %v, said %q", c.execed, c.stderr.String())
	}
	if err := os.WriteFile(api.PauseMarkPath(c.home), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c.run("docker", "ps")
	if c.execed == nil || c.execed[0] != filepath.Join(c.tools, "docker") {
		t.Fatalf("with the VM paused docker was not run: %v", c.execed)
	}
	if !strings.HasPrefix(c.stderr.String(), api.PausedLine+"\n") || strings.Count(c.stderr.String(), "paused") != 1 {
		t.Errorf("said %q, want the one line first", c.stderr.String())
	}
	c.run("docker", "--context", "other", "ps")
	if strings.Contains(c.stderr.String(), "paused") {
		t.Errorf("a command for another context was told %q", c.stderr.String())
	}
}

func TestAStopAskedForWhileTheEntryPointStartedFevomStands(t *testing.T) {
	c := newEntryCase(t)
	c.run("docker", "ps")
	if started := c.wasStarted(); !strings.Contains(started, "--on-demand") {
		t.Errorf("fevd was started with %q; want --on-demand", started)
	}
	old := ensure
	t.Cleanup(func() { ensure = old })
	ensure = func(launch.Options) error {
		return os.WriteFile(api.StopMarkPath(c.home), nil, 0o644)
	}
	if code := c.run("docker", "ps"); code != 1 || c.execed != nil || c.stderr.String() != api.StoppedLine+"\n" {
		t.Errorf("exit %d, became %v, said %q", code, c.execed, c.stderr.String())
	}
	os.Remove(api.StopMarkPath(c.home))
	ensure = func(launch.Options) error {
		os.WriteFile(api.StopMarkPath(c.home), nil, 0o644)
		return errors.New("the daemon reported ready but its socket does not accept connections")
	}
	if code := c.run("docker", "ps"); code != 1 || c.execed != nil || c.stderr.String() != api.StoppedLine+"\n" {
		t.Errorf("with the daemon gone: exit %d, became %v, said %q", code, c.execed, c.stderr.String())
	}
}

func TestStopLeavesTheMarkBeforeItAsksAnybody(t *testing.T) {
	f := newFake(t)
	marked := false
	f.on(api.MethodVMStop, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		_, err := os.Stat(api.StopMarkPath(f.home))
		marked = err == nil
		return api.Status{Seq: 3, State: api.StateStopped, StopRequested: true, Engine: api.EngineStopped, PendingRestart: []string{}}, nil
	})
	f.on(api.MethodShutdown, func(c *jsonrpc.Conn, _ json.RawMessage) (any, error) {
		go func() { c.Close() }()
		return api.Empty{}, nil
	})
	f.run("stop").want(t, 0)
	if !marked {
		t.Error("the mark was not there when the daemon was asked to stop")
	}
}

func TestRestartDockerOnAPausedVMSaysItResumesIt(t *testing.T) {
	f := newFake(t)
	f.fixture(api.MethodStatus, "daemon.status.paused")
	if err := os.WriteFile(api.PauseMarkPath(f.home), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := f.run("restart", "docker").want(t, 0)
	if !strings.Contains(r.err, "paused for the Mac's sleep; resuming") {
		t.Errorf("stderr %q", r.err)
	}
	os.Remove(api.PauseMarkPath(f.home))
	if r := f.run("restart", "docker").want(t, 0); r.err != "" {
		t.Errorf("with nothing paused: stderr %q", r.err)
	}
}

func TestLogsFilesReadsTheFilesAndStartsNothing(t *testing.T) {
	home := scratchHome(t)
	started := noDaemon(t)
	dir := filepath.Join(home, "log")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "fevd.log.1"), []byte("2026-10-10T08:00:00.000100Z daemon 6 fevd is ready\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "fevd.log"), []byte("2026-10-10T08:00:02.000000Z daemon 4 the VM failed and will be started again\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "guest.log"), []byte("2026-10-10T08:00:01.000000Z kernel 6 Run /init as init process\n"), 0o600)
	r := fev(t, home, "", "logs", "--files").want(t, 0)
	want := "2026-10-10T08:00:00.000100Z daemon  fevd is ready\n" +
		"2026-10-10T08:00:01.000000Z kernel  Run /init as init process\n" +
		"2026-10-10T08:00:02.000000Z daemon  the VM failed and will be started again\n"
	if r.out != want || r.err != "" {
		t.Errorf("stdout %q\nstderr %q", r.out, r.err)
	}
	r = fev(t, home, "", "logs", "--files", "--source", "kernel", "--format", "json").want(t, 0)
	var e api.LogEntry
	if err := json.Unmarshal([]byte(r.out), &e); err != nil || e.Source != api.LogKernel || e.Text != "Run /init as init process" || e.Level != 6 || e.Time != "2026-10-10T08:00:01.000000Z" {
		t.Errorf("as JSON: %q (%v)", r.out, err)
	}
	if fev(t, home, "", "logs", "--files", "--source", "nonsense").code != ExitUsage {
		t.Error("a source that does not exist was accepted")
	}
	if data, _ := os.ReadFile(started); len(data) != 0 {
		t.Errorf("a daemon was started: %q", data)
	}
}

func TestAFailedRunThatNamesAMacPathIsToldFileSharingIsOff(t *testing.T) {
	c := newEntryCase(t)
	f := newFakeIn(t, c.home)
	f.on(api.MethodForwardsList, func(*jsonrpc.Conn, json.RawMessage) (any, error) { return api.Forwards{Forwards: []api.Forward{}}, nil })
	share := false
	f.on(api.MethodFilesStatus, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return api.FilesStatus{Share: share, Watch: api.FilesWatch{Paths: []string{}}}, nil
	})
	status := filepath.Join(t.TempDir(), "status")
	os.WriteFile(status, []byte("125"), 0o644)
	os.WriteFile(filepath.Join(c.tools, "docker"), []byte("#!/bin/sh\necho 'docker: Error response from daemon: error while creating mount source path' >&2\nexit $(cat "+status+")\n"), 0o755)
	run := func(argv ...string) (int, string) {
		c.stderr.Reset()
		code := Entry(Env{Name: "docker", Args: argv, Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &c.stderr})
		return code, c.stderr.String()
	}
	const line = "fev: file sharing is off (files.share is false)"
	for _, argv := range [][]string{
		{"run", "--rm", "-v", "/Users/me/src:/app", "alpine", "ls"},
		{"run", "--volume=/Users/me/src:/app:ro", "alpine"},
		{"run", "--mount", "type=bind,source=/Users/me/src,target=/app", "alpine"},
		{"run", "--mount=type=bind,src=/tmp/x,dst=/app", "alpine"},
		{"container", "run", "-v", "./src:/app", "alpine"},
		{"run", "-v", "~/src:/app", "alpine"},
	} {
		code, said := run(argv...)
		if code != 125 || !strings.Contains(said, "docker: Error response") || strings.Count(said, line) != 1 || !strings.Contains(said, "fev config set files.share true") {
			t.Errorf("docker %s: exit %d, stderr %q", strings.Join(argv, " "), code, said)
		}
	}
	for _, argv := range [][]string{
		{"run", "-v", "data:/data", "alpine"},
		{"run", "alpine", "ls", "-v", "/Users"},
		{"run", "--mount", "type=volume,source=data,target=/data", "alpine"},
		{"run", "alpine"},
	} {
		if _, said := run(argv...); strings.Contains(said, line) {
			t.Errorf("docker %s was told file sharing is off: %q", strings.Join(argv, " "), said)
		}
	}
	share = true
	if _, said := run("run", "-v", "/Users/me/src:/app", "alpine"); strings.Contains(said, line) {
		t.Errorf("with sharing on: %q", said)
	}
	share = false
	os.WriteFile(status, []byte("0"), 0o644)
	asked := len(f.made())
	if code, said := run("run", "-v", "/Users/me/src:/app", "alpine"); code != 0 || strings.Contains(said, line) {
		t.Errorf("a run that worked: exit %d, %q", code, said)
	}
	for _, made := range f.made()[asked:] {
		if made.Method == api.MethodFilesStatus {
			t.Error("the daemon was asked about file sharing after a run that worked")
		}
	}
}

func TestStatusNotesPortsThatAnswerTheLAN(t *testing.T) {
	f := newFake(t)
	running := api.Status{Seq: 9, State: api.StateRunning, Engine: api.EngineReady, BootID: "c40d19", PendingRestart: []string{}}
	f.on(api.MethodStatus, func(*jsonrpc.Conn, json.RawMessage) (any, error) { return running, nil })
	rows := []api.Forward{
		{ID: "a", Source: api.ForwardDocker, Proto: "tcp", Mac: []string{"0.0.0.0:8080", "[::]:8080"}, State: api.ForwardActive},
		{ID: "b", Source: api.ForwardDocker, Proto: "udp", Mac: []string{"0.0.0.0:5353", "[::]:5353"}, State: api.ForwardActive},
		{ID: "c", Source: api.ForwardDocker, Proto: "tcp", Mac: []string{"127.0.0.1:9000", "[::1]:9000"}, State: api.ForwardActive},
		{ID: "d", Source: api.ForwardDocker, Proto: "tcp", Mac: []string{"0.0.0.0:7000", "[::]:7000"}, State: api.ForwardConflict},
	}
	f.on(api.MethodForwardsList, func(*jsonrpc.Conn, json.RawMessage) (any, error) { return api.Forwards{Forwards: rows}, nil })
	r := f.run("status").want(t, 0)
	if !strings.Contains(r.out, "note: 2 published ports answer other devices on your network (8080/tcp, 5353/udp); \"fev config set network.publish_lan false\" keeps them to this Mac\n") {
		t.Errorf("stdout:\n%s", r.out)
	}
	if out := f.run("status", "--format", "json").want(t, 0).out; strings.Contains(out, "note") || strings.Contains(out, "8080") {
		t.Errorf("the JSON status carries the note: %s", out)
	}
	if out := f.run("status", "-q").want(t, 0).out; out != "running\n" {
		t.Errorf("status -q: %q", out)
	}
	rows = rows[2:]
	if out := f.run("status").want(t, 0).out; strings.Contains(out, "note:") {
		t.Errorf("with nothing served to the LAN: %s", out)
	}
	f.on(api.MethodForwardsList, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return nil, jsonrpc.Errorf(api.CodeFailed, "forwards.list is not available")
	})
	if r := f.run("status").want(t, 0); strings.Contains(r.out, "note:") || r.err != "" {
		t.Errorf("with no forward table: %q %q", r.out, r.err)
	}
}
