package logbuf

import (
	"fmt"
	"io"
	"sync"
)

type Entry struct {
	TimeMS float64
	Level  int
	Text   string
}

const (
	Error   = 3
	Warning = 4
	Info    = 6
	Debug   = 7
)

type Buffer struct {
	now     func() float64
	console io.Writer

	mu      sync.Mutex
	entries []Entry
	start   int
	subs    map[uint64]*sub
	nextSub uint64
}

type sub struct {
	mu      sync.Mutex
	queue   []Entry
	wake    chan struct{}
	dropped int
}

const subQueueLimit = 1024

func New(capacity int, now func() float64, console io.Writer) *Buffer {
	return &Buffer{now: now, console: console, entries: make([]Entry, 0, capacity), subs: map[uint64]*sub{}}
}

func (b *Buffer) Logf(level int, format string, args ...any) {
	b.Append(Entry{TimeMS: b.now(), Level: level, Text: fmt.Sprintf(format, args...)})
}

func (b *Buffer) Append(e Entry) {
	b.mu.Lock()
	if len(b.entries) < cap(b.entries) {
		b.entries = append(b.entries, e)
	} else {
		b.entries[b.start] = e
		b.start = (b.start + 1) % len(b.entries)
	}
	for _, s := range b.subs {
		s.push(e)
	}
	console := b.console
	b.mu.Unlock()
	if console != nil && e.Level <= Info {
		fmt.Fprintf(console, "fev-init: %s\n", e.Text)
	}
}

func (s *sub) push(e Entry) {
	s.mu.Lock()
	if len(s.queue) >= subQueueLimit {
		s.dropped++
	} else {
		s.queue = append(s.queue, e)
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (b *Buffer) Subscribe(backlog bool, deliver func(Entry)) (cancel func()) {
	return b.SubscribeMarked(backlog, deliver, nil)
}

func (b *Buffer) SubscribeMarked(backlog bool, deliver func(Entry), backlogDone func()) (cancel func()) {
	s := &sub{wake: make(chan struct{}, 1)}
	b.mu.Lock()
	if backlog {
		n := len(b.entries)
		for i := range n {
			s.queue = append(s.queue, b.entries[(b.start+i)%n])
		}
	}
	held := len(s.queue)
	b.nextSub++
	id := b.nextSub
	b.subs[id] = s
	b.mu.Unlock()

	stop := make(chan struct{})
	go func() {
		for {
			s.mu.Lock()
			batch, dropped := s.queue, s.dropped
			s.queue, s.dropped = nil, 0
			s.mu.Unlock()
			for _, e := range batch {
				deliver(e)
				if held--; held == 0 && backlogDone != nil {
					backlogDone()
					backlogDone = nil
				}
			}
			if held <= 0 && backlogDone != nil {
				backlogDone()
				backlogDone = nil
			}
			if dropped > 0 {
				deliver(Entry{TimeMS: b.now(), Level: Warning, Text: fmt.Sprintf("%d log entries dropped: subscriber too slow", dropped)})
			}
			if len(batch) > 0 {
				continue
			}
			select {
			case <-s.wake:
			case <-stop:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
			close(stop)
		})
	}
}

func (b *Buffer) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
