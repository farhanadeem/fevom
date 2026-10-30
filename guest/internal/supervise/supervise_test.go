package supervise

import (
	"errors"
	"sync"
	"testing"
	"time"

	api "fev/proto/agent"
)

type fake struct {
	t        *testing.T
	mu       sync.Mutex
	runs     []*fakeRun
	startErr error
	nowMS    float64
	timers   []*fakeTimer
	stops    []time.Duration
}

type fakeRun struct {
	run    *Run
	ready  chan error
	exited chan struct{}
	how    string
	once   sync.Once
}

func (r *fakeRun) exit(how string) {
	r.once.Do(func() { r.how = how; close(r.exited) })
}

type fakeTimer struct {
	d         time.Duration
	f         func()
	cancelled bool
}

func (f *fake) hooks() Hooks {
	return Hooks{
		Start: func() (*Run, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.startErr != nil {
				return nil, f.startErr
			}
			r := &fakeRun{ready: make(chan error, 1), exited: make(chan struct{})}
			r.run = &Run{PID: 100 + len(f.runs), Ready: r.ready, Exited: r.exited, How: func() string { return r.how }}
			f.runs = append(f.runs, r)
			return r.run, nil
		},
		Stop: func(run *Run, grace time.Duration) {
			f.mu.Lock()
			f.stops = append(f.stops, grace)
			var r *fakeRun
			for _, c := range f.runs {
				if c.run == run {
					r = c
				}
			}
			f.mu.Unlock()
			r.exit("signal 15")
		},
		NowMS: func() float64 { f.mu.Lock(); defer f.mu.Unlock(); return f.nowMS },
		After: func(d time.Duration, fn func()) func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			tm := &fakeTimer{d: d, f: fn}
			f.timers = append(f.timers, tm)
			return func() { f.mu.Lock(); tm.cancelled = true; f.mu.Unlock() }
		},
		Logf: func(string, ...any) {},
	}
}

func (f *fake) last() *fakeRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[len(f.runs)-1]
}

func (f *fake) advance(d time.Duration) {
	f.mu.Lock()
	f.nowMS += float64(d.Milliseconds())
	f.mu.Unlock()
}

func (f *fake) fire() time.Duration {
	f.t.Helper()
	f.mu.Lock()
	if len(f.timers) == 0 {
		f.mu.Unlock()
		f.t.Fatal("no restart is scheduled")
	}
	tm := f.timers[len(f.timers)-1]
	f.timers = f.timers[:len(f.timers)-1]
	f.mu.Unlock()
	if !tm.cancelled {
		tm.f()
	}
	return tm.d
}

type states struct {
	mu  sync.Mutex
	got []api.EngineStatus
	ch  chan struct{}
}

func watch(s *Supervisor) *states {
	st := &states{ch: make(chan struct{}, 64)}
	s.Subscribe(func(e api.EngineStatus) {
		st.mu.Lock()
		st.got = append(st.got, e)
		st.mu.Unlock()
		st.ch <- struct{}{}
	})
	return st
}

func (st *states) until(t *testing.T, want string) api.EngineStatus {
	t.Helper()
	return st.when(t, want, func(e api.EngineStatus) bool { return e.State == want })
}

func (st *states) running(t *testing.T) api.EngineStatus {
	t.Helper()
	return st.when(t, "starting with a process", func(e api.EngineStatus) bool { return e.State == api.EngineStarting && e.PID != 0 })
}

func (st *states) pending(t *testing.T) api.EngineStatus {
	t.Helper()
	return st.when(t, "a scheduled restart", func(e api.EngineStatus) bool {
		return e.State == api.EngineStarting && e.PID == 0 && e.LastExit != ""
	})
}

func (st *states) when(t *testing.T, want string, match func(api.EngineStatus) bool) api.EngineStatus {
	t.Helper()
	seen := 0
	deadline := time.After(5 * time.Second)
	for {
		st.mu.Lock()
		for ; seen < len(st.got); seen++ {
			if match(st.got[seen]) {
				e := st.got[seen]
				st.got = st.got[seen+1:]
				st.mu.Unlock()
				return e
			}
		}
		st.mu.Unlock()
		select {
		case <-st.ch:
		case <-deadline:
			t.Fatalf("%s was never reported", want)
		}
	}
}

func TestReadyMeansTheProcessSaidSoAndStatesArriveInOrder(t *testing.T) {
	f := &fake{t: t, nowMS: 1000}
	s := New(f.hooks())
	st := watch(s)
	if got := st.until(t, api.EngineStopped); got.Starts != 0 {
		t.Fatalf("first state %+v", got)
	}
	status, err := s.Start()
	if err != nil || status.State != api.EngineStarting || status.PID != 100 || status.Starts != 1 || status.StartedMS != 1000 {
		t.Fatalf("start: %+v, %v", status, err)
	}
	f.advance(300 * time.Millisecond)
	f.last().ready <- nil
	if got := st.until(t, api.EngineReady); got.ReadyMS != 1300 || got.PID != 100 {
		t.Fatalf("ready: %+v", got)
	}
	if status, _ := s.Start(); status.Starts != 1 || status.State != api.EngineReady {
		t.Fatalf("second start: %+v", status)
	}
}

