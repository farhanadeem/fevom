package daemon

import (
	"context"
	"reflect"
	"sync"
	"time"

	api "fev/proto/daemon"
)

const eventQueue = 256

type Hub struct {
	mu      sync.Mutex
	status  api.Status
	subs    map[*EventSub]struct{}
	waiters map[*waiter]struct{}
	tap     func(event any)
	paused  func(bool)
}

func NewHub(initial api.Status) *Hub {
	initial.Seq = 1
	initial.Since = stamp(time.Now())
	if initial.PendingRestart == nil {
		initial.PendingRestart = []string{}
	}
	return &Hub{status: initial, subs: map[*EventSub]struct{}{}, waiters: map[*waiter]struct{}{}}
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000Z07:00") }

func (h *Hub) Status() api.Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

func (h *Hub) Update(change func(*api.Status)) api.Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	next := h.status
	change(&next)
	if next.PendingRestart == nil {
		next.PendingRestart = []string{}
	}
	if next.State != api.StatePaused {
		next.PauseReason = ""
	}
	if next.State != api.StateStopped && next.State != api.StateStopping {
		next.StopRequested = false
	}
	if next.State != api.StateRunning && next.State != api.StatePaused {
		next.Windows = nil
	}
	if next.State == api.StateStopped || next.BootID != h.status.BootID {
		next.Data = nil
		next.Emulation = nil
		next.Debug = nil
	}
	next.Seq, next.Since = h.status.Seq, h.status.Since
	if reflect.DeepEqual(next, h.status) {
		return h.status
	}
	next.Seq++
	if next.State != h.status.State || next.Phase != h.status.Phase {
		next.Since = stamp(time.Now())
	}
	if was, is := h.status.State == api.StatePaused, next.State == api.StatePaused; was != is && h.paused != nil {
		h.paused(is)
	}
	h.status = next
	h.send(next)
	for w := range h.waiters {
		if w.until(next) {
			w.done <- next
			delete(h.waiters, w)
		}
	}
	return next
}

func (h *Hub) Publish(event any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.send(event)
}

func (h *Hub) send(event any) {
	if h.tap != nil {
		h.tap(event)
	}
	for s := range h.subs {
		if len(s.queue) >= eventQueue {
			delete(h.subs, s)
			s.closed = true
			wake(s.wake)
			go s.overflow()
			continue
		}
		s.queue = append(s.queue, event)
		wake(s.wake)
	}
}

func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

type EventSub struct {
	hub      *Hub
	overflow func()

	queue  []any
	closed bool
	wake   chan struct{}
}

func (h *Hub) Subscribe(overflow func()) (*EventSub, api.Status) {
	s := &EventSub{hub: h, overflow: overflow, wake: make(chan struct{}, 1)}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[s] = struct{}{}
	return s, h.status
}

func (s *EventSub) Next(stop <-chan struct{}) (any, bool) {
	for {
		s.hub.mu.Lock()
		if len(s.queue) > 0 {
			event := s.queue[0]
			s.queue = s.queue[1:]
			s.hub.mu.Unlock()
			return event, true
		}
		closed := s.closed
		s.hub.mu.Unlock()
		if closed {
			return nil, false
		}
		select {
		case <-s.wake:
		case <-stop:
			return nil, false
		}
	}
}

func (h *Hub) Finish() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		delete(h.subs, s)
		s.closed = true
		wake(s.wake)
	}
}

func (s *EventSub) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	delete(s.hub.subs, s)
	s.closed = true
	s.queue = nil
	wake(s.wake)
}

type waiter struct {
	until func(api.Status) bool
	done  chan api.Status
}

func (h *Hub) Wait(ctx context.Context, until func(api.Status) bool) (api.Status, error) {
	h.mu.Lock()
	if until(h.status) {
		st := h.status
		h.mu.Unlock()
		return st, nil
	}
	w := &waiter{until: until, done: make(chan api.Status, 1)}
	h.waiters[w] = struct{}{}
	h.mu.Unlock()
	select {
	case st := <-w.done:
		return st, nil
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.waiters, w)
		h.mu.Unlock()
		return api.Status{}, ctx.Err()
	}
}
