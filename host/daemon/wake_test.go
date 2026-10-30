package daemon

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"fev/host/power"
	api "fev/proto/daemon"
)

func asleepVM(t *testing.T, change func(*fixture), script ...string) (*fixture, *power.Injected) {
	t.Helper()
	f, src := powered(t, change, script...)
	f.running()
	if slept := send[api.PowerSleep](t, src, power.WillSleep); !slept.Paused {
		t.Fatalf("the VM was not paused: %+v", slept)
	}
	return f, src
}

func TestASleepThatIsCalledOffEndsThePause(t *testing.T) {
	f, src := asleepVM(t, nil)
	time.Sleep(150 * time.Millisecond)
	woke := send[api.PowerWake](t, src, power.WillNotSleep)
	if !woke.Resumed || woke.Cause != api.WakeNotSleep || !woke.ClockStepped {
		t.Fatalf("will-not-sleep with the VM paused: %+v", woke)
	}
	if st := f.d.hub.Status(); st.State != api.StateRunning || st.PauseReason != "" {
		t.Fatalf("after the sleep was called off: %+v", st)
	}
	if again := send[api.PowerWake](t, src, power.WillNotSleep); again.Resumed || again.ClockStepped {
		t.Fatalf("a second will-not-sleep: %+v", again)
	}
}

func TestAWakeTheMacBeganAndNeverFinishedIsFinishedByTheFirstCallThatNeedsTheGuest(t *testing.T) {
	held := &holder{}
	f, src := asleepVM(t, func(f *fixture) { f.opts.Modules = []Module{held} })
	c := f.client()
	host := moduleHost{f.d}

	if st, err := c.Status(bg); err != nil || st.State != api.StatePaused {
		t.Fatalf("the status of a VM paused for a sleep: %+v, %v", st, err)
	}
	if host.Rouse(false) {
		t.Fatal("a held connection resumed a VM the Mac had said nothing about")
	}
	if st := f.d.hub.Status(); st.State != api.StatePaused {
		t.Fatalf("%+v", st)
	}

	send[api.PowerWake](t, src, power.WillPowerOn)
	if st := f.d.hub.Status(); st.State != api.StatePaused {
		t.Fatalf("will-power-on alone resumed the VM: %+v", st)
	}
	time.Sleep(150 * time.Millisecond)
	held.n.Store(2)
	if !host.Rouse(false) {
		t.Fatal("a held connection did not finish the wake the Mac had begun")
	}
	st, err := c.Status(bg)
	if err != nil || st.State != api.StateRunning || st.PauseReason != "" {
		t.Fatalf("after the wake was finished: %+v, %v", st, err)
	}
	log := f.logFile("fevd.log")
	if count(log, " wake: ") != 1 || !strings.Contains(log, "never said it had powered on") {
		t.Errorf("the daemon's log:\n%s", log)
	}
	if n := count(f.logFile("guest.log"), "stepped the wall clock"); n != 1 {
		t.Errorf("the guest's clock was stepped %d times", n)
	}
	if late := send[api.PowerWake](t, src, power.HasPoweredOn); late.Resumed || late.ClockStepped {
		t.Fatalf("a late has-powered-on: %+v", late)
	}
	if vmm := f.logFile("vmm.log"); count(vmm, "control: resumed") != 1 {
		t.Errorf("the VMM's side:\n%s", vmm)
	}

	send[api.PowerSleep](t, src, power.WillSleep)
	if host.Rouse(false) {
		t.Fatal("a wake begun before this sleep was taken for one begun in it")
	}
	send[api.PowerWake](t, src, power.WillPowerOn)
	if st, err := c.Status(bg); err != nil || st.State != api.StateRunning {
		t.Fatalf("the status after will-power-on: %+v, %v", st, err)
	}
}

