package fsnotify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	api "fev/proto/agent"
	"fev/proto/fsevents"
	"fev/proto/version"
)

type Helper struct {
	Dial    func() (io.ReadWriteCloser, error)
	Emit    func(fsevents.Event) (uint32, error)
	Build   string
	Logf    func(format string, args ...any)
	OnState func()

	turnOnce sync.Once
	turn     chan struct{}

	mu      sync.Mutex
	state   string
	problem string
	conn    io.ReadWriteCloser
	seq     uint32
	sent    *fsevents.Watch
	answer  chan answer

	watching      int
	records       uint64
	refused       uint64
	invalidations uint64
}

type answer struct {
	status []fsevents.Status
	err    error
}

type Stats struct {
	Feed, FeedError                 string
	Watching                        int
	Records, Refused, Invalidations uint64
}

func (h *Helper) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
	}
}

func (h *Helper) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	state := h.state
	if state == "" {
		state = api.FeedNone
	}
	return Stats{Feed: state, FeedError: h.problem, Watching: h.watching,
		Records: h.records, Refused: h.refused, Invalidations: h.invalidations}
}

var ErrNoDevice = errors.New("the guest kernel has no file-change device")

func (h *Helper) Connect() error {
	h.mu.Lock()
	err := h.connectLocked()
	h.mu.Unlock()
	h.changed()
	return err
}

func (h *Helper) changed() {
	if h.OnState != nil {
		h.OnState()
	}
}

func (h *Helper) connectLocked() error {
	fail := func(state string, err error) error {
		h.state, h.problem = state, err.Error()
		return err
	}
	if h.Emit == nil {
		return fail(api.FeedNone, ErrNoDevice)
	}
	conn, err := h.Dial()
	if err != nil {
		return fail(api.FeedNone, fmt.Errorf("the VMM does not answer the change feed: %w", err))
	}
	if err := fsevents.Write(conn, fsevents.Hello{Version: fsevents.Version, Build: h.Build}); err != nil {
		conn.Close()
		return fail(api.FeedNone, fmt.Errorf("the change feed's hello could not be sent: %w", err))
	}
	order := fsevents.NewOrder(fsevents.FromHost)
	f, err := fsevents.Read(conn, fsevents.FromHost)
	if err == nil {
		err = order.Check(f)
	}
	if err != nil {
		conn.Close()
		return fail(api.FeedNone, fmt.Errorf("the VMM's hello on the change feed: %w", err))
	}
	if err := f.(fsevents.Hello).Check(h.Build, "VMM"); err != nil {
		conn.Close()
		var m *version.Mismatch
		if errors.As(err, &m) || f.(fsevents.Hello).Version != fsevents.Version {
			return fail(api.FeedMismatch, err)
		}
		return fail(api.FeedNone, err)
	}
	h.conn, h.seq, h.sent, h.answer = conn, 0, nil, nil
	h.state, h.problem, h.watching = api.FeedConnected, "", 0
	go h.read(conn, order)
	return nil
}

func (h *Helper) read(conn io.ReadWriteCloser, order *fsevents.Order) {
	var err error
	for {
		var f fsevents.Frame
		if f, err = fsevents.Read(conn, fsevents.FromHost); err != nil {
			break
		}
		if err = order.Check(f); err != nil {
			break
		}
		switch f := f.(type) {
		case fsevents.Events:
			h.apply(f.Records)
		case fsevents.Watching:
			if err = h.acknowledged(conn, f); err != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	conn.Close()
	h.lost(conn, err)
}

func (h *Helper) apply(records []fsevents.Event) {
	var refused, inval uint64
	for _, e := range records {
		if _, err := h.Emit(e); err != nil {
			refused++
			if refused == 1 {
				h.logf("fsnotify: the device refused a record (%s): %v", e.Op, err)
			}
		}
		if e.Op == fsevents.OpInvalAll {
			inval++
		}
	}
	h.mu.Lock()
	h.records += uint64(len(records))
	h.refused += refused
	h.invalidations += inval
	h.mu.Unlock()
	if inval > 0 {
		h.logf("fsnotify: the VMM reports lost events; everything cached from the share was dropped")
	}
}

func (h *Helper) acknowledged(conn io.ReadWriteCloser, w fsevents.Watching) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn != conn || h.sent == nil {
		return errors.New("fsevents: an acknowledgement of a watch set that was not sent")
	}
	if err := w.Answers(*h.sent); err != nil {
		return err
	}
	n := 0
	for _, s := range w.Status {
		if s == fsevents.StatusOK {
			n++
		}
	}
	h.watching = n
	h.answer <- answer{status: w.Status}
	h.sent, h.answer = nil, nil
	return nil
}

func (h *Helper) lost(conn io.ReadWriteCloser, err error) {
	h.mu.Lock()
	if h.conn != conn {
		h.mu.Unlock()
		return
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = errors.New("the VMM closed the change feed")
	}
	h.conn, h.state, h.problem, h.watching = nil, api.FeedLost, err.Error(), 0
	if h.sent != nil {
		h.answer <- answer{err: fmt.Errorf("the change feed was lost before the VMM acknowledged the watch set: %w", err)}
		h.sent, h.answer = nil, nil
	}
	h.invalidations++
	h.mu.Unlock()
	h.changed()
	h.logf("fsnotify: the change feed is lost (%v); everything cached from the share is dropped", err)
	if _, err := h.Emit(fsevents.Event{Op: fsevents.OpInvalAll}); err != nil {
		h.logf("fsnotify: the device refused to drop its caches: %v", err)
	}
}

func (h *Helper) Watch(ctx context.Context, paths []string) ([]fsevents.Status, error) {
	h.turnOnce.Do(func() { h.turn = make(chan struct{}, 1) })
	select {
	case h.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	redialled := false
	h.mu.Lock()
	switch h.state {
	case api.FeedConnected:
	case api.FeedMismatch:
		err := errors.New(h.problem)
		h.mu.Unlock()
		<-h.turn
		return nil, err
	default:
		if err := h.connectLocked(); err != nil {
			h.mu.Unlock()
			h.changed()
			<-h.turn
			return nil, err
		}
		h.logf("fsnotify: the change feed is connected again")
		redialled = true
	}
	w := fsevents.Watch{Seq: h.seq + 1, Paths: paths}
	got := make(chan answer, 1)
	conn := h.conn
	frame, err := fsevents.Append(nil, w)
	if err != nil {
		h.mu.Unlock()
		if redialled {
			h.changed()
		}
		<-h.turn
		return nil, err
	}
	h.seq, h.sent, h.answer = w.Seq, &w, got
	h.mu.Unlock()
	if redialled {
		h.changed()
	}
	if _, err := conn.Write(frame); err != nil {
		conn.Close()
	}
	select {
	case a := <-got:
		<-h.turn
		return a.status, a.err
	case <-ctx.Done():
		go func() {
			<-got
			<-h.turn
		}()
		return nil, ctx.Err()
	}
}

func Reason(s fsevents.Status) string {
	switch s {
	case fsevents.StatusOK:
		return ""
	case fsevents.StatusOutside:
		return api.WatchOutside
	case fsevents.StatusMissing:
		return api.WatchMissing
	}
	return api.WatchFailed
}
