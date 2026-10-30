package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fev/host/daemon/logs"
	api "fev/proto/daemon"
	"fev/proto/version"
)

type pipeClient struct {
	t       *testing.T
	conn    net.Conn
	want    chan struct{}
	got     chan wireMessage
	waiting bool
}

func (f *fixture) pipe() *pipeClient {
	client, server := net.Pipe()
	f.d.serve(server)
	f.t.Cleanup(func() { client.Close() })
	p := &pipeClient{t: f.t, conn: client, want: make(chan struct{}), got: make(chan wireMessage)}
	go func() {
		defer close(p.got)
		sc := bufio.NewScanner(client)
		sc.Buffer(nil, 1<<20)
		for range p.want {
			if !sc.Scan() {
				return
			}
			var m wireMessage
			json.Unmarshal(sc.Bytes(), &m)
			p.got <- m
		}
	}()
	f.t.Cleanup(func() { close(p.want) })
	p.call(1, api.MethodHello, fmt.Sprintf(`{"api_version":%d,"build":%q,"client":"pipe"}`, api.APIVersion, version.Build))
	return p
}

type wireMessage struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}

func (p *pipeClient) call(id int, method, params string) json.RawMessage {
	p.t.Helper()
	go fmt.Fprintf(p.conn, `{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`+"\n", id, method, params)
	for {
		m, ok := p.next(5 * time.Second)
		if !ok {
			p.t.Fatalf("no reply to %s", method)
		}
		if m.ID != nil && *m.ID == id {
			return m.Result
		}
	}
}

func (p *pipeClient) next(patience time.Duration) (wireMessage, bool) {
	if !p.waiting {
		select {
		case p.want <- struct{}{}:
			p.waiting = true
		case _, ok := <-p.got:
			return wireMessage{}, ok
		}
	}
	select {
	case m, ok := <-p.got:
		p.waiting = false
		return m, ok
	case <-time.After(patience):
		return wireMessage{}, false
	}
}

func TestDMNS8AStalledSubscriberCannotStallTheDaemon(t *testing.T) {
	const burst = 10000
	f := started(t)
	stalledEvents := f.pipe()
	stalledEvents.call(2, api.MethodEventsSubscribe, `{}`)
	stalledLogs := f.pipe()
	stalledLogs.call(2, api.MethodLogSubscribe, `{"sources":["console"]}`)

	bystander := f.client()
	healthy := f.client()
	h := f.watch(healthy)

	maxQueued := 0
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < burst; i++ {
			f.d.hub.Update(func(st *api.Status) { st.RetryInMS = i + 1 })
			f.d.logs.Write(logs.Entry{Source: api.LogConsole, Level: 6, Text: fmt.Sprintf("line %d", i)})
			if i%64 == 0 {
				f.d.hub.mu.Lock()
				for s := range f.d.hub.subs {
					maxQueued = max(maxQueued, len(s.queue))
				}
				f.d.hub.mu.Unlock()
				h.reached(t, uint64(i)+2-min(uint64(i), 100))
			}
		}
	}()
	var took []time.Duration
	driving := make(chan struct{})
	go func() { wg.Wait(); close(driving) }()
	for calling := true; calling; {
		select {
		case <-driving:
			calling = false
		default:
		}
		begun := time.Now()
		if _, err := bystander.Status(bg); err != nil {
			t.Fatalf("a call while subscribers were stalled: %v", err)
		}
		took = append(took, time.Since(begun))
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	worst, p99 := took[len(took)-1], took[len(took)*99/100]
	t.Logf("%d calls during the burst: median %v, p99 %v, worst %v", len(took), took[len(took)/2], p99, worst)
	if worst > 250*time.Millisecond || p99 > 50*time.Millisecond {
		t.Errorf("calls were slow while subscribers were stalled: p99 %v, worst %v", p99, worst)
	}

	last := f.d.hub.Status()
	h.states(t, last.Seq)
	h.mu.Lock()
	if len(h.seen) != burst {
		t.Errorf("the healthy subscriber saw %d of %d transitions", len(h.seen), burst)
	}
	h.mu.Unlock()

	if maxQueued > eventQueue {
		t.Errorf("an events queue reached %d entries; the bound is %d", maxQueued, eventQueue)
	}
	f.d.hub.mu.Lock()
	subs := len(f.d.hub.subs)
	f.d.hub.mu.Unlock()
	if subs != 1 {
		t.Errorf("%d events subscribers are registered; only the healthy one should be", subs)
	}
	got := 0
	for {
		if _, ok := stalledEvents.next(5 * time.Second); !ok {
			break
		}
		got++
	}
	if got > 2 {
		t.Errorf("the stalled events subscriber still received %d messages", got)
	}
	if _, err := stalledEvents.conn.Write([]byte("{}\n")); err == nil {
		t.Error("the stalled events subscriber's connection is still open")
	}
	if log := f.logFile("fevd.log"); !contains([]string{log}, "a client stopped reading its events and was disconnected client=pipe") {
		t.Error("the daemon's log does not say why the client was disconnected")
	}

	received, dropped, asked := 0, 0, false
	for {
		m, ok := stalledLogs.next(300 * time.Millisecond)
		if !ok {
			if asked {
				t.Fatal("the log subscriber's stream stopped")
			}
			asked = true
			f.d.logs.Write(logs.Entry{Source: api.LogConsole, Level: 6, Text: "after the burst"})
			continue
		}
		var e api.LogEntry
		json.Unmarshal(m.Params, &e)
		if e.Dropped > 0 {
			dropped += e.Dropped
		}
		if e.Text == "after the burst" {
			break
		}
		received++
	}
	if received+dropped != burst || dropped == 0 || received > logs.QueueLines+2 {
		t.Errorf("the log subscriber received %d lines and was told %d were lost, of %d", received, dropped, burst)
	}
}

