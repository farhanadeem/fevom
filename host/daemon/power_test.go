package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fev/host/power"
	"fev/proto/agent"
	api "fev/proto/daemon"
)

func powered(t *testing.T, change func(*fixture), script ...string) (*fixture, *power.Injected) {
	t.Helper()
	f := newFixture(t, script)
	src := power.NewInjected()
	f.opts.Power = src
	if change != nil {
		change(f)
	}
	f.start()
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	return f, src
}

func send[T any](t *testing.T, src *power.Injected, kind power.Kind) T {
	t.Helper()
	got := make(chan any, 1)
	go func() { got <- src.Send(kind) }()
	select {
	case v := <-got:
		out, ok := v.(T)
		if !ok {
			t.Fatalf("%v was dealt with as %T", kind, v)
		}
		return out
	case <-time.After(15 * time.Second):
		t.Fatalf("the daemon never dealt with %q", kind)
	}
	panic("unreachable")
}

type holder struct {
	stub
	n atomic.Int32
}

func (h *holder) Held() int { return int(h.n.Load()) }

func count(text, what string) int { return strings.Count(text, what) }

func TestASleepPausesTheVMAndTheWakeResumesItAndSetsItsClock(t *testing.T) {
	held := &holder{}
	f, src := powered(t, func(f *fixture) { f.opts.Modules = []Module{held} })
	before := f.running()

	var mu sync.Mutex
	var order []string
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	yes := map[bool]string{true: "yes", false: "no"}
	if _, _, err := f.client().Subscribe(bg, api.Events{
		State: func(st api.Status) { note("state " + st.State + " " + st.PauseReason) },
		Sleep: func(n api.PowerSleep) { note("sleep changed=" + yes[n.Changed]) },
		Wake:  func(n api.PowerWake) { note("wake resumed=" + yes[n.Resumed]) },
	}); err != nil {
		t.Fatal(err)
	}

	slept := send[api.PowerSleep](t, src, power.WillSleep)
	if !slept.Paused || !slept.Changed || slept.TimedOut || slept.PauseMS <= 0 || slept.PauseMS > api.PauseBoundMS || slept.Time == "" {
		t.Fatalf("the first will-sleep: %+v", slept)
	}
	st := f.d.hub.Status()
	if st.State != api.StatePaused || st.PauseReason != api.PauseSleep || st.Engine != api.EngineReady || st.BootID != before.BootID || st.Phase != "" {
		t.Fatalf("after the acknowledgement: %+v", st)
	}
	again := send[api.PowerSleep](t, src, power.WillSleep)
	if !again.Paused || again.Changed || again.PauseMS != 0 {
		t.Fatalf("a second will-sleep while paused: %+v", again)
	}

	time.Sleep(300 * time.Millisecond)
	held.n.Store(3)
	woke := send[api.PowerWake](t, src, power.HasPoweredOn)
	if !woke.Resumed || woke.PausedMS < 300 || woke.PausedMS > 5000 || woke.Released != 3 {
		t.Fatalf("the first has-powered-on: %+v", woke)
	}
	if !woke.ClockStepped || woke.ClockOffsetMS > -300 || woke.ClockOffsetMS < -woke.PausedMS-100 {
		t.Fatalf("the clock after %v ms parked: %+v", woke.PausedMS, woke)
	}
	if st := f.d.hub.Status(); st.State != api.StateRunning || st.PauseReason != "" || st.BootID != before.BootID || st.Engine != api.EngineReady {
		t.Fatalf("after the wake: %+v", st)
	}
	held.n.Store(0)
	twice := send[api.PowerWake](t, src, power.HasPoweredOn)
	if twice.Resumed || twice.PausedMS != 0 || twice.ClockStepped || twice.ClockOffsetMS != 0 || twice.Released != 0 {
		t.Fatalf("a second has-powered-on with the VM running: %+v", twice)
	}

	deadline := time.Now().Add(5 * time.Second)
	want := "state paused sleep, sleep changed=yes, sleep changed=no, state running , wake resumed=yes, wake resumed=no"
	for {
		mu.Lock()
		got := strings.Join(order, ", ")
		mu.Unlock()
		if got == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a subscriber saw\n%s\nwant\n%s", got, want)
		}
		time.Sleep(time.Millisecond)
	}
	vmm := f.logFile("vmm.log")
	if count(vmm, "control: paused") != 1 || count(vmm, "control: resumed") != 1 || count(vmm, "already paused") != 0 {
		t.Errorf("the VMM's side:\n%s", vmm)
	}
	if n := count(f.logFile("guest.log"), "stepped the wall clock"); n != 1 {
		t.Errorf("the guest's clock was stepped %d times", n)
	}
	log := f.logFile("fevd.log")
	if count(log, " sleep: ") != 2 || count(log, " wake: ") != 2 {
		t.Errorf("the daemon's log has %d sleep and %d wake lines for two of each:\n%s", count(log, " sleep: "), count(log, " wake: "), log)
	}
}

