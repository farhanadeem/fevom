package netstack

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestIdleWithLifetimesShorterThanItsOwnSetUp(t *testing.T) {
	var expired atomic.Int32
	for range 200 {
		i := NewIdle(time.Nanosecond, time.Nanosecond, func() { expired.Add(1) })
		i.Sent()
		i.Replied()
		i.Stop()
	}
	done := make(chan struct{})
	i := NewIdle(2*time.Millisecond, 2*time.Millisecond, func() { close(done) })
	i.Sent()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a quiet flow did not expire")
	}
	i.Stop()
	before := expired.Load()
	time.Sleep(20 * time.Millisecond)
	if expired.Load() != before {
		t.Fatal("a stopped flow expired later")
	}
}
