package nfsview

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	api "fev/proto/agent"
)

type fakeStarter struct {
	mu      sync.Mutex
	starts  []api.ViewStartParams
	fail    error
	hold    chan struct{}
	lost    func(error)
	loseNow error
	runs    []*fakeRun
}

type fakeRun struct {
	mu      sync.Mutex
	stopped int
}

func (r *fakeRun) Stop() error {
	r.mu.Lock()
	r.stopped++
	r.mu.Unlock()
	return nil
}

func (r *fakeRun) Dial(context.Context) (net.Conn, error) {
	a, b := net.Pipe()
	b.Close()
	return a, nil
}

func (f *fakeStarter) Start(p api.ViewStartParams, lost func(error)) (Running, *Tree, error) {
	f.mu.Lock()
	f.starts = append(f.starts, p)
	hold, fail, loseNow := f.hold, f.fail, f.loseNow
	f.lost = lost
	f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if fail != nil {
		return nil, nil, fail
	}
	r := &fakeRun{}
	f.mu.Lock()
	f.runs = append(f.runs, r)
	f.mu.Unlock()
	if loseNow != nil {
		lost(loseNow)
	}
	t := NewTree("/v", Owner{p.UID, p.GID})
	t.Set(Object{Kind: KindMachine, Key: "m", Name: "m"})
	t.Set(Object{Kind: KindVolume, Key: "v", Name: "v"})
	return r, t, nil
}

type watch struct {
	t  *testing.T
	ch chan api.ViewStatus
}

func newWatch(t *testing.T) *watch { return &watch{t: t, ch: make(chan api.ViewStatus, 64)} }

func (w *watch) notify(s api.ViewStatus) {
	if err := api.CheckViewStatus(s); err != nil {
		w.t.Errorf("told %+v: %v", s, err)
	}
	w.ch <- s
}

func (w *watch) next(want string) api.ViewStatus {
	w.t.Helper()
	select {
	case s := <-w.ch:
		if s.State != want {
			w.t.Fatalf("told %s (%q), want %s", s.State, s.Error, want)
		}
		return s
	case <-time.After(5 * time.Second):
		w.t.Fatalf("nothing told, want %s", want)
	}
	return api.ViewStatus{}
}

func (w *watch) none() {
	w.t.Helper()
	select {
	case s := <-w.ch:
		w.t.Fatalf("told %+v, want nothing", s)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestViewStartStop(t *testing.T) {
	f := &fakeStarter{}
	w := newWatch(t)
	v := NewView(f, w.notify)
	if s := v.Status(); s.State != api.ViewStopped || api.CheckViewStatus(s) != nil {
		t.Fatalf("a new view: %+v", s)
	}
	if _, err := v.Dial(context.Background()); !errors.Is(err, ErrViewStopped) {
		t.Fatalf("dial while stopped: %v", err)
	}
	if s := v.Start(api.ViewStartParams{UID: 501, GID: 20}); s.State != api.ViewStarting {
		t.Fatalf("start returned %s", s.State)
	}
	w.next(api.ViewStarting)
	ready := w.next(api.ViewReady)
	if ready.Objects == nil || ready.Objects.Machines != 1 || ready.Objects.Volumes != 1 {
		t.Fatalf("ready's objects: %+v", ready.Objects)
	}
	if v.Tree() == nil || v.Tree().Owner() != (Owner{501, 20}) {
		t.Fatalf("the tree: %+v", v.Tree())
	}
	if s := v.Start(api.ViewStartParams{UID: 502, GID: 20}); s.State != api.ViewReady {
		t.Fatalf("a second start: %s", s.State)
	}
	w.none()
	if len(f.starts) != 1 || v.Tree().Owner().UID != 501 {
		t.Fatalf("a second start started again: %+v", f.starts)
	}
	c, err := v.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	s, err := v.Stop()
	if err != nil || s.State != api.ViewStopped {
		t.Fatalf("stop: %+v %v", s, err)
	}
	w.next(api.ViewStopped)
	if f.runs[0].stopped != 1 || v.Tree() != nil {
		t.Fatalf("stopped %d times, tree %v", f.runs[0].stopped, v.Tree())
	}
	if _, err := v.Dial(context.Background()); !errors.Is(err, ErrViewStopped) {
		t.Fatalf("dial after stop: %v", err)
	}
	if s, _ := v.Stop(); s.State != api.ViewStopped {
		t.Fatalf("stop again: %s", s.State)
	}
	w.none()
}

func TestViewStartFails(t *testing.T) {
	f := &fakeStarter{fail: errors.New("no nfsd")}
	w := newWatch(t)
	v := NewView(f, w.notify)
	v.Start(api.ViewStartParams{UID: 501})
	w.next(api.ViewStarting)
	if s := w.next(api.ViewFailed); s.Error != "no nfsd" {
		t.Fatalf("failed with %q", s.Error)
	}
	f.mu.Lock()
	f.fail = nil
	f.mu.Unlock()
	v.Start(api.ViewStartParams{UID: 501})
	w.next(api.ViewStarting)
	w.next(api.ViewReady)
}

func TestViewStopDuringStart(t *testing.T) {
	f := &fakeStarter{hold: make(chan struct{})}
	w := newWatch(t)
	v := NewView(f, w.notify)
	v.Start(api.ViewStartParams{UID: 501})
	w.next(api.ViewStarting)
	for {
		f.mu.Lock()
		n := len(f.starts)
		f.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan api.ViewStatus)
	go func() { s, _ := v.Stop(); done <- s }()
	select {
	case <-done:
		t.Fatal("the stop did not wait for the start")
	case <-time.After(50 * time.Millisecond):
	}
	close(f.hold)
	w.next(api.ViewReady)
	if s := <-done; s.State != api.ViewStopped {
		t.Fatalf("stop: %s", s.State)
	}
	w.next(api.ViewStopped)
	if f.runs[0].stopped != 1 {
		t.Fatalf("the start's run stopped %d times", f.runs[0].stopped)
	}
}

func TestViewLost(t *testing.T) {
	f := &fakeStarter{}
	w := newWatch(t)
	v := NewView(f, w.notify)
	v.Start(api.ViewStartParams{UID: 501})
	w.next(api.ViewStarting)
	w.next(api.ViewReady)
	f.lost(errors.New("nfsview: the upcall answers ended"))
	if s := w.next(api.ViewFailed); s.Error != "nfsview: the upcall answers ended" {
		t.Fatalf("failed with %q", s.Error)
	}
	if f.runs[0].stopped != 1 || v.Tree() != nil {
		t.Fatalf("a lost view's run stopped %d times", f.runs[0].stopped)
	}
	f.lost(errors.New("again"))
	w.none()
	v.Stop()
	w.next(api.ViewStopped)
}

func TestViewLostDuringStart(t *testing.T) {
	f := &fakeStarter{loseNow: errors.New("cld ended")}
	w := newWatch(t)
	v := NewView(f, w.notify)
	v.Start(api.ViewStartParams{UID: 501})
	w.next(api.ViewStarting)
	if s := w.next(api.ViewFailed); s.Error != "cld ended" {
		t.Fatalf("failed with %q", s.Error)
	}
	if f.runs[0].stopped != 1 {
		t.Fatalf("the run stopped %d times", f.runs[0].stopped)
	}
}
