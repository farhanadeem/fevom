package agent

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"fev/proto/jsonrpc"
)

func TestSubscribingWhileAnotherStreamIsBusyDoesNotStall(t *testing.T) {
	const lines = 1000
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	jsonrpc.NewConn(a, jsonrpc.HandlerFunc(func(ctx context.Context, c *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
		switch method {
		case MethodHello:
			return HelloResult{APIVersion: APIVersion, AgentVersion: "dev", Kernel: "7.2.9"}, nil
		case MethodLogSubscribe:
			return Subscription{Subscription: 1}, nil
		case MethodEngineSubscribe:
			for i := range lines {
				c.Notify(NotifyLogEntry, LogEntry{Subscription: 1, Source: LogKernel, Level: i})
			}
			jsonrpc.AfterReply(ctx, func() {
				c.Notify(NotifyEngineState, EngineState{Subscription: 2, EngineStatus: EngineStatus{State: EngineReady}})
			})
			return Subscription{Subscription: 2}, nil
		case MethodExecStart:
			jsonrpc.AfterReply(ctx, func() { c.Notify(NotifyExecExit, ExecExit{ID: 9, ExitCode: 3}) })
			return ExecStarted{ID: 9, Token: "6f1c0e9a4b7d42c38a5e1f0b9c8d7e6f"}, nil
		}
		return Empty{}, nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := NewClient(ctx, b, "test")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan int, lines)
	if _, err := c.SubscribeLogs(ctx, LogSubscribeParams{Backlog: true}, func(e LogEntry) { got <- e.Level }); err != nil {
		t.Fatal(err)
	}
	states := make(chan EngineStatus, 1)
	if _, err := c.SubscribeEngine(ctx, func(s EngineStatus) { states <- s }); err != nil {
		t.Fatalf("subscribing with another stream busy: %v", err)
	}
	for want := range lines {
		select {
		case level := <-got:
			if level != want {
				t.Fatalf("log line %d arrived where %d was due", level, want)
			}
		case <-ctx.Done():
			t.Fatalf("log line %d never arrived", want)
		}
	}
	select {
	case s := <-states:
		if s.State != EngineReady {
			t.Fatalf("engine state arrived as %+v", s)
		}
	case <-ctx.Done():
		t.Fatal("the state sent straight after the subscription reply was dropped")
	}
	for range 50 {
		exits := make(chan ExecExit, 1)
		if _, err := c.ExecStart(ctx, ExecStartParams{Context: ExecInit, Argv: []string{"/bin/true"}}, func(e ExecExit) { exits <- e }); err != nil {
			t.Fatal(err)
		}
		select {
		case e := <-exits:
			if e.ID != 9 || e.ExitCode != 3 {
				t.Fatalf("exit arrived as %+v", e)
			}
		case <-ctx.Done():
			t.Fatal("the exit sent straight after the exec.start reply was dropped")
		}
	}
}