func TestAStartAskedOfAPausedVMResumesIt(t *testing.T) {
	f, src := asleepVM(t, nil)
	c := f.client()
	var mu sync.Mutex
	var wakes []api.PowerWake
	if _, _, err := c.Subscribe(bg, api.Events{Wake: func(n api.PowerWake) { mu.Lock(); wakes = append(wakes, n); mu.Unlock() }}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := f.client().VMStart(bg, api.StartParams{})
			if err != nil || st.State != api.StateRunning {
				t.Errorf("vm.start on a paused VM: %+v, %v", st, err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		send[api.PowerWake](t, src, power.HasPoweredOn)
	}()
	wg.Wait()
	if st := f.d.hub.Status(); st.State != api.StateRunning {
		t.Fatalf("%+v", st)
	}
	if vmm := f.logFile("vmm.log"); count(vmm, "control: resumed") != 1 {
		t.Errorf("the VMM resumed more or less than once:\n%s", vmm)
	}
	if n := count(f.logFile("guest.log"), "stepped the wall clock"); n != 1 {
		t.Errorf("the guest's clock was stepped %d times", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		resumed := 0
		for _, w := range wakes {
			if w.Resumed {
				resumed++
			}
		}
		mu.Unlock()
		if resumed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscribers were told of %d resumes, want 1: %+v", resumed, wakes)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAMarkSaysTheVMIsPausedForAsLongAsItIs(t *testing.T) {
	f, src := powered(t, nil)
	f.running()
	mark := api.PauseMarkPath(f.layout.Home)
	exists := func(want bool, when string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, err := os.Stat(mark)
			if (err == nil) == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the mark's presence is %v, want %v", when, err == nil, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	exists(false, "running")
	send[api.PowerSleep](t, src, power.WillSleep)
	exists(true, "paused")
	send[api.PowerWake](t, src, power.HasPoweredOn)
	exists(false, "after the wake")
	send[api.PowerSleep](t, src, power.WillSleep)
	exists(true, "paused again")
	if _, err := f.client().VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	exists(false, "stopped")

	g := newFixture(t)
	if err := g.layout.Create(); err != nil {
		t.Fatal(err)
	}
	touch(t, api.PauseMarkPath(g.layout.Home))
	g.start()
	if _, err := os.Stat(api.PauseMarkPath(g.layout.Home)); err == nil {
		t.Error("a daemon started with a stale pause mark and left it")
	}
}

func TestAWillSleepThatMeetsAWakeInProgressStillPausesTheVM(t *testing.T) {
	f, src := asleepVM(t, nil, "resume-ms=300")
	started := make(chan error, 1)
	go func() {
		_, err := f.client().VMStart(bg, api.StartParams{})
		started <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for count(f.logFile("vmm.log"), "control: resume asked for") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the start never asked the VMM to resume")
		}
		time.Sleep(time.Millisecond)
	}
	slept := send[api.PowerSleep](t, src, power.WillSleep)
	if err := <-started; err != nil {
		t.Fatalf("vm.start: %v", err)
	}
	if !slept.Paused || !slept.Changed || slept.TimedOut {
		t.Errorf("will-sleep during a wake: %+v", slept)
	}
	if st := f.d.hub.Status(); st.State != api.StatePaused {
		t.Fatalf("the VM runs through the Mac's sleep: %+v", st)
	}
	if vmm := f.logFile("vmm.log"); count(vmm, "control: paused") != 2 || count(vmm, "control: resumed") != 1 {
		t.Errorf("the VMM's side:\n%s", vmm)
	}
	if woke := send[api.PowerWake](t, src, power.HasPoweredOn); !woke.Resumed {
		t.Fatalf("the wake after it: %+v", woke)
	}
}

func TestAWillSleepDoesNotWaitPastItsBoundForAWakeInProgress(t *testing.T) {
	f, src := asleepVM(t, func(f *fixture) { f.opts.Timing.PauseBound = 150 * time.Millisecond }, "resume-ms=600")
	started := make(chan error, 1)
	go func() {
		_, err := f.client().VMStart(bg, api.StartParams{})
		started <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for count(f.logFile("vmm.log"), "control: resume asked for") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the start never asked the VMM to resume")
		}
		time.Sleep(time.Millisecond)
	}
	began := time.Now()
	slept := send[api.PowerSleep](t, src, power.WillSleep)
	if took := time.Since(began); !slept.TimedOut || slept.Paused || took > 450*time.Millisecond {
		t.Errorf("will-sleep behind a slow wake: %+v after %v", slept, took)
	}
	if err := <-started; err != nil {
		t.Fatalf("vm.start: %v", err)
	}
}

func TestWillPowerOnDuringAPauseStillInHandFinishesTheWake(t *testing.T) {
	was := UnfinishedAfter
	UnfinishedAfter = 700 * time.Millisecond
	defer func() { UnfinishedAfter = was }()
	f, src := powered(t, func(f *fixture) { f.opts.Timing.PauseBound = 100 * time.Millisecond }, "pause-ms=350")
	f.running()
	if slept := send[api.PowerSleep](t, src, power.WillSleep); !slept.TimedOut {
		t.Fatalf("a pause slower than its bound: %+v", slept)
	}
	send[api.PowerWake](t, src, power.WillPowerOn)
	f.until("the late pause", func(st api.Status) bool { return st.State == api.StatePaused })
	deadline := time.Now().Add(3 * time.Second)
	for f.d.hub.Status().State != api.StateRunning {
		if time.Now().After(deadline) {
			t.Fatalf("nothing finished the wake the Mac had begun: %+v\n%s", f.d.hub.Status(), f.logFile("fevd.log"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if log := f.logFile("fevd.log"); !strings.Contains(log, "never said it had powered on") {
		t.Errorf("the daemon's log:\n%s", log)
	}
}

func TestTheGuestSyncsBeforeItIsPausedForASleep(t *testing.T) {
	f, src := powered(t, nil)
	f.running()
	if slept := send[api.PowerSleep](t, src, power.WillSleep); !slept.Paused || !slept.Changed {
		t.Fatalf("will-sleep: %+v", slept)
	}
	send[api.PowerSleep](t, src, power.WillSleep)
	send[api.PowerWake](t, src, power.HasPoweredOn)
	guest := f.logFile("guest.log")
	if n := count(guest, "storage: sync "); n != 1 {
		t.Fatalf("the guest synced %d times for one pause:\n%s", n, guest)
	}
	if log := f.logFile("fevd.log"); strings.Index(log, "sync: the guest has written its data") < 0 ||
		strings.Index(log, "sync: the guest has written its data") > strings.Index(log, "the VMM has paused the VM") {
		t.Errorf("the sync did not come before the pause:\n%s", log)
	}
}

func TestASlowSyncDoesNotKeepTheVMFromBeingPaused(t *testing.T) {
	f, src := powered(t, func(f *fixture) {
		f.opts.Timing.SyncBound, f.opts.Timing.PauseBound = 100*time.Millisecond, time.Second
	}, "sync-ms=5000")
	f.running()
	began := time.Now()
	slept := send[api.PowerSleep](t, src, power.WillSleep)
	if took := time.Since(began); !slept.Paused || slept.TimedOut || took < 100*time.Millisecond || took > 600*time.Millisecond {
		t.Fatalf("will-sleep with a guest that is slow to sync: %+v after %v", slept, took)
	}
	if log := f.logFile("fevd.log"); !strings.Contains(log, "had not written its data before the pause") {
		t.Errorf("the daemon's log:\n%s", log)
	}
}

func TestAWakeDuringTheSyncCallsThePauseOff(t *testing.T) {
	f, src := powered(t, func(f *fixture) {
		f.opts.Timing.SyncBound, f.opts.Timing.PauseBound = 400*time.Millisecond, 100*time.Millisecond
	}, "sync-ms=300")
	f.running()
	if slept := send[api.PowerSleep](t, src, power.WillSleep); !slept.TimedOut {
		t.Fatalf("will-sleep: %+v", slept)
	}
	if woke := send[api.PowerWake](t, src, power.HasPoweredOn); woke.Resumed {
		t.Fatalf("a wake with nothing paused yet: %+v", woke)
	}
	time.Sleep(500 * time.Millisecond)
	if st := f.d.hub.Status(); st.State != api.StateRunning {
		t.Fatalf("%+v", st)
	}
	if vmm := f.logFile("vmm.log"); count(vmm, "control: paused") != 0 {
		t.Errorf("the VMM parked a VM the daemon calls running:\n%s", vmm)
	}
	if _, err := f.client().Status(bg); err != nil {
		t.Fatal(err)
	}
	if slept := send[api.PowerSleep](t, src, power.WillSleep); !slept.TimedOut && !slept.Paused {
		t.Fatalf("the next will-sleep: %+v", slept)
	}
}
