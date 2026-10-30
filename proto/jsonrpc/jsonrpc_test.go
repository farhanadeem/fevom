package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func pair(t *testing.T, server Handler, client Handler) (*Conn, *Conn) {
	t.Helper()
	a, b := net.Pipe()
	s, c := NewConn(a, server), NewConn(b, client)
	t.Cleanup(func() { s.Close(); c.Close() })
	return s, c
}

type addParams struct {
	A, B int
}

var echo = HandlerFunc(func(ctx context.Context, c *Conn, method string, params json.RawMessage) (any, error) {
	switch method {
	case "add":
		p, err := Bind[addParams](params)
		if err != nil {
			return nil, err
		}
		return p.A + p.B, nil
	case "fail":
		return nil, errors.New("boom")
	case "coded":
		return nil, Errorf(1234, "specific")
	case "panic":
		panic("oh no")
	case "wait":
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, Errorf(CodeMethodNotFound, "method %q not found", method)
})

func TestCallAndErrors(t *testing.T) {
	_, c := pair(t, echo, nil)
	ctx := context.Background()

	var sum int
	if err := c.Call(ctx, "add", addParams{2, 40}, &sum); err != nil || sum != 42 {
		t.Fatalf("add = %d, %v", sum, err)
	}
	if err := c.Call(ctx, "add", nil, &sum); err != nil || sum != 0 {
		t.Fatalf("add with no params = %d, %v", sum, err)
	}

	cases := []struct {
		method string
		params any
		code   int
	}{
		{"fail", nil, CodeInternalError},
		{"coded", nil, 1234},
		{"panic", nil, CodeInternalError},
		{"nope", nil, CodeMethodNotFound},
		{"add", "not an object", CodeInvalidParams},
	}
	for _, tc := range cases {
		err := c.Call(ctx, tc.method, tc.params, nil)
		var rpcErr *Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != tc.code {
			t.Errorf("%s: got %v, want code %d", tc.method, err, tc.code)
		}
	}
	if err := c.Call(ctx, "add", addParams{1, 1}, &sum); err != nil || sum != 2 {
		t.Fatalf("after panic: %d, %v", sum, err)
	}
}

func TestConcurrentCallsAreCorrelated(t *testing.T) {
	_, c := pair(t, echo, nil)
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var sum int
			if err := c.Call(context.Background(), "add", addParams{i, i}, &sum); err != nil || sum != 2*i {
				t.Errorf("add(%d): %d, %v", i, sum, err)
			}
		}()
	}
	wg.Wait()
}

