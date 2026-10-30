package netstack

import (
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
)

type TimerCounts struct {
	Armed   uint64
	Fired   uint64
	Pending int64
}

type timerCounts struct {
	armed, fired atomic.Uint64
	pending      atomic.Int64
}

func (c *timerCounts) read() TimerCounts {
	return TimerCounts{Armed: c.armed.Load(), Fired: c.fired.Load(), Pending: c.pending.Load()}
}

var looseTimers timerCounts

func LooseTimers() TimerCounts { return looseTimers.read() }

type countedTimer struct {
	c *timerCounts
	f func()

	mu    sync.Mutex
	t     *time.Timer
	due   time.Time
	armed bool
}

func (c *timerCounts) afterFunc(d time.Duration, f func()) *countedTimer {
	t := &countedTimer{c: c, f: f}
	t.mu.Lock()
	t.count(d)
	t.t = time.AfterFunc(d, t.fire)
	t.mu.Unlock()
	return t
}

func (t *countedTimer) count(d time.Duration) {
	t.armed = true
	t.due = time.Now().Add(d)
	t.c.armed.Add(1)
	t.c.pending.Add(1)
}

func (t *countedTimer) fire() {
	t.mu.Lock()
	if !t.armed || time.Now().Before(t.due) {
		t.mu.Unlock()
		return
	}
	t.armed = false
	t.c.pending.Add(-1)
	t.c.fired.Add(1)
	t.mu.Unlock()
	t.f()
}

func (t *countedTimer) disarm() bool {
	if !t.armed {
		return false
	}
	t.armed = false
	t.t.Stop()
	t.c.pending.Add(-1)
	return true
}

func (t *countedTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.disarm()
}

func (t *countedTimer) Reset(d time.Duration) {
	t.mu.Lock()
	if t.armed {
		t.armed = false
		t.c.pending.Add(-1)
	}
	t.count(d)
	t.t.Reset(d)
	t.mu.Unlock()
}

type countedClock struct {
	tcpip.Clock
	c *timerCounts
}

func newCountedClock(c *timerCounts) tcpip.Clock {
	return &countedClock{Clock: tcpip.NewStdClock(), c: c}
}

func (k *countedClock) AfterFunc(d time.Duration, f func()) tcpip.Timer {
	return k.c.afterFunc(d, f)
}
