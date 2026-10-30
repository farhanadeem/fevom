package cli

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func TestTheEntryPointReadsShortFlagsAsDockerDoes(t *testing.T) {
	for _, c := range []struct {
		name, argv string
		want       globals
	}{
		{"docker", "-DH unix:///x ps", globals{host: "unix:///x", rest: []string{"ps"}}},
		{"docker", "-DHunix:///x ps", globals{host: "unix:///x", rest: []string{"ps"}}},
		{"docker", "-H=unix:///x ps", globals{host: "unix:///x", rest: []string{"ps"}}},
		{"docker", "-Dc other ps", globals{context: "other", rest: []string{"ps"}}},
		{"docker", "-c=other ps", globals{context: "other", rest: []string{"ps"}}},
		{"docker", "-lH ps", globals{rest: []string{"ps"}}},
		{"docker", "-D=true -H unix:///x ps", globals{host: "unix:///x", rest: []string{"ps"}}},
		{"docker-compose", "-fa.yml -H unix:///x up", globals{host: "unix:///x", rest: []string{"up"}}},
	} {
		got := parseGlobals(c.name, strings.Fields(c.argv))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %s: %+v, want %+v", c.name, c.argv, got, c.want)
		}
	}
}

func TestTheDefaultContextIsDockerHostWhenThatIsSet(t *testing.T) {
	cfg := dockerConfig(t, "other", map[string]string{"other": "unix:///o/docker.sock"})
	t.Setenv("DOCKER_CONFIG", cfg)
	for _, c := range []struct{ argv, host, context, want string }{
		{"--context default ps", "unix:///b/run/docker.sock", "", "unix:///b/run/docker.sock"},
		{"--context=default ps", "tcp://e:1", "other", "tcp://e:1"},
		{"-c default ps", "tcp://e:1", "", "tcp://e:1"},
		{"--context default ps", "", "other", defaultHost},
		{"ps", "tcp://e:1", "default", "tcp://e:1"},
	} {
		t.Setenv("DOCKER_HOST", c.host)
		t.Setenv("DOCKER_CONTEXT", c.context)
		if got := headed(parseGlobals("docker", strings.Fields(c.argv)), "/nonexistent"); got != c.want {
			t.Errorf("docker %s with DOCKER_HOST=%q DOCKER_CONTEXT=%q: headed for %q, want %q", c.argv, c.host, c.context, got, c.want)
		}
	}
}

func TestStartOnAVMPausedForSleepIsNotAFailure(t *testing.T) {
	f := newFake(t)
	paused := func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return api.Status{Seq: 9, State: api.StatePaused, PauseReason: api.PauseSleep, Engine: api.EngineReady, BootID: "c40d19", PendingRestart: []string{}}, nil
	}
	f.on(api.MethodVMStart, paused)
	f.on(api.MethodVMRestart, paused)
	for _, cmd := range []string{"start", "restart"} {
		r := f.run(cmd)
		if r.code != ExitOK || !strings.Contains(r.out, "state: running (paused for sleep)") || r.err != "" {
			t.Errorf("fev %s on a paused VM: exit %d, out %q, err %q", cmd, r.code, r.out, r.err)
		}
	}
}

func TestLogsPrintsWhatTheDaemonWroteJustBeforeItExited(t *testing.T) {
	const lines = 200
	for round := 0; round < 5; round++ {
		f := newFake(t)
		f.on(api.MethodLogSubscribe, func(c *jsonrpc.Conn, _ json.RawMessage) (any, error) {
			go func() {
				time.Sleep(10 * time.Millisecond)
				for i := 0; i < lines; i++ {
					c.Notify(api.NotifyLogEntry, api.LogEntry{Subscription: 3, Source: api.LogDaemon, Time: "2026-10-10T08:00:00.001Z", Level: 6, Text: fmt.Sprintf("line %d", i)})
				}
				c.Close()
			}()
			return api.Subscription{Subscription: 3}, nil
		})
		r := f.run("logs").want(t, ExitNotRunning)
		if got := strings.Count(r.out, "\n"); got != lines || !strings.Contains(r.out, fmt.Sprintf("line %d\n", lines-1)) {
			t.Fatalf("round %d: %d of the %d lines the daemon wrote before it exited were printed", round, got, lines)
		}
		f.ln.Close()
	}
}