func TestNotificationsFlowBothWays(t *testing.T) {
	got := make(chan string, 4)
	record := HandlerFunc(func(ctx context.Context, c *Conn, method string, params json.RawMessage) (any, error) {
		got <- method + string(params)
		return nil, nil
	})
	s, c := pair(t, record, record)
	if err := c.Notify("to.server", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Notify("to.client", "x"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{<-got: true, <-got: true}
	if !seen["to.server1"] || !seen[`to.client"x"`] {
		t.Fatalf("got %v", seen)
	}
}

func TestCallsFailWhenTheConnectionEnds(t *testing.T) {
	s, c := pair(t, echo, nil)
	errc := make(chan error, 1)
	go func() { errc <- c.Call(context.Background(), "wait", nil, nil) }()
	time.Sleep(10 * time.Millisecond)
	s.Close()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("call succeeded on a closed connection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call did not return after close")
	}
	<-c.Done()
	if err := c.Call(context.Background(), "add", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("call after close: %v", err)
	}
	if err := c.Notify("x", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("notify after close: %v", err)
	}
}

func TestCallHonoursContext(t *testing.T) {
	_, c := pair(t, echo, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "wait", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestWireFormatAndMalformedInput(t *testing.T) {
	a, b := net.Pipe()
	s := NewConn(a, echo)
	defer s.Close()
	defer b.Close()
	r := bufio.NewReader(b)
	send := func(line string) map[string]any {
		t.Helper()
		b.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := b.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
		reply, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("after %q: %v", line, err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(reply), &m); err != nil {
			t.Fatalf("reply %q: %v", reply, err)
		}
		return m
	}

	m := send(`{"jsonrpc":"2.0","id":7,"method":"add","params":{"A":1,"B":2}}`)
	if m["jsonrpc"] != "2.0" || m["id"] != float64(7) || m["result"] != float64(3) {
		t.Fatalf("reply %v", m)
	}
	if m := send(`{"jsonrpc":"2.0","id":"abc","method":"add"}`); m["id"] != "abc" {
		t.Fatalf("reply %v", m)
	}
	for _, bad := range []string{`not json`, `{"jsonrpc":"1.0","id":1,"method":"add"}`, `[1,2,3]`} {
		m := send(bad)
		e, _ := m["error"].(map[string]any)
		if e == nil || e["code"] != float64(CodeParseError) || m["id"] != nil {
			t.Fatalf("%q: reply %v", bad, m)
		}
	}
	if m := send(`{"jsonrpc":"2.0","id":8,"method":"add"}`); m["result"] != float64(0) {
		t.Fatalf("reply %v", m)
	}
}

func TestOversizedMessageEndsTheConnection(t *testing.T) {
	a, b := net.Pipe()
	s := NewConn(a, echo)
	defer b.Close()
	go func() {
		chunk := []byte(strings.Repeat("x", 1<<16))
		for range MaxMessage/len(chunk) + 2 {
			if _, err := b.Write(chunk); err != nil {
				return
			}
		}
	}()
	select {
	case <-s.Done():
		if s.Err() == nil {
			t.Fatal("no error recorded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("connection kept buffering")
	}
}

func TestNotificationsArriveInOrder(t *testing.T) {
	const n = 2000
	got := make(chan int, n)
	record := HandlerFunc(func(ctx context.Context, c *Conn, method string, params json.RawMessage) (any, error) {
		var i int
		json.Unmarshal(params, &i)
		got <- i
		return nil, nil
	})
	s, _ := pair(t, nil, record)
	go func() {
		for i := range n {
			s.Notify("item", i)
		}
	}()
	for want := range n {
		select {
		case i := <-got:
			if i != want {
				t.Fatalf("notification %d arrived where %d was expected", i, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("stalled at %d", want)
		}
	}
}

func TestAfterReplyRunsOnceTheReplyIsWritten(t *testing.T) {
	var s *Conn
	stream := HandlerFunc(func(ctx context.Context, c *Conn, method string, params json.RawMessage) (any, error) {
		AfterReply(ctx, func() { c.Notify("item", "first") })
		return "subscribed", nil
	})
	events := make(chan string, 2)
	client := HandlerFunc(func(ctx context.Context, c *Conn, method string, params json.RawMessage) (any, error) {
		events <- "notification"
		return nil, nil
	})
	for range 200 {
		a, b := net.Pipe()
		s = NewConn(a, stream)
		c := NewConn(b, client)
		var r string
		if err := c.Call(context.Background(), "subscribe", nil, &r); err != nil {
			t.Fatal(err)
		}
		select {
		case <-events:
		case <-time.After(5 * time.Second):
			t.Fatal("no notification")
		}
		s.Close()
		c.Close()
	}
}

func TestCallThenRunsBeforeWhatFollowsTheReplyIsHandled(t *testing.T) {
	const ahead = 1000
	server := HandlerFunc(func(ctx context.Context, c *Conn, method string, params json.RawMessage) (any, error) {
		switch method {
		case "subscribe":
			for range ahead {
				c.Notify("item", 0)
			}
			AfterReply(ctx, func() { c.Notify("item", 7) })
			return 7, nil
		case "refuse":
			return nil, Errorf(1234, "no")
		case "wrong":
			return "not a number", nil
		}
		return nil, Errorf(CodeMethodNotFound, "method %q not found", method)
	})
	var mu sync.Mutex
	known := map[int]bool{}
	got := make(chan bool, 1)
	client := HandlerFunc(func(ctx context.Context, c *Conn, method string, params json.RawMessage) (any, error) {
		id, _ := Bind[int](params)
		if id == 7 {
			mu.Lock()
			got <- known[id]
			mu.Unlock()
		}
		return nil, nil
	})
	_, c := pair(t, server, client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var id int
	err := c.CallThen(ctx, "subscribe", nil, &id, func() {
		mu.Lock()
		known[id] = true
		mu.Unlock()
	})
	if err != nil || id != 7 {
		t.Fatalf("subscribe = %d, %v", id, err)
	}
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("the item behind the reply was handled before the subscription was recorded")
		}
	case <-ctx.Done():
		t.Fatal("the item behind the reply never arrived")
	}

	ran := false
	var rpcErr *Error
	if err := c.CallThen(ctx, "refuse", nil, &id, func() { ran = true }); !errors.As(err, &rpcErr) || rpcErr.Code != 1234 {
		t.Errorf("refuse: %v", err)
	}
	if err := c.CallThen(ctx, "wrong", nil, &id, func() { ran = true }); err == nil {
		t.Error("a result of the wrong type was accepted")
	}
	if ran {
		t.Error("then ran for a call that failed")
	}
}

func TestCallThenGivenUpIsAllOrNothing(t *testing.T) {
	release := make(chan struct{})
	server := HandlerFunc(func(ctx context.Context, c *Conn, method string, params json.RawMessage) (any, error) {
		<-release
		return 1, nil
	})
	_, c := pair(t, server, nil)
	for range 200 {
		ctx, cancel := context.WithCancel(context.Background())
		var ran atomic.Bool
		var id int
		errc := make(chan error, 1)
		go func() { errc <- c.CallThen(ctx, "subscribe", nil, &id, func() { ran.Store(true) }) }()
		go func() { release <- struct{}{} }()
		cancel()
		err := <-errc
		if (err == nil) != ran.Load() {
			t.Fatalf("call returned %v, then ran: %v", err, ran.Load())
		}
		if err == nil && id != 1 {
			t.Fatalf("succeeded without a result: %d", id)
		}
	}
}

func TestWhatThePeerSentBeforeClosingIsHandled(t *testing.T) {
	const rounds, each = 200, 20
	for round := 0; round < rounds; round++ {
		a, b := net.Pipe()
		var got atomic.Int64
		c := NewConn(b, HandlerFunc(func(ctx context.Context, c *Conn, method string, params json.RawMessage) (any, error) {
			got.Add(1)
			return nil, nil
		}))
		go func() {
			w := bufio.NewWriter(a)
			for i := 0; i < each; i++ {
				w.WriteString(`{"jsonrpc":"2.0","method":"note"}` + "\n")
			}
			w.Flush()
			a.Close()
		}()
		select {
		case <-c.Drained():
		case <-time.After(5 * time.Second):
			t.Fatal("the connection never drained")
		}
		if n := got.Load(); n != each {
			t.Fatalf("round %d: %d of %d notifications sent before the peer closed were handled", round, n, each)
		}
		c.Close()
	}
}

func TestAConnectionClosedHereDrainsAtOnce(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	c := NewConn(b, nil)
	c.Close()
	select {
	case <-c.Drained():
	case <-time.After(5 * time.Second):
		t.Fatal("a connection closed here never drained")
	}
}
