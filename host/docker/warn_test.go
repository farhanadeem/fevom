package docker

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"fev/host/daemon"
	"fev/host/docker/fakeguest"
	"fev/proto/agent"
)

func (w *world) waitWarning(key string, ok func(string) bool, what string) string {
	w.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := w.warning(key)
		if ok(got) {
			return got
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("%s: the status warning is %q", what, got)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAPortInConflictIsAWarningInTheStatusWhileItLasts(t *testing.T) {
	w := newWorld(t, Options{Autostart: true})
	w.boot()
	if got := w.warning(WarnPorts); got != "" {
		t.Fatalf("with nothing published: %q", got)
	}
	port := freePort(t)
	other, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	id, _ := w.engine.Run(fakeguest.Spec{Name: "web", Ports: []fakeguest.Port{{HostPort: port, Proto: "tcp", ContainerPort: 80}}})
	w.waitForwards(fmt.Sprintf("tcp/%d conflict", port))
	got := w.waitWarning(WarnPorts, func(s string) bool { return s != "" }, "a port in conflict")
	for _, want := range []string{fmt.Sprintf("port %d/tcp", port), "another program on this Mac", id[:12], "fev ports --retry"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning %q lacks %q", got, want)
		}
	}
	second := freePort(t)
	other2, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", second))
	if err != nil {
		t.Fatal(err)
	}
	defer other2.Close()
	w.engine.Run(fakeguest.Spec{Name: "api", Ports: []fakeguest.Port{{HostPort: second, Proto: "tcp", ContainerPort: 80}}})
	w.waitWarning(WarnPorts, func(s string) bool {
		return strings.Contains(s, fmt.Sprintf("%d/tcp", port)) && strings.Contains(s, fmt.Sprintf("%d/tcp", second)) && strings.Contains(s, "2 published ports")
	}, "two ports in conflict")
	other.Close()
	other2.Close()
	if _, err := w.m.table.Retry(""); err != nil {
		t.Fatal(err)
	}
	w.waitWarning(WarnPorts, func(s string) bool { return s == "" }, "after the retry")
}

func TestTheModuleCountsRunningContainersWhileItFollowsTheEngine(t *testing.T) {
	w := newWorld(t, Options{Autostart: true})
	var counter daemon.ContainerCounter = w.m
	if n, known := counter.RunningContainers(); known {
		t.Fatalf("before the engine is ready: %d, known", n)
	}
	w.boot()
	count := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			n, known := counter.RunningContainers()
			if known && n == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d running (known: %v), want %d", n, known, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	count(0)
	a, _ := w.engine.Run(fakeguest.Spec{Name: "a"})
	w.engine.Run(fakeguest.Spec{Name: "b"})
	count(2)
	w.engine.Die(a)
	count(1)
	w.to("stopping", "stopping", "")
	if n, known := counter.RunningContainers(); known {
		t.Fatalf("with the engine gone: %d, known", n)
	}
}

func TestATrimIsAnnouncedToWhoeverAsked(t *testing.T) {
	old := TrimDelay
	TrimDelay = 20 * time.Millisecond
	defer func() { TrimDelay = old }()
	trimmed := make(chan struct{}, 4)
	w := newWorld(t, Options{Autostart: true, Trimmed: func() { trimmed <- struct{}{} }})
	w.m.trimCall = func(context.Context, daemon.VM) (*agent.TrimResult, error) { return &agent.TrimResult{}, nil }
	w.boot()
	select {
	case <-trimmed:
		t.Fatal("announced with no trim")
	case <-time.After(5 * TrimDelay):
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.engine.Emit("image", "delete", "x", nil)
		select {
		case <-trimmed:
			return
		case <-time.After(10 * TrimDelay):
		}
		if time.Now().After(deadline) {
			t.Fatal("a trim was made and nobody was told")
		}
	}
}
