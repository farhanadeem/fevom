package cli

import (
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func TestPortsListsForwardsWithConflictsAndReach(t *testing.T) {
	f := newFake(t)
	r := f.run("ports").want(t, 0)
	golden(t, "ports", r.out)
	sameJSON(t, f.run("ports", "--format", "json").want(t, 0).out, fixtureResult(t, "forwards.list"))

	conflict := api.Forwards{Forwards: []api.Forward{
		{ID: "docker/3f2a9c1d7e10/tcp/8080", Source: api.ForwardDocker, Owner: "3f2a9c1d7e10b6a4c2d8e9f0a1b2c3d4", Proto: "tcp",
			Mac: []string{"0.0.0.0:8080", "[::]:8080"}, Guest: "192.168.215.2:8080", State: api.ForwardConflict,
			Error: "0.0.0.0:8080 is in use by another program on this Mac"},
		{ID: "docker/aa11bb22cc33/tcp/5432", Source: api.ForwardDocker, Owner: "aa11bb22cc33", Proto: "tcp",
			Mac: []string{"127.0.0.1:5432", "[::1]:5432"}, Guest: "127.0.0.1:5432", State: api.ForwardActive},
	}}
	f.on(api.MethodForwardsList, func(*jsonrpc.Conn, json.RawMessage) (any, error) { return conflict, nil })
	r = f.run("ports").want(t, ExitFailed)
	golden(t, "ports.conflict", r.out)
	if r.err != "fev: 1 of 2 ports are not served\n" {
		t.Errorf("stderr: %q", r.err)
	}
	r = f.run("ports", "--format", "json").want(t, ExitFailed)
	if r.err != "" {
		t.Errorf("stderr with --format json: %q", r.err)
	}

	f.on(api.MethodForwardsList, func(*jsonrpc.Conn, json.RawMessage) (any, error) { return api.Forwards{}, nil })
	if out := f.run("ports", "--format", "json").want(t, 0).out; compact([]byte(out)) != `{"forwards":[]}` {
		t.Errorf("no forwards: %s", out)
	}

	retry := fixtureFile(t, "forwards.retry")
	f2 := newFake(t)
	f2.fixture(api.MethodForwardsRetry, "forwards.retry")
	r = f2.run("ports", "--retry", "docker/3f2a9c1d7e10/tcp/8080").want(t, 0)
	if got := f2.params(api.MethodForwardsRetry); got != compact(retry.Params) {
		t.Errorf("forwards.retry params: %s, want %s", got, compact(retry.Params))
	}
	golden(t, "ports.retry", r.out)
	f3 := newFake(t)
	f3.fixture(api.MethodForwardsRetry, "forwards.retry")
	f3.run("ports", "--retry").want(t, 0)
	if got := f3.params(api.MethodForwardsRetry); got != `{}` {
		t.Errorf("forwards.retry params for all: %s", got)
	}
	if f3.called(api.MethodForwardsList) {
		t.Error("ports --retry also listed: one call is enough")
	}
	f3.on(api.MethodForwardsRetry, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return nil, jsonrpc.Errorf(api.CodeNotImplemented, "forwards.retry is not implemented in this build")
	})
	if r := f3.run("ports", "--retry").want(t, ExitFailed); r.err != "fev: forwards.retry is not implemented in this build\n" {
		t.Errorf("stderr: %q", r.err)
	}
}

func TestLogsTailsTheUnifiedLog(t *testing.T) {
	f := newFake(t)
	entries := []api.LogEntry{
		{Subscription: 3, Source: api.LogDaemon, Time: "2026-10-10T08:00:00.001Z", Level: 6, Text: "ready"},
		{Subscription: 3, Source: api.LogKernel, Time: "2026-10-10T08:00:00.038Z", Level: 6, Text: "Run /init as init process", Dropped: 14},
	}
	f.on(api.MethodLogSubscribe, func(c *jsonrpc.Conn, _ json.RawMessage) (any, error) {
		go func() {
			time.Sleep(20 * time.Millisecond)
			for _, e := range entries {
				c.Notify(api.NotifyLogEntry, e)
			}
			time.Sleep(50 * time.Millisecond)
			c.Close()
		}()
		return api.Subscription{Subscription: 3}, nil
	})
	r := f.run("logs", "-n", "5", "--source", "daemon", "--source", "kernel").want(t, ExitNotRunning)
	golden(t, "logs", r.out)
	if got := f.params(api.MethodLogSubscribe); got != `{"sources":["daemon","kernel"],"backlog":5}` {
		t.Errorf("log.subscribe params: %s", got)
	}
	if !strings.Contains(r.err, "14 lines of kernel were dropped") || !strings.Contains(r.err, "fev: the daemon has stopped") {
		t.Errorf("stderr: %q", r.err)
	}
	f2 := newFake(t)
	f2.on(api.MethodLogSubscribe, f.reply(api.MethodLogSubscribe))
	r = f2.run("logs", "--format", "json")
	lines := strings.Split(strings.TrimSpace(r.out), "\n")
	if len(lines) != 2 {
		t.Fatalf("JSON lines: %q", r.out)
	}
	for i, line := range lines {
		if err := api.Conforms([]byte(line), api.LogEntry{}); err != nil {
			t.Errorf("line %d is not a LogEntry: %v", i, err)
		}
	}
	if got := f2.params(api.MethodLogSubscribe); got != `{"backlog":20}` {
		t.Errorf("default log.subscribe params: %s", got)
	}
}