func TestACrashIsRestartedWithAGrowingDelayAndGivenUpOnAfterFive(t *testing.T) {
	f := &fake{t: t}
	s := New(f.hooks())
	st := watch(s)
	s.Start()
	var delays []time.Duration
	for i := range MaxFailures {
		st.running(t)
		f.last().exit("exit 1")
		if i < MaxFailures-1 {
			if got := st.pending(t); got.LastExit != "exit 1" {
				t.Fatalf("after crash %d: %+v", i, got)
			}
			delays = append(delays, f.fire())
		}
	}
	got := st.until(t, api.EngineFailed)
	if got.Error == "" || got.PID != 0 || got.Starts != MaxFailures {
		t.Fatalf("gave up as %+v", got)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("delays %v, want %v", delays, want)
		}
	}
	if len(f.timers) != 0 {
		t.Fatalf("%d restarts still scheduled", len(f.timers))
	}
	if status, err := s.Start(); err != nil || status.State != api.EngineStarting || status.Starts != MaxFailures+1 {
		t.Fatalf("start after giving up: %+v, %v", status, err)
	}
}

func TestTenSecondsReadyEndsACrashLoop(t *testing.T) {
	f := &fake{t: t, nowMS: 5000}
	s := New(f.hooks())
	st := watch(s)
	s.Start()
	for range 3 {
		st.running(t)
		f.last().exit("exit 1")
		st.pending(t)
		f.fire()
	}
	st.running(t)
	f.last().ready <- nil
	st.until(t, api.EngineReady)
	f.advance(HealthyAfter)
	f.last().exit("signal 9")
	st.pending(t)
	if d := f.fire(); d != FirstDelay {
		t.Fatalf("delay after a healthy run was %v, want %v", d, FirstDelay)
	}
}

func TestStopAndRestartAreNotCrashes(t *testing.T) {
	f := &fake{t: t}
	s := New(f.hooks())
	st := watch(s)
	s.Start()
	f.last().ready <- nil
	st.until(t, api.EngineReady)

	status, err := s.Restart(3 * time.Second)
	if err != nil || status.State != api.EngineStarting || status.Starts != 2 || status.PID != 101 || status.LastExit != "signal 15" {
		t.Fatalf("restart: %+v, %v", status, err)
	}
	st.until(t, api.EngineStopping)
	st.until(t, api.EngineStopped)
	st.until(t, api.EngineStarting)
	if len(f.stops) != 1 || f.stops[0] != 3*time.Second {
		t.Fatalf("stop was given %v", f.stops)
	}
	if len(f.timers) != 0 {
		t.Fatal("a requested stop scheduled a restart")
	}

	status, _ = s.Stop(time.Second)
	if status.State != api.EngineStopped || status.PID != 0 {
		t.Fatalf("stop: %+v", status)
	}
	if status, err := s.Stop(time.Second); err != nil || status.State != api.EngineStopped || len(f.stops) != 2 {
		t.Fatalf("second stop: %+v, %v, %d stops", status, err, len(f.stops))
	}
}

func TestStopCancelsAPendingRestart(t *testing.T) {
	f := &fake{t: t}
	s := New(f.hooks())
	st := watch(s)
	s.Start()
	f.last().exit("exit 2")
	st.pending(t)
	s.Stop(time.Second)
	f.fire()
	if status := s.Status(); status.State != api.EngineStopped || status.Starts != 1 {
		t.Fatalf("the cancelled restart ran: %+v", status)
	}
}

func TestNotReadyIsAFailedStart(t *testing.T) {
	f := &fake{t: t}
	s := New(f.hooks())
	st := watch(s)
	s.Start()
	st.running(t)
	f.last().ready <- errors.New("ping: 500")
	got := st.pending(t)
	if got.LastExit != "signal 15" {
		t.Fatalf("after a failed ping: %+v", got)
	}
	if len(f.stops) != 1 {
		t.Fatalf("the unready process was not stopped")
	}
}

func TestAbsentAndClosedRefuseAStart(t *testing.T) {
	f := &fake{t: t}
	s := New(f.hooks())
	s.Absent("no engine in the system image")
	if status, err := s.Start(); err == nil || status.State != api.EngineFailed || status.Error == "" {
		t.Fatalf("start with nothing to start: %+v, %v", status, err)
	}
	if _, err := s.Restart(time.Second); err == nil {
		t.Fatal("restart with nothing to start succeeded")
	}

	s = New(f.hooks())
	s.Start()
	s.Close(time.Second)
	if status := s.Status(); status.State != api.EngineStopped {
		t.Fatalf("after close: %+v", status)
	}
	if _, err := s.Start(); !errors.Is(err, ErrClosed) {
		t.Fatalf("start after close: %v", err)
	}
}

func TestAStartThatCannotSpawnCountsAsAFailure(t *testing.T) {
	f := &fake{t: t, startErr: errors.New("no such file")}
	s := New(f.hooks())
	status, err := s.Start()
	if err != nil || status.State != api.EngineStarting || status.PID != 0 {
		t.Fatalf("%+v, %v", status, err)
	}
	for range MaxFailures - 1 {
		f.fire()
	}
	if len(f.runs) != 0 {
		t.Fatal("a run exists although every start failed")
	}
	if status := s.Status(); status.State != api.EngineFailed {
		t.Fatalf("%+v", status)
	}
}

func TestAStaleRestartTimerStartsNothing(t *testing.T) {
	f := &fake{t: t, startErr: errors.New("no such file")}
	s := New(f.hooks())
	s.Start()
	f.mu.Lock()
	stale := f.timers[0]
	f.mu.Unlock()
	s.Start()
	f.mu.Lock()
	scheduled := len(f.timers)
	f.mu.Unlock()
	stale.f()
	f.mu.Lock()
	after := len(f.timers)
	f.startErr = nil
	f.mu.Unlock()
	if after != scheduled {
		t.Fatalf("the stale timer made a start of its own: %d restarts scheduled, then %d", scheduled, after)
	}
	st := watch(s)
	f.fire()
	st.running(t)
}