func TestDMNS8SubscribingDuringABurstMissesNoTransition(t *testing.T) {
	const transitions = 3000
	const subscribers = 12
	f := started(t)
	start := make(chan struct{})
	driven := make(chan struct{})
	var progress atomic.Int64
	go func() {
		defer close(driven)
		<-start
		for i := 0; i < transitions; i++ {
			f.d.hub.Update(func(st *api.Status) { st.RetryInMS = i + 1 })
			progress.Store(int64(i + 1))
			for i%32 == 0 && f.longestQueue() > eventQueue/2 {
				time.Sleep(50 * time.Microsecond)
			}
		}
	}()
	histories := make([]*history, subscribers)
	var wg sync.WaitGroup
	for i := range histories {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := f.client()
			<-start
			target := int64((i + 1) * transitions / (subscribers + 1))
			for progress.Load() < target {
				time.Sleep(50 * time.Microsecond)
			}
			histories[i] = f.watch(c)
		}(i)
	}
	close(start)
	wg.Wait()
	<-driven
	last := f.d.hub.Status().Seq
	if last != 1+transitions {
		t.Fatalf("the hub is at seq %d after %d transitions", last, transitions)
	}
	during := 0
	for i, h := range histories {
		h.states(t, last)
		h.mu.Lock()
		if n := uint64(len(h.seen)); h.snapshot.Seq+n != last {
			t.Errorf("subscriber %d: snapshot %d plus %d transitions does not reach %d", i, h.snapshot.Seq, n, last)
		}
		if h.snapshot.Seq > 1 && h.snapshot.Seq < last {
			during++
		}
		h.mu.Unlock()
	}
	if during == 0 {
		t.Errorf("no subscriber arrived during the burst; the test did not exercise the race")
	}
}

func TestUnsubscribingEndsTheStreamAndOnlyTheOwnerCan(t *testing.T) {
	f := started(t)
	a, b := f.pipe(), f.pipe()
	var sub api.EventsSubscribed
	json.Unmarshal(a.call(2, api.MethodEventsSubscribe, `{}`), &sub)
	if sub.Status.Seq != 1 || sub.Subscription == 0 {
		t.Fatalf("subscribed: %+v", sub)
	}
	b.call(2, api.MethodEventsUnsubscribe, fmt.Sprintf(`{"subscription":%d}`, sub.Subscription))
	f.d.hub.Update(func(st *api.Status) { st.RetryInMS = 1 })
	if m, ok := a.next(5 * time.Second); !ok || m.Method != api.NotifyStateChanged {
		t.Fatalf("after another client's unsubscribe: %+v %v", m, ok)
	}
	a.call(3, api.MethodEventsUnsubscribe, fmt.Sprintf(`{"subscription":%d}`, sub.Subscription))
	f.d.hub.Update(func(st *api.Status) { st.RetryInMS = 2 })
	if m, ok := a.next(100 * time.Millisecond); ok {
		t.Errorf("a notification after unsubscribing: %+v", m)
	}
	var again api.EventsSubscribed
	json.Unmarshal(b.call(3, api.MethodEventsSubscribe, `{}`), &again)
	b.call(4, api.MethodLogSubscribe, `{}`)
	b.conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.d.hub.mu.Lock()
		subs := len(f.d.hub.subs)
		f.d.hub.mu.Unlock()
		if subs == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d subscriptions outlived their connections", subs)
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *fixture) longestQueue() int {
	f.d.hub.mu.Lock()
	defer f.d.hub.mu.Unlock()
	longest := 0
	for s := range f.d.hub.subs {
		longest = max(longest, len(s.queue))
	}
	return longest
}

func (h *history) reached(t *testing.T, seq uint64) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.seen)
		done := seq <= h.snapshot.Seq || (n > 0 && h.seen[n-1].Seq >= seq)
		h.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("the subscriber never saw seq %d", seq)
			return
		}
		time.Sleep(50 * time.Microsecond)
	}
}
