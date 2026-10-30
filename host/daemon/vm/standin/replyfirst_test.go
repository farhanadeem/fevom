package standin

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
	"fev/proto/version"
)

func TestASubscriptionsItemsComeAfterItsReply(t *testing.T) {
	a := NewAgent(version.Build, func() float64 { return 10 })
	a.Boot(1)
	var mu sync.Mutex
	known := map[uint64]bool{}
	early := 0
	conn := jsonrpc.NewConn(connect(t, a), jsonrpc.HandlerFunc(func(_ context.Context, _ *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
		var p struct {
			Subscription uint64 `json:"subscription"`
		}
		json.Unmarshal(params, &p)
		mu.Lock()
		if p.Subscription != 0 && !known[p.Subscription] {
			early++
		}
		mu.Unlock()
		return nil, nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var hello api.HelloResult
	if err := conn.Call(ctx, api.MethodHello, api.HelloParams{APIVersion: api.APIVersion, Build: version.Build, Client: "test"}, &hello); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		for _, call := range []struct {
			method string
			params any
		}{
			{api.MethodLogSubscribe, api.LogSubscribeParams{Backlog: true}},
			{api.MethodEngineSubscribe, nil},
		} {
			var sub api.Subscription
			if err := conn.CallThen(ctx, call.method, call.params, &sub, func() {
				mu.Lock()
				known[sub.Subscription] = true
				mu.Unlock()
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if early != 0 {
		t.Errorf("%d items came before the reply that carried their subscription's id", early)
	}
}
