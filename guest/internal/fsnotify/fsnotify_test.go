package fsnotify

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	api "fev/proto/agent"
	"fev/proto/fsevents"
)

type vmm struct {
	t      *testing.T
	conns  chan net.Conn
	mu     sync.Mutex
	refuse bool
	dials  int
}

func newVMM(t *testing.T) *vmm { return &vmm{t: t, conns: make(chan net.Conn, 4)} }

func (v *vmm) dial() (io.ReadWriteCloser, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.dials++
	if v.refuse {
		return nil, errors.New("connection reset by peer")
	}
	guest, host := net.Pipe()
	v.conns <- host
	return guest, nil
}

func (v *vmm) accept(build string) net.Conn {
	v.t.Helper()
	c := <-v.conns
	f, err := fsevents.Read(c, fsevents.FromGuest)
	if err != nil {
		v.t.Fatalf("the guest's hello: %v", err)
	}
	if h, ok := f.(fsevents.Hello); !ok || h.Version != fsevents.Version || h.Build != "b1" {
		v.t.Fatalf("the guest's hello is %#v", f)
	}
	if err := fsevents.Write(c, fsevents.Hello{Version: fsevents.Version, Build: build}); err != nil {
		v.t.Fatal(err)
	}
	return c
}

type device struct {
	mu     sync.Mutex
	got    []fsevents.Event
	refuse func(fsevents.Event) bool
	wake   chan struct{}
}

func (d *device) emit(e fsevents.Event) (uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.got = append(d.got, e)
	select {
	case d.wake <- struct{}{}:
	default:
	}
	if d.refuse != nil && d.refuse(e) {
		return 0, errors.New("invalid argument")
	}
	return fsevents.HadParent, nil
}

func (d *device) wait(t *testing.T, n int) []fsevents.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		d.mu.Lock()
		got := append([]fsevents.Event{}, d.got...)
		d.mu.Unlock()
		if len(got) >= n {
			return got
		}
		select {
		case <-d.wake:
		case <-deadline:
			t.Fatalf("the device has %d records, want %d: %v", len(got), n, got)
		}
	}
}

func setup(t *testing.T) (*Helper, *vmm, *device) {
	v := newVMM(t)
	d := &device{wake: make(chan struct{}, 1)}
	return &Helper{Dial: v.dial, Emit: d.emit, Build: "b1"}, v, d
}

func connect(t *testing.T, h *Helper, v *vmm) net.Conn {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- h.Connect() }()
	c := v.accept("b1")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRecordsReachTheDeviceInOrder(t *testing.T) {
	h, v, d := setup(t)
	c := connect(t, h, v)
	defer c.Close()
	if s := h.Stats(); s.Feed != api.FeedConnected || s.FeedError != "" {
		t.Fatalf("stats after connecting: %+v", s)
	}
	batch := []fsevents.Event{
		{Op: fsevents.OpCreate, Parent: 7, Name: "new.txt"},
		{Op: fsevents.OpChange, Mask: fsevents.Content, Parent: 7, Child: 9, Name: "new.txt"},
		{Op: fsevents.OpRename, Parent: 7, Parent2: 8, Child: 9, Name: "new.txt", Name2: "old.txt"},
	}
	d.refuse = func(e fsevents.Event) bool { return e.Op == fsevents.OpChange }
	if err := fsevents.Write(c, fsevents.Events{Records: batch[:2]}); err != nil {
		t.Fatal(err)
	}
	if err := fsevents.Write(c, fsevents.Events{Records: batch[2:]}); err != nil {
		t.Fatal(err)
	}
	if got := d.wait(t, 3); !reflect.DeepEqual(got, batch) {
		t.Fatalf("the device got %v", got)
	}
	waitStats(t, h, func(s Stats) bool { return s.Records == 3 && s.Refused == 1 && s.Invalidations == 0 })
}

func waitStats(t *testing.T, h *Helper, ok func(Stats) bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if ok(h.Stats()) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("stats: %+v", h.Stats())
}