func TestCaffeinateHoldsWhileTheCommandRunsAndPassesItsStatus(t *testing.T) {
	f := newFake(t)
	var held, released atomic.Int64
	f.on(api.MethodCaffeinateHold, func(c *jsonrpc.Conn, _ json.RawMessage) (any, error) {
		held.Store(time.Now().UnixNano())
		go func() {
			<-c.Done()
			released.Store(time.Now().UnixNano())
		}()
		return api.Caffeinated{Holders: 1, Display: true}, nil
	})
	ran := filepath.Join(t.TempDir(), "ran")
	r := f.run("caffeinate", "-d", "--", "sh", "-c", "sleep 0.3; date +%s >"+ran+"; exit 7")
	if r.code != 7 || r.err != "" {
		t.Fatalf("exit %d, stderr %q; want the command's own status and nothing said", r.code, r.err)
	}
	if got := f.params(api.MethodCaffeinateHold); !strings.HasPrefix(got, `{"reason":"sh -c sleep 0.3;`) || !strings.HasSuffix(got, `"display":true}`) {
		t.Errorf("caffeinate.hold params: %s", got)
	}
	if _, err := os.Stat(ran); err != nil {
		t.Fatal("the command did not run")
	}
	deadline := time.Now().Add(2 * time.Second)
	for released.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if d := time.Duration(released.Load() - held.Load()); d < 300*time.Millisecond {
		t.Errorf("the hold lasted %v; the command ran for 300 ms", d)
	}
	if r := f.run("caffeinate", "sh", "-c", "exit 3"); r.code != 3 {
		t.Errorf("caffeinate sh -c: exit %d", r.code)
	}
	if r := f.run("caffeinate", "sh", "-c", "kill -TERM $$"); r.code != 128+15 {
		t.Errorf("a command killed by SIGTERM: exit %d", r.code)
	}
	if r := f.run("caffeinate", "/nonexistent/program"); r.code != 127 || !strings.Contains(r.err, "cannot run") {
		t.Errorf("a command that is not there: exit %d %q", r.code, r.err)
	}
	f.on(api.MethodCaffeinateHold, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return nil, jsonrpc.Errorf(api.CodeNotImplemented, "caffeinate.hold is not implemented in this build")
	})
	os.Remove(ran)
	f.run("caffeinate", "sh", "-c", "date >"+ran).want(t, ExitFailed)
	if _, err := os.Stat(ran); err == nil {
		t.Error("the command ran although the Mac was not held awake")
	}
}

func TestAnInterruptSentToFevomAloneReachesTheCommand(t *testing.T) {
	if foreground() {
		t.Skip("run from a terminal's foreground: an interrupt here is the terminal's to deliver")
	}
	if signal.Ignored(syscall.SIGINT) {
		t.Skip("this process was started with SIGINT ignored")
	}
	f := newFake(t)
	f.on(api.MethodCaffeinateHold, func(*jsonrpc.Conn, json.RawMessage) (any, error) { return api.Caffeinated{Holders: 1}, nil })
	ready := filepath.Join(t.TempDir(), "ready")
	done := make(chan result, 1)
	go func() {
		done <- f.run("caffeinate", "sh", "-c", "trap 'kill $!; exit 42' INT; echo >"+ready+"; sleep 5 & wait")
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	syscall.Kill(os.Getpid(), syscall.SIGINT)
	select {
	case r := <-done:
		if r.code != 42 {
			t.Errorf("exit %d, want the command's own answer to the interrupt, 42; stderr %q", r.code, r.err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("the command was not interrupted: it ran its whole 5 s")
	}
}