func TestTheGuestsClockIsSetAtHello(t *testing.T) {
	f := started(t, []string{"clock-behind-ms=700"})
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(f.logFile("guest.log"), "stepped the wall clock by 7") {
		if time.Now().After(deadline) {
			t.Fatalf("no step of 700 ms after the hello:\n%s", f.logFile("guest.log"))
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAVMMThatNeverPausesDoesNotHoldTheMacAwake(t *testing.T) {
	f, src := powered(t, func(f *fixture) {
		f.opts.Timing.PauseBound = 150 * time.Millisecond
		f.opts.Timing.ResumeBound = 200 * time.Millisecond
	}, "pause-never=1")
	first := f.running()
	began := time.Now()
	slept := send[api.PowerSleep](t, src, power.WillSleep)
	if took := time.Since(began); took < 150*time.Millisecond || took > 2*time.Second {
		t.Errorf("the Mac was let sleep after %v with a bound of 150 ms", took)
	}
	if !slept.TimedOut || slept.Paused || slept.Changed {
		t.Fatalf("will-sleep with a VMM that never pauses: %+v", slept)
	}
	if st := f.d.hub.Status(); st.State != api.StateRunning {
		t.Fatalf("the state followed a pause that never landed: %+v", st)
	}
	if again := send[api.PowerSleep](t, src, power.WillSleep); !again.TimedOut {
		t.Fatalf("a second will-sleep: %+v", again)
	}
	if n := count(f.logFile("vmm.log"), "pause asked for"); n != 1 {
		t.Errorf("the VMM was asked to pause %d times", n)
	}
	woke := send[api.PowerWake](t, src, power.HasPoweredOn)
	if woke.Resumed {
		t.Fatalf("has-powered-on: %+v", woke)
	}
	st := f.until("the VM started again", func(st api.Status) bool {
		return st.State == api.StateRunning && st.BootID != first.BootID
	})
	if st.LastExit == nil || !strings.Contains(f.logFile("fevd.log"), "the VMM did not resume after the Mac's sleep") {
		t.Errorf("after the wake: %+v", st)
	}
}

func TestWithPauseInSleepOffAndWithNoVMNothingIsPaused(t *testing.T) {
	f, src := powered(t, nil)
	f.running()
	if _, err := f.client().ConfigSet(bg, map[string]any{api.KeyPowerPauseInSleep: false}); err != nil {
		t.Fatal(err)
	}
	if slept := send[api.PowerSleep](t, src, power.WillSleep); slept.Paused || slept.Changed || slept.TimedOut {
		t.Fatalf("will-sleep with the setting off: %+v", slept)
	}
	if st := f.d.hub.Status(); st.State != api.StateRunning {
		t.Fatalf("%+v", st)
	}
	if woke := send[api.PowerWake](t, src, power.HasPoweredOn); woke.Resumed || woke.ClockStepped {
		t.Fatalf("has-powered-on with the setting off: %+v", woke)
	}
	if vmm := f.logFile("vmm.log"); strings.Contains(vmm, "control:") {
		t.Errorf("the VMM was asked something:\n%s", vmm)
	}
	c := f.client()
	if _, err := c.ConfigReset(bg, api.KeyPowerPauseInSleep); err != nil {
		t.Fatal(err)
	}
	if _, err := c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	if slept := send[api.PowerSleep](t, src, power.WillSleep); slept.Paused || slept.Changed {
		t.Fatalf("will-sleep with no VM: %+v", slept)
	}
	if woke := send[api.PowerWake](t, src, power.HasPoweredOn); woke.Resumed || woke.ClockStepped {
		t.Fatalf("has-powered-on with no VM: %+v", woke)
	}
	if st := f.d.hub.Status(); st.State != api.StateStopped {
		t.Fatalf("%+v", st)
	}
}

func TestAHealthyVMIsNotMarkedDeadWhileItIsParked(t *testing.T) {
	f, src := powered(t, func(f *fixture) {
		f.opts.Timing.EngineDeadline = 40 * time.Millisecond
		f.opts.Timing.AliveBound = 60 * time.Millisecond
		f.opts.Timing.ClockBound = 80 * time.Millisecond
		f.opts.Timing.HelloTimeout = 100 * time.Millisecond
		f.opts.Timing.StartDeadline = 2 * time.Second
	})
	first := f.running()
	if slept := send[api.PowerSleep](t, src, power.WillSleep); !slept.Paused {
		t.Fatalf("%+v", slept)
	}
	c := f.client()
	for i := 0; i < 6; i++ {
		time.Sleep(100 * time.Millisecond)
		st, err := c.Status(bg)
		if err != nil || st.State != api.StatePaused || st.Error != "" || len(st.Warnings) != 0 || st.BootID != first.BootID {
			t.Fatalf("%d ms into the pause: %+v, %v", (i+1)*100, st, err)
		}
	}
	woke := send[api.PowerWake](t, src, power.HasPoweredOn)
	if !woke.Resumed || woke.PausedMS < 600 || !woke.ClockStepped {
		t.Fatalf("%+v", woke)
	}
	st, err := c.Status(bg)
	if err != nil || st.State != api.StateRunning || st.Error != "" || len(st.Warnings) != 0 || st.BootID != first.BootID || st.Engine != api.EngineReady {
		t.Fatalf("after the wake: %+v, %v", st, err)
	}
	if log := f.logFile("fevd.log"); strings.Contains(log, "not responding") || strings.Contains(log, "giving up") || strings.Contains(log, "was not ready") || strings.Contains(log, "did not take the Mac's time") {
		t.Errorf("something timed out on a parked guest:\n%s", log)
	}
}

func TestAStopWhilePausedResumesTheGuestToShutItDown(t *testing.T) {
	f, src := powered(t, nil)
	f.running()
	if slept := send[api.PowerSleep](t, src, power.WillSleep); !slept.Paused {
		t.Fatalf("%+v", slept)
	}
	st, err := f.client().VMStop(bg, api.StopParams{})
	if err != nil || st.State != api.StateStopped || st.PauseReason != "" || !st.StopRequested {
		t.Fatalf("vm.stop while paused: %+v, %v", st, err)
	}
	if st.LastExit == nil || !st.LastExit.Expected || st.LastExit.Code != 0 {
		t.Errorf("the VM's exit: %+v", st.LastExit)
	}
	if vmm := f.logFile("vmm.log"); count(vmm, "control: resumed") != 1 || strings.Contains(vmm, "lifeline closed") {
		t.Errorf("the VMM's side of a stop while paused:\n%s", vmm)
	}
	if woke := send[api.PowerWake](t, src, power.HasPoweredOn); woke.Resumed {
		t.Fatalf("%+v", woke)
	}
}

func TestASleepNobodyAnnouncedIsNoticedAtTheNextCall(t *testing.T) {
	var asleep atomic.Int64
	asleep.Store(int64(time.Hour))
	old := asleepNow
	asleepNow = func() time.Duration { return time.Duration(asleep.Load()) }
	defer func() { asleepNow = old }()

	f, src := powered(t, nil)
	f.running()
	c := f.client()
	sets := func() int {
		log := f.logFile("guest.log")
		return count(log, "stepped the wall clock") + count(log, "not stepped")
	}
	settle := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for sets() != want {
			if time.Now().After(deadline) {
				t.Fatalf("time.set was called %d times, want %d:\n%s", sets(), want, f.logFile("guest.log"))
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		if sets() != want {
			t.Fatalf("time.set was called %d times, want %d", sets(), want)
		}
	}
	settle(1)
	c.Status(bg)
	c.Status(bg)
	settle(1)
	asleep.Add(int64(5 * time.Second))
	c.Status(bg)
	c.Status(bg)
	settle(2)
	asleep.Add(int64(50 * time.Millisecond))
	c.Status(bg)
	settle(2)
	if slept := send[api.PowerSleep](t, src, power.WillSleep); !slept.Paused {
		t.Fatalf("%+v", slept)
	}
	asleep.Add(int64(time.Minute))
	if st, err := c.Status(bg); err != nil || st.State != api.StatePaused {
		t.Fatalf("%+v, %v", st, err)
	}
	if vmm := f.logFile("vmm.log"); strings.Contains(vmm, "control: resumed") {
		t.Fatal("a call served during the sleep resumed the VM")
	}
	settle(2)
	send[api.PowerWake](t, src, power.HasPoweredOn)
	settle(3)
	c.Status(bg)
	settle(3)
}

func TestAVMMThatHasStoppedAnsweringIsReportedWhenTheStatusIsAskedFor(t *testing.T) {
	f := newFixture(t)
	hang := filepath.Join(f.layout.Home, "hang")
	f.launch.script = [][]string{{"hang-file=" + hang}}
	f.opts.Timing.AliveBound = 100 * time.Millisecond
	f.start()
	c := f.client()
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	if st, err := c.Status(bg); err != nil || len(st.Warnings) != 0 {
		t.Fatalf("a VMM that answers: %+v, %v", st, err)
	}
	touch(t, hang)
	began := time.Now()
	st, err := c.Status(bg)
	if err != nil || st.State != api.StateRunning || !contains(st.Warnings, NotRespondingWarning) {
		t.Fatalf("a VMM that does not answer: %+v, %v", st, err)
	}
	if took := time.Since(began); took < 100*time.Millisecond || took > 3*time.Second {
		t.Errorf("the status took %v with a bound of 100 ms", took)
	}
	c.Status(bg)
	if n := count(f.logFile("fevd.log"), "the VM is not responding"); n != 1 {
		t.Errorf("the log says so %d times", n)
	}
	if st, _ := c.Status(bg); count(strings.Join(st.Warnings, "\n"), NotRespondingWarning) != 1 {
		t.Errorf("warnings: %q", st.Warnings)
	}
	remove(t, hang)
	moduleHost{f.d}.CheckAlive()
	if st, err := c.Status(bg); err != nil || contains(st.Warnings, NotRespondingWarning) {
		t.Fatalf("a VMM that answers again: %+v, %v", st, err)
	}
}

func TestAnEngineThatIsNotReadyInTimeIsReportedAndALateOneIsTaken(t *testing.T) {
	f := newFixture(t, []string{"engine-ms=600"})
	f.opts.Timing.EngineDeadline = 150 * time.Millisecond
	f.start()
	c := f.client()
	_, err := c.VMStart(bg, api.StartParams{})
	if err == nil || !strings.Contains(err.Error(), "the Docker engine was not ready 150ms after it was started") {
		t.Fatalf("vm.start with an engine that takes 600 ms and a deadline of 150: %v", err)
	}
	st := f.d.hub.Status()
	if st.State != api.StateStarting || st.Phase != api.PhaseEngine || st.Engine != api.EngineFailed || f.launch.started() != 1 {
		t.Fatalf("past the deadline: %+v after %d boots", st, f.launch.started())
	}
	st = f.running()
	if st.Engine != api.EngineReady || st.Error != "" || f.launch.started() != 1 {
		t.Fatalf("when the engine was ready after all: %+v", st)
	}
}

func TestAStopThatWasAskedForIsRememberedUntilAStart(t *testing.T) {
	f := started(t)
	mark := api.StopMarkPath(f.layout.Home)
	c := f.client()
	if st, _ := c.Status(bg); st.StopRequested {
		t.Fatalf("a VM that was never started: %+v", st)
	}
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	h := f.watch(f.client())
	st, err := c.VMStop(bg, api.StopParams{})
	if err != nil || st.State != api.StateStopped || !st.StopRequested {
		t.Fatalf("vm.stop: %+v, %v", st, err)
	}
	h.states(t, st.Seq)
	h.mu.Lock()
	for _, seen := range h.seen {
		if (seen.State == api.StateStopping || seen.State == api.StateStopped) != seen.StopRequested {
			t.Errorf("state %s with stop_requested=%v", seen.State, seen.StopRequested)
		}
	}
	h.mu.Unlock()
	if _, err := os.Stat(mark); err != nil {
		t.Errorf("vm.stop alone left no mark: %v", err)
	}
	st, err = c.VMRestart(bg, api.StartParams{})
	if err != nil || st.State != api.StateRunning || st.StopRequested {
		t.Fatalf("vm.restart: %+v, %v", st, err)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("vm.restart left the mark")
	}
	if err := (moduleHost{f.d}).StopVM(bg); err != nil {
		t.Fatal(err)
	}
	if st := f.stopped(); st.StopRequested {
		t.Fatalf("a stop the daemon made for itself: %+v", st)
	}
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}

	watcher := f.client()
	last := make(chan api.Status, 64)
	watcher.Subscribe(bg, api.Events{State: func(st api.Status) { last <- st }})
	if _, err := c.Shutdown(bg, api.ShutdownParams{StopRequested: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mark); err != nil {
		t.Fatalf("the mark was not there when daemon.shutdown returned: %v", err)
	}
	<-f.d.Done()
	select {
	case <-watcher.Drained():
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher's connection outlived the daemon")
	}
	var final api.Status
	for len(last) > 0 {
		final = <-last
	}
	if final.State != api.StateStopped || !final.StopRequested {
		t.Errorf("the last status of a daemon that was asked to stop: %+v", final)
	}
	f.d.release()

	g := newFixture(t)
	g.layout, g.opts.Layout = f.layout, f.layout
	g.launch.Process.Dir = f.layout.VMDir()
	g.start()
	gc := g.client()
	if st, _ := gc.Status(bg); !st.StopRequested {
		t.Fatalf("a daemon started over a standing stop: %+v", st)
	}
	if st, err := gc.VMStart(bg, api.StartParams{}); err != nil || st.StopRequested {
		t.Fatalf("vm.start: %+v, %v", st, err)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("vm.start left the mark")
	}
}

func TestOnlyAStopTheUserAskedForLeavesAMarkAndALoginClearsIt(t *testing.T) {
	f := started(t)
	mark := api.StopMarkPath(f.layout.Home)
	if _, err := f.client().Shutdown(bg, api.ShutdownParams{}); err != nil {
		t.Fatal(err)
	}
	<-f.d.Done()
	f.d.release()
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("a shutdown nobody asked for as a stop left the mark")
	}
	touch(t, mark)
	g := newFixture(t)
	g.layout, g.opts.Layout = f.layout, f.layout
	g.launch.Process.Dir = f.layout.VMDir()
	g.opts.Login = true
	g.start()
	if st, _ := g.client().Status(bg); st.StopRequested {
		t.Fatalf("a daemon started at login: %+v", st)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("a daemon started at login left the mark")
	}
}

type asserts struct {
	mu      sync.Mutex
	held    map[int]string
	next    int
	reasons []string
}

func (a *asserts) assert(reason string, display bool) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.held == nil {
		a.held = map[int]string{}
	}
	id := a.next
	a.next++
	kind := "system"
	if display {
		kind = "display"
	}
	a.held[id] = kind
	a.reasons = append(a.reasons, reason)
	return func() {
		a.mu.Lock()
		delete(a.held, id)
		a.mu.Unlock()
	}, nil
}

func (a *asserts) now() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var system, display int
	for _, kind := range a.held {
		if kind == "display" {
			display++
		} else {
			system++
		}
	}
	return strings.Repeat("s", system) + strings.Repeat("d", display)
}

func TestAnAssertionIsHeldWhileAnyConnectionThatAskedIsOpen(t *testing.T) {
	a := &asserts{}
	f := newFixture(t)
	f.opts.Assert = a.assert
	f.start()
	wait := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for a.now() != want {
			if time.Now().After(deadline) {
				t.Fatalf("assertions held: %q, want %q", a.now(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	one, two := f.client(), f.client()
	r, err := one.CaffeinateHold(bg, api.CaffeinateParams{Reason: "make test"})
	if err != nil || r.Holders != 1 || r.Display {
		t.Fatalf("the first hold: %+v, %v", r, err)
	}
	wait("s")
	if r, err = two.CaffeinateHold(bg, api.CaffeinateParams{Display: true}); err != nil || r.Holders != 2 || !r.Display {
		t.Fatalf("a second, with the display: %+v, %v", r, err)
	}
	wait("sd")
	if r, err = two.CaffeinateHold(bg, api.CaffeinateParams{}); err != nil || r.Holders != 2 || !r.Display {
		t.Fatalf("the same connection again: %+v, %v", r, err)
	}
	wait("sd")
	two.Close()
	wait("s")
	if r, err = one.CaffeinateHold(bg, api.CaffeinateParams{}); err != nil || r.Holders != 1 || r.Display {
		t.Fatalf("after the other went: %+v, %v", r, err)
	}
	one.Close()
	wait("")
	a.mu.Lock()
	reasons := strings.Join(a.reasons, "|")
	a.mu.Unlock()
	if reasons != "fev: make test|fev: make test" && reasons != "fev: make test|fev" {
		t.Errorf("the reasons shown to the Mac: %s", reasons)
	}
	three := f.client()
	if _, err := three.CaffeinateHold(bg, api.CaffeinateParams{Display: true}); err != nil {
		t.Fatal(err)
	}
	wait("sd")
	f.d.Shutdown(false)
	wait("")
}

func TestStartAtLoginWritesAndRemovesALaunchAgent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "LaunchAgents")
	f := newFixture(t)
	f.opts.LoginItem = &LoginItem{Dir: dir, Program: "/Applications/Fevom & Co/fevd", Args: []string{"--home", f.layout.Home}}
	f.start()
	path := filepath.Join(dir, "dev.fevom.fevd.plist")
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("something was written with the setting off")
	}
	c := f.client()
	if _, err := c.ConfigSet(bg, map[string]any{api.KeyStartAtLogin: true}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the LaunchAgent was not written by the time config.set returned: %v", err)
	}
	if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Errorf("plutil does not accept it: %s", out)
	}
	read := func(key string) string {
		format := "raw"
		if key == "ProgramArguments" {
			format = "json"
		}
		out, err := exec.Command("plutil", "-extract", key, format, "-o", "-", path).Output()
		if err != nil {
			t.Fatalf("plutil -extract %s: %v\n%s", key, err, data)
		}
		return strings.TrimSpace(string(out))
	}
	if got := read("Label"); got != "dev.fevom.fevd" {
		t.Errorf("Label: %s", got)
	}
	if got := read("RunAtLoad"); got != "true" {
		t.Errorf("RunAtLoad: %s", got)
	}
	want := `["\/Applications\/Fevom & Co\/fevd","--foreground","--no-autostart","--login","--home","` + strings.ReplaceAll(f.layout.Home, "/", `\/`) + `"]`
	if got := read("ProgramArguments"); got != want {
		t.Errorf("ProgramArguments: %s\nwant %s", got, want)
	}
	if strings.Contains(string(data), "KeepAlive") {
		t.Error("KeepAlive: a daemon that was stopped would be started again")
	}
	info, _ := os.Stat(path)
	f.d.Shutdown(false)
	f.d.release()
	g := newFixture(t)
	g.layout, g.opts.Layout = f.layout, f.layout
	g.launch.Process.Dir = f.layout.VMDir()
	g.opts.LoginItem = f.opts.LoginItem
	g.start()
	if again, _ := os.Stat(path); again == nil || !again.ModTime().Equal(info.ModTime()) {
		t.Error("the file was rewritten with nothing changed")
	}
	if _, err := g.client().ConfigReset(bg, api.KeyStartAtLogin); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the LaunchAgent is still there with the setting off")
	}

	h := started(t)
	if _, err := h.client().ConfigSet(bg, map[string]any{api.KeyStartAtLogin: true}); err != nil {
		t.Fatal(err)
	}
	if log := h.logFile("fevd.log"); !strings.Contains(log, "manages no LaunchAgent") {
		t.Errorf("the log does not say the setting has no effect here:\n%s", log)
	}
}