func TestWatchWaitsForItsAcknowledgement(t *testing.T) {
	h, v, _ := setup(t)
	c := connect(t, h, v)
	defer c.Close()
	type result struct {
		status []fsevents.Status
		err    error
	}
	first := make(chan result, 1)
	go func() {
		s, err := h.Watch(context.Background(), []string{"/Users/a/src", "/Users/a/gone"})
		first <- result{s, err}
	}()
	f, err := fsevents.Read(c, fsevents.FromGuest)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.(fsevents.Watch); w.Seq != 1 || !reflect.DeepEqual(w.Paths, []string{"/Users/a/src", "/Users/a/gone"}) {
		t.Fatalf("the first set is %#v", w)
	}
	second := make(chan result, 1)
	go func() {
		s, err := h.Watch(context.Background(), nil)
		second <- result{s, err}
	}()
	select {
	case r := <-first:
		t.Fatalf("Watch returned before its acknowledgement: %+v", r)
	case r := <-second:
		t.Fatalf("the second Watch returned first: %+v", r)
	case <-time.After(30 * time.Millisecond):
	}
	fsevents.Write(c, fsevents.Events{Records: []fsevents.Event{{Op: fsevents.OpChange, Child: 3, Mask: fsevents.MaskAttrib}}})
	fsevents.Write(c, fsevents.Watching{Seq: 1, Status: []fsevents.Status{fsevents.StatusOK, fsevents.StatusMissing}})
	r := <-first
	if r.err != nil || !reflect.DeepEqual(r.status, []fsevents.Status{fsevents.StatusOK, fsevents.StatusMissing}) {
		t.Fatalf("the first Watch: %+v", r)
	}
	if s := h.Stats(); s.Watching != 1 {
		t.Fatalf("watching %d paths, want 1", s.Watching)
	}
	f, err = fsevents.Read(c, fsevents.FromGuest)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.(fsevents.Watch); w.Seq != 2 || len(w.Paths) != 0 {
		t.Fatalf("the second set is %#v", w)
	}
	fsevents.Write(c, fsevents.Watching{Seq: 2, Status: nil})
	if r := <-second; r.err != nil || len(r.status) != 0 {
		t.Fatalf("the second Watch: %+v", r)
	}
	if s := h.Stats(); s.Watching != 0 {
		t.Fatalf("watching %d paths after the empty set", s.Watching)
	}
}

func TestALostFeedDropsCachesAndIsDialledOnceByTheNextWatch(t *testing.T) {
	h, v, d := setup(t)
	c := connect(t, h, v)
	c.Close()
	if got := d.wait(t, 1); got[0].Op != fsevents.OpInvalAll {
		t.Fatalf("the device got %v", got)
	}
	waitStats(t, h, func(s Stats) bool {
		return s.Feed == api.FeedLost && s.FeedError != "" && s.Invalidations == 1 && s.Records == 0
	})
	if v.dials != 1 {
		t.Fatalf("%d dials; nothing may dial on its own", v.dials)
	}
	v.mu.Lock()
	v.refuse = true
	v.mu.Unlock()
	if _, err := h.Watch(context.Background(), []string{"/a"}); err == nil {
		t.Fatal("Watch succeeded with nothing to dial")
	}
	if v.dials != 2 {
		t.Fatalf("%d dials, want 2", v.dials)
	}
	v.mu.Lock()
	v.refuse = false
	v.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := h.Watch(context.Background(), []string{"/a"})
		done <- err
	}()
	c = v.accept("b1")
	defer c.Close()
	f, err := fsevents.Read(c, fsevents.FromGuest)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.(fsevents.Watch); w.Seq != 1 {
		t.Fatalf("seq %d on a new connection", w.Seq)
	}
	fsevents.Write(c, fsevents.Watching{Seq: 1, Status: []fsevents.Status{fsevents.StatusOK}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s := h.Stats(); s.Feed != api.FeedConnected || s.Watching != 1 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestAWatchInFlightFailsWhenTheFeedIsLost(t *testing.T) {
	h, v, _ := setup(t)
	c := connect(t, h, v)
	done := make(chan error, 1)
	go func() {
		_, err := h.Watch(context.Background(), []string{"/a"})
		done <- err
	}()
	if _, err := fsevents.Read(c, fsevents.FromGuest); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "lost") {
		t.Fatalf("Watch returned %v", err)
	}
}

func TestProtocolErrorsEndTheConnection(t *testing.T) {
	for name, send := range map[string]func(c net.Conn){
		"an acknowledgement nobody asked for": func(c net.Conn) {
			fsevents.Write(c, fsevents.Watching{Seq: 1, Status: nil})
		},
		"a second hello": func(c net.Conn) {
			fsevents.Write(c, fsevents.Hello{Version: fsevents.Version, Build: "b1"})
		},
		"a frame the VMM may not send": func(c net.Conn) {
			c.Write([]byte{8, 0, 0, 0, fsevents.TypeWatch, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0})
		},
		"a malformed frame": func(c net.Conn) {
			c.Write([]byte{4, 0, 0, 0, fsevents.TypeEvents, 0, 0, 1, 0, 0, 0, 0})
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, v, d := setup(t)
			c := connect(t, h, v)
			defer c.Close()
			go send(c)
			d.wait(t, 1)
			waitStats(t, h, func(s Stats) bool { return s.Feed == api.FeedLost })
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := c.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("read after the error: %v", err)
			}
		})
	}
}

func TestAWrongAcknowledgementIsAProtocolError(t *testing.T) {
	h, v, _ := setup(t)
	c := connect(t, h, v)
	defer c.Close()
	done := make(chan error, 1)
	go func() {
		_, err := h.Watch(context.Background(), []string{"/a", "/b"})
		done <- err
	}()
	fsevents.Read(c, fsevents.FromGuest)
	go fsevents.Write(c, fsevents.Watching{Seq: 1, Status: []fsevents.Status{fsevents.StatusOK}})
	if err := <-done; err == nil {
		t.Fatal("Watch took an acknowledgement of the wrong length")
	}
}

