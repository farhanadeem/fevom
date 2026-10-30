package agent

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"fev/proto/jsonrpc"
)

func TestTheTypedClientHearsTheBacklogsEnd(t *testing.T) {
	server, conn := net.Pipe()
	defer server.Close()
	defer conn.Close()
	agentConn := jsonrpc.NewConn(server, jsonrpc.HandlerFunc(func(ctx context.Context, c *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
		switch method {
		case MethodHello:
			return HelloResult{APIVersion: APIVersion, AgentVersion: "dev", Kernel: "7.2.9"}, nil
		case MethodLogSubscribe:
			jsonrpc.AfterReply(ctx, func() {
				c.Notify(NotifyLogEntry, LogEntry{Subscription: 9, Source: MachineConsoleLog("dev"), Text: "old"})
				c.Notify(NotifyLogBacklogEnd, Subscription{Subscription: 9})
				c.Notify(NotifyLogEntry, LogEntry{Subscription: 9, Source: MachineConsoleLog("dev"), Text: "new"})
			})
			return Subscription{Subscription: 9}, nil
		}
		return Empty{}, nil
	}))
	defer agentConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := NewClient(ctx, conn, "test")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 8)
	stop, err := c.SubscribeLogsMarked(ctx, LogSubscribeParams{Sources: []string{MachineConsoleLog("dev")}, Backlog: true},
		func(e LogEntry) { got <- e.Text }, func() { got <- "END" })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for _, want := range []string{"old", "END", "new"} {
		select {
		case s := <-got:
			if s != want {
				t.Fatalf("got %q, want %q", s, want)
			}
		case <-ctx.Done():
			t.Fatalf("no %q", want)
		}
	}
	if MachineRuntimeLog("dev") != "machine:dev:runtime" || MachineConsoleLog("dev") != "machine:dev" {
		t.Fatal("the machine's sources are not as documented")
	}
}