func TestInjectedPowerArrivesOnASocketOfTheDaemonsOwn(t *testing.T) {
	f := newFixture(t)
	f.opts.PowerInject = filepath.Join(f.layout.RunDir(), "power.sock")
	f.start()
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(f.opts.PowerInject); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the socket: %v, %v", info, err)
	}
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	for _, step := range []struct{ line, want string }{
		{"sleep", `"paused":true,"changed":true`}, {"sleep", `"paused":true,"changed":false`},
		{"wake", `"resumed":true`}, {"wake", `"resumed":false`}, {"nap", `"error"`},
	} {
		out, err := exec.CommandContext(ctx, "/bin/sh", "-c", `printf '%s\n' "$1" | nc -U "$2"`, "sh", step.line, f.opts.PowerInject).Output()
		if err != nil || !strings.Contains(string(out), step.want) {
			t.Fatalf("%s answered %q (%v), want %s", step.line, out, err, step.want)
		}
	}
	f.d.Shutdown(false)
	if _, err := os.Stat(f.opts.PowerInject); err == nil {
		t.Error("the socket outlived the daemon")
	}
}

func TestTheEngineDeadlineDoesNotRunThroughAPause(t *testing.T) {
	f, src := powered(t, nil, "engine-ms=400")
	f.running()
	f.d.sup.mu.Lock()
	f.d.sup.timing.EngineDeadline = 150 * time.Millisecond
	f.d.sup.mu.Unlock()
	vm, ok := f.d.sup.VM()
	if !ok {
		t.Fatal("no VM")
	}
	if _, err := vm.Agent().EngineRestart(bg, agent.EngineStopParams{}); err != nil {
		t.Fatal(err)
	}
	f.until("the engine on its way", func(st api.Status) bool { return st.Engine != api.EngineReady })
	if slept := send[api.PowerSleep](t, src, power.WillSleep); !slept.Paused {
		t.Fatalf("%+v", slept)
	}
	time.Sleep(600 * time.Millisecond)
	if st := f.d.hub.Status(); st.State != api.StatePaused || st.Engine == api.EngineFailed || st.Error != "" {
		t.Fatalf("during the pause: %+v", st)
	}
	send[api.PowerWake](t, src, power.HasPoweredOn)
	st := f.until("the engine ready", func(st api.Status) bool { return st.Engine == api.EngineReady })
	if st.State != api.StateRunning || st.Error != "" {
		t.Fatalf("after the wake: %+v", st)
	}
	if log := f.logFile("fevd.log"); strings.Contains(log, "was not ready") {
		t.Errorf("the engine's deadline ran on a parked guest:\n%s", log)
	}
}