func TestAnotherBuildIsAMismatchAndIsNotDialledAgain(t *testing.T) {
	h, v, _ := setup(t)
	done := make(chan error, 1)
	go func() { done <- h.Connect() }()
	c := v.accept("b2")
	defer c.Close()
	if err := <-done; err == nil {
		t.Fatal("connected to another build")
	}
	if s := h.Stats(); s.Feed != api.FeedMismatch || !strings.Contains(s.FeedError, "b2") {
		t.Fatalf("stats: %+v", s)
	}
	if _, err := h.Watch(context.Background(), nil); err == nil {
		t.Fatal("Watch succeeded against another build")
	}
	if v.dials != 1 {
		t.Fatalf("%d dials; a mismatch is not retried", v.dials)
	}
}

func TestWithoutADeviceTheFeedIsNeverDialled(t *testing.T) {
	v := newVMM(t)
	h := &Helper{Dial: v.dial, Build: "b1"}
	if err := h.Connect(); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := h.Watch(context.Background(), nil); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("Watch: %v", err)
	}
	if s := h.Stats(); s.Feed != api.FeedNone || v.dials != 0 {
		t.Fatalf("stats %+v, %d dials", s, v.dials)
	}
}

func TestACancelledWatchKeepsItsTurn(t *testing.T) {
	h, v, _ := setup(t)
	c := connect(t, h, v)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.Watch(ctx, []string{"/a"})
		done <- err
	}()
	fsevents.Read(c, fsevents.FromGuest)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Watch: %v", err)
	}
	next := make(chan error, 1)
	go func() {
		_, err := h.Watch(context.Background(), nil)
		next <- err
	}()
	select {
	case err := <-next:
		t.Fatalf("the next Watch did not wait: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	fsevents.Write(c, fsevents.Watching{Seq: 1, Status: []fsevents.Status{fsevents.StatusOK}})
	f, err := fsevents.Read(c, fsevents.FromGuest)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.(fsevents.Watch); w.Seq != 2 {
		t.Fatalf("seq %d", w.Seq)
	}
	fsevents.Write(c, fsevents.Watching{Seq: 2})
	if err := <-next; err != nil {
		t.Fatal(err)
	}
}

func TestReason(t *testing.T) {
	for s, want := range map[fsevents.Status]string{
		fsevents.StatusOK: "", fsevents.StatusOutside: api.WatchOutside,
		fsevents.StatusMissing: api.WatchMissing, fsevents.StatusFailed: api.WatchFailed, 9: api.WatchFailed,
	} {
		if got := Reason(s); got != want {
			t.Errorf("Reason(%d) = %q, want %q", s, got, want)
		}
	}
}

func TestOnStateIsToldOfEveryChangeOfTheFeed(t *testing.T) {
	h, v, d := setup(t)
	var mu sync.Mutex
	var seen []string
	h.OnState = func() {
		s := h.Stats()
		mu.Lock()
		seen = append(seen, s.Feed)
		mu.Unlock()
	}
	states := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, seen...)
	}
	until := func(want ...string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !reflect.DeepEqual(states(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("OnState saw %v, want %v", states(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	c := connect(t, h, v)
	until(api.FeedConnected)
	fsevents.Write(c, fsevents.Events{Records: []fsevents.Event{{Op: fsevents.OpInvalAll}}})
	d.wait(t, 1)
	done := make(chan error, 1)
	go func() {
		_, err := h.Watch(context.Background(), []string{"/a"})
		done <- err
	}()
	if _, err := fsevents.Read(c, fsevents.FromGuest); err != nil {
		t.Fatal(err)
	}
	fsevents.Write(c, fsevents.Watching{Seq: 1, Status: []fsevents.Status{fsevents.StatusOK}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	until(api.FeedConnected)

	c.Close()
	until(api.FeedConnected, api.FeedLost)
	v.mu.Lock()
	v.refuse = true
	v.mu.Unlock()
	if _, err := h.Watch(context.Background(), []string{"/a"}); err == nil {
		t.Fatal("Watch succeeded with nothing to dial")
	}
	until(api.FeedConnected, api.FeedLost, api.FeedNone)
	v.mu.Lock()
	v.refuse = false
	v.mu.Unlock()
	go func() {
		_, err := h.Watch(context.Background(), []string{"/a"})
		done <- err
	}()
	c = v.accept("b1")
	defer c.Close()
	if _, err := fsevents.Read(c, fsevents.FromGuest); err != nil {
		t.Fatal(err)
	}
	fsevents.Write(c, fsevents.Watching{Seq: 1, Status: []fsevents.Status{fsevents.StatusOK}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	until(api.FeedConnected, api.FeedLost, api.FeedNone, api.FeedConnected)
}
