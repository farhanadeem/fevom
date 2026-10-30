package netstack

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	UDPUnreplied = 30 * time.Second
	UDPAssured   = 120 * time.Second
)

var udpUnreplied, udpAssured = UDPUnreplied, UDPAssured

type Idle struct {
	unreplied, assured time.Duration
	start              time.Time
	last               atomic.Int64
	replied            atomic.Bool
	mu                 sync.Mutex
	stopped            bool
	timer              *countedTimer
	expire             func()
}

func NewIdle(unreplied, assured time.Duration, expire func()) *Idle {
	return newIdle(&looseTimers, unreplied, assured, expire)
}

func newIdle(c *timerCounts, unreplied, assured time.Duration, expire func()) *Idle {
	i := &Idle{unreplied: unreplied, assured: assured, start: time.Now(), expire: expire}
	i.mu.Lock()
	i.timer = c.afterFunc(unreplied, i.fire)
	i.mu.Unlock()
	return i
}

func (i *Idle) fire() {
	i.mu.Lock()
	if i.stopped {
		i.mu.Unlock()
		return
	}
	lifetime := i.unreplied
	if i.replied.Load() {
		lifetime = i.assured
	}
	quiet := time.Since(i.start) - time.Duration(i.last.Load())
	if quiet < lifetime {
		i.timer.Reset(lifetime - quiet)
		i.mu.Unlock()
		return
	}
	i.mu.Unlock()
	i.expire()
}

func (i *Idle) Sent() { i.last.Store(int64(time.Since(i.start))) }

func (i *Idle) Replied() {
	i.last.Store(int64(time.Since(i.start)))
	i.replied.Store(true)
}

func (i *Idle) Stop() {
	i.mu.Lock()
	i.stopped = true
	i.timer.Stop()
	i.mu.Unlock()
}