type quieter struct {
	stub
	mu     sync.Mutex
	events []string
	f      *fixture
}

func (q *quieter) note(s string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.f != nil && strings.Contains(q.f.logFile("vmm.log"), "pause asked for") {
		s += " (the VMM asked already)"
	}
	q.events = append(q.events, s)
}
func (q *quieter) Quiet() { q.note("quiet") }
func (q *quieter) QuietWait(deadline time.Time) int {
	q.note("wait")
	return 0
}
func (q *quieter) Unquiet() { q.note("unquiet") }
func (q *quieter) got() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return strings.Join(q.events, ", ")
}

func TestThePausesQuietersAreQuietedBeforeTheVMMIsAskedAndSetBackIfItNeverLands(t *testing.T) {
	q := &quieter{}
	_, src := powered(t, func(f *fixture) { f.opts.Modules = []Module{q}; q.f = f })
	if s := send[api.PowerSleep](t, src, power.WillSleep); !s.Paused {
		t.Fatalf("will-sleep: %+v", s)
	}
	if got := q.got(); got != "quiet, wait" {
		t.Fatalf("a pause that landed told the Quieter: %q, want \"quiet, wait\", both before the VMM was asked", got)
	}
	send[api.PowerWake](t, src, power.HasPoweredOn)

	q2 := &quieter{}
	f2, src2 := powered(t, func(f *fixture) {
		f.opts.Modules = []Module{q2}
		f.opts.Timing.PauseBound = 150 * time.Millisecond
	}, "pause-never=1")
	_ = f2
	if s := send[api.PowerSleep](t, src2, power.WillSleep); s.Paused {
		t.Fatalf("will-sleep with a VMM that never pauses: %+v", s)
	}
	send[api.PowerWake](t, src2, power.HasPoweredOn)
	deadline := time.Now().Add(10 * time.Second)
	for !strings.HasSuffix(q2.got(), "unquiet") {
		if time.Now().After(deadline) {
			t.Fatalf("a pause that never landed told the Quieter: %q, want it set back", q2.got())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
