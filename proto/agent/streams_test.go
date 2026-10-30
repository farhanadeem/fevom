package agent

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"fev/proto/jsonrpc"
)

func TestMachinesStreamRequestsAreKeptApartFromTheEngines(t *testing.T) {
	fixtures, err := Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	var engine, machine []json.RawMessage
	for _, fx := range fixtures {
		if fx.Method != NotifyStreamRequest {
			continue
		}
		var r StreamRequest
		if err := json.Unmarshal(fx.Params, &r); err != nil {
			t.Fatal(err)
		}
		if r.Machine != "" {
			machine = append(machine, fx.Params)
		} else {
			engine = append(engine, fx.Params)
		}
	}
	if len(engine) == 0 || len(machine) < 2 {
		t.Fatalf("%d engine and %d machine examples of stream.request", len(engine), len(machine))
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	server := jsonrpc.NewConn(a, jsonrpc.HandlerFunc(func(context.Context, *jsonrpc.Conn, string, json.RawMessage) (any, error) {
		return HelloResult{APIVersion: APIVersion, AgentVersion: "dev"}, nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := NewClient(ctx, b, "test")
	if err != nil {
		t.Fatal(err)
	}
	toMachines := make(chan StreamRequest, 8)
	toEngine := make(chan StreamRequest, 8)
	c.OnMachineStreamRequest(func(r StreamRequest) { toMachines <- r })
	send := func(params json.RawMessage) {
		if err := server.Notify(NotifyStreamRequest, params); err != nil {
			t.Fatal(err)
		}
	}
	send(engine[0])
	for _, p := range machine {
		send(p)
	}
	for range machine {
		select {
		case r := <-toMachines:
			if r.Machine == "" || r.UID == nil {
				t.Errorf("the machines' follower was given %+v", r)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a machine's request was not delivered")
		}
	}
	c.OnStreamRequest(func(r StreamRequest) { toEngine <- r })
	send(engine[0])
	send(machine[0])
	select {
	case r := <-toEngine:
		if r.Machine != "" {
			t.Errorf("the engine's follower was given %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the engine's request was not delivered")
	}
	select {
	case r := <-toMachines:
		if r.Machine == "" {
			t.Errorf("the machines' follower was given %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a machine's request was not delivered")
	}
	select {
	case r := <-toEngine:
		t.Errorf("the engine's follower was also given %+v", r)
	case r := <-toMachines:
		t.Errorf("the machines' follower was also given %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
}
