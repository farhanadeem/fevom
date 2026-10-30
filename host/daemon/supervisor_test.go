package daemon

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"fev/host/daemon/boottrace"
	"fev/host/daemon/vm"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/version"
)

func TestABootPassesThroughEveryPhaseAndAStopGoesBack(t *testing.T) {
	f := started(t)
	c := f.client()
	h := f.watch(c)
	if h.snapshot.State != api.StateStopped || h.snapshot.Engine != api.EngineStopped || !h.snapshot.Fake || h.snapshot.Seq != 1 {
		t.Fatalf("a new daemon's status: %+v", h.snapshot)
	}

	st, err := c.VMStart(bg, api.StartParams{})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != api.StateRunning || st.Engine != api.EngineReady || st.BootID == "" || st.Phase != "" || st.Error != "" {
		t.Errorf("vm.start returned %+v", st)
	}
	want := []string{"starting/vmm", "starting/kernel", "starting/init", "starting/engine", "running"}
	if got := h.states(t, st.Seq); !reflect.DeepEqual(got, want) {
		t.Errorf("transitions: %v", got)
	}
	pid := f.vmmPID()
	if !alive(pid) {
		t.Fatalf("fevd.json names VMM pid %d, which is not running", pid)
	}

	again, err := c.VMStart(bg, api.StartParams{})
	if err != nil || again.Seq != st.Seq || f.launch.started() != 1 {
		t.Errorf("a second vm.start: %+v %v, %d boots", again, err, f.launch.started())
	}

	stopped, err := c.VMStop(bg, api.StopParams{})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != api.StateStopped || stopped.Engine != api.EngineStopped || stopped.LastExit == nil ||
		!stopped.LastExit.Expected || stopped.LastExit.Code != 0 || len(stopped.LastExit.VMMTail) != 0 {
		t.Errorf("vm.stop returned %+v, last exit %+v", stopped, stopped.LastExit)
	}
	want = append(want, "stopping", "stopped")
	if got := h.states(t, stopped.Seq); !reflect.DeepEqual(got, want) {
		t.Errorf("transitions: %v", got)
	}
	if alive(pid) || f.vmmPID() != 0 {
		t.Errorf("after the stop the VMM (pid %d) is alive=%v and fevd.json says %d", pid, alive(pid), f.vmmPID())
	}
	if log := f.logFile("vmm.log"); strings.Contains(log, "lifeline closed") {
		t.Errorf("an orderly stop used the lifeline:\n%s", log)
	}
	if again, err := c.VMStop(bg, api.StopParams{}); err != nil || again.Seq != stopped.Seq {
		t.Errorf("a second vm.stop: %+v %v", again, err)
	}
}

func TestAForcedStopClosesTheLifelineWithoutAskingTheGuest(t *testing.T) {
	f := started(t, []string{"ignore-shutdown=1"})
	c := f.client()
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	begun := time.Now()
	st, err := c.VMStop(bg, api.StopParams{Force: true})
	if err != nil || st.State != api.StateStopped || !st.LastExit.Expected || st.LastExit.Code != 0 {
		t.Fatalf("forced stop: %+v %v", st, err)
	}
	if took := time.Since(begun); took > 250*time.Millisecond {
		t.Errorf("a forced stop took %v; it should not wait for anything", took)
	}
	if log := f.logFile("vmm.log"); !strings.Contains(log, "lifeline closed; stopping") {
		t.Errorf("the VMM did not stop by its lifeline:\n%s", log)
	}
	if log := f.logFile("fevd.log"); strings.Contains(log, "asking the guest") {
		t.Error("a forced stop asked the guest")
	}
}

func TestAStopEscalatesFromTheGuestToTheLifelineToAKill(t *testing.T) {
	t.Run("a guest that ignores the shutdown is stopped by the lifeline", func(t *testing.T) {
		f := started(t, []string{"ignore-shutdown=1"})
		c := f.client()
		c.VMStart(bg, api.StartParams{})
		begun := time.Now()
		st, err := c.VMStop(bg, api.StopParams{TimeoutMS: 150})
		took := time.Since(begun)
		if err != nil || st.State != api.StateStopped || st.LastExit.Code != 0 || st.LastExit.Signal != "" {
			t.Fatalf("stop: %+v %v", st, err)
		}
		if took < 150*time.Millisecond || took > 400*time.Millisecond {
			t.Errorf("the stop took %v with a 150 ms timeout", took)
		}
	})
	t.Run("a VMM that ignores the lifeline too is killed", func(t *testing.T) {
		f := started(t, []string{"ignore-shutdown=1", "ignore-lifeline=1"})
		c := f.client()
		c.VMStart(bg, api.StartParams{})
		pid := f.vmmPID()
		begun := time.Now()
		st, err := c.VMStop(bg, api.StopParams{TimeoutMS: 100})
		took := time.Since(begun)
		if err != nil || st.State != api.StateStopped || st.LastExit.Code != -1 || st.LastExit.Signal != "SIGKILL" || !st.LastExit.Expected {
			t.Fatalf("stop: %+v, last exit %+v, %v", st, st.LastExit, err)
		}
		if took < 400*time.Millisecond || took > 900*time.Millisecond || alive(pid) {
			t.Errorf("the stop took %v; VMM alive=%v", took, alive(pid))
		}
	})
	t.Run("a second, forced stop hastens a slow one", func(t *testing.T) {
		f := started(t, []string{"ignore-shutdown=1"})
		c := f.client()
		c.VMStart(bg, api.StartParams{})
		st, err := c.VMStop(bg, api.StopParams{TimeoutMS: 60000, NoWait: true})
		if err != nil || st.State != api.StateStopping {
			t.Fatalf("stop with no_wait: %+v %v", st, err)
		}
		begun := time.Now()
		if st, err := c.VMStop(bg, api.StopParams{Force: true}); err != nil || st.State != api.StateStopped || time.Since(begun) > 250*time.Millisecond {
			t.Errorf("forced stop over a slow one: %+v %v after %v", st, err, time.Since(begun))
		}
	})
	t.Run("a shorter timeout replaces a longer one, not the reverse", func(t *testing.T) {
		f := started(t, []string{"ignore-shutdown=1"})
		c := f.client()
		c.VMStart(bg, api.StartParams{})
		c.VMStop(bg, api.StopParams{TimeoutMS: 60000, NoWait: true})
		begun := time.Now()
		c.VMStop(bg, api.StopParams{TimeoutMS: 100, NoWait: true})
		c.VMStop(bg, api.StopParams{TimeoutMS: 60000, NoWait: true})
		f.stopped()
		if took := time.Since(begun); took > 500*time.Millisecond {
			t.Errorf("the stop took %v", took)
		}
	})
	t.Run("a timeout out of range is refused", func(t *testing.T) {
		f := started(t)
		c := f.client()
		for _, ms := range []int{-1, MaxStopTimeoutMS + 1} {
			if _, err := c.VMStop(bg, api.StopParams{TimeoutMS: ms}); code(err) != -32602 {
				t.Errorf("timeout_ms %d: %v", ms, err)
			}
		}
	})
}

func TestACrashIsReportedWithItsEvidenceAndTheVMStartsAgain(t *testing.T) {
	f := started(t)
	c := f.client()
	h := f.watch(c)
	first, _ := c.VMStart(bg, api.StartParams{})
	pid := f.vmmPID()
	syscallKill(t, pid)

	backoff := f.until("backoff", func(st api.Status) bool { return st.Phase == api.PhaseBackoff })
	if backoff.State != api.StateStarting || backoff.RetryInMS != 20 || backoff.Engine != api.EngineStopped || !strings.Contains(backoff.Error, "killed by SIGKILL") {
		t.Errorf("after the crash: %+v", backoff)
	}
	exit := backoff.LastExit
	if exit == nil || exit.Expected || exit.Code != -1 || exit.Signal != "SIGKILL" || exit.At == "" {
		t.Fatalf("last exit: %+v", exit)
	}
	if !contains(exit.VMMTail, "stand-in:") || !contains(exit.ConsoleTail, "fev-init: init: ready") {
		t.Errorf("the tails attached to the crash: vmm %q, console %q", exit.VMMTail, exit.ConsoleTail)
	}

	second := f.until("running again", func(st api.Status) bool { return st.State == api.StateRunning && st.BootID != first.BootID })
	if second.Error != "" || second.LastExit == nil || second.LastExit.Expected {
		t.Errorf("running again: %+v", second)
	}
	if newPID := f.vmmPID(); newPID == pid || !alive(newPID) {
		t.Errorf("the VMM after the restart: pid %d (was %d)", newPID, pid)
	}
	want := []string{"starting/vmm", "starting/kernel", "starting/init", "starting/engine", "running",
		"starting/backoff", "starting/vmm", "starting/kernel", "starting/init", "starting/engine", "running"}
	if got := h.states(t, second.Seq); !reflect.DeepEqual(got, want) {
		t.Errorf("transitions: %v", got)
	}
	if log := f.logFile("fevd.log"); !strings.Contains(log, "the VM failed and will be started again") {
		t.Errorf("the daemon's log does not record the crash:\n%s", log)
	}
}

func syscallKill(t *testing.T, pid int) {
	t.Helper()
	if !alive(pid) {
		t.Fatalf("no VMM to kill (pid %d)", pid)
	}
	if err := killPID(pid); err != nil {
		t.Fatal(err)
	}
}

func TestAfterFiveFailuresInARowTheVMIsLeftStopped(t *testing.T) {
	failFile := t.TempDir() + "/fail"
	f := started(t, []string{"fail-file=" + failFile})
	touch(t, failFile)
	c := f.client()
	h := f.watch(c)

	_, err := c.VMStart(bg, api.StartParams{})
	if code(err) != api.CodeFailed || !strings.Contains(err.Error(), "failed 5 times in a row") || !strings.Contains(err.Error(), "exited with status 1") {
		t.Fatalf("vm.start of a VM that cannot start: %v", err)
	}
	st := f.d.hub.Status()
	if st.State != api.StateStopped || st.Error == "" || st.LastExit == nil || st.LastExit.Code != 1 || st.LastExit.Expected {
		t.Errorf("status: %+v, last exit %+v", st, st.LastExit)
	}
	if !contains(st.LastExit.VMMTail, "failing as told") {
		t.Errorf("VMM tail: %q", st.LastExit.VMMTail)
	}
	if f.launch.started() != 5 {
		t.Errorf("%d boots were tried", f.launch.started())
	}
	var waits []int
	h.states(t, st.Seq)
	h.mu.Lock()
	for _, s := range h.seen {
		if s.Phase == api.PhaseBackoff {
			waits = append(waits, s.RetryInMS)
		}
	}
	h.mu.Unlock()
	if !reflect.DeepEqual(waits, []int{20, 40, 60, 70}) {
		t.Errorf("waits between the attempts: %v", waits)
	}
	if traces := f.traces(); len(traces) != 5 || traces[4].Complete || traces[4].OK {
		t.Errorf("%d boot records", len(traces))
	}

	remove(t, failFile)
	st2, err := c.VMStart(bg, api.StartParams{})
	if err != nil || st2.State != api.StateRunning || st2.Error != "" {
		t.Fatalf("vm.start after the cause was removed: %+v %v", st2, err)
	}
	syscallKill(t, f.vmmPID())
	if b := f.until("backoff", func(st api.Status) bool { return st.Phase == api.PhaseBackoff }); b.RetryInMS != 20 {
		t.Errorf("the first failure after a good start waits %d ms", b.RetryInMS)
	}
	f.running()
}

func TestAStableRunResetsTheFailureCount(t *testing.T) {
	f := newFixture(t)
	f.opts.Timing.StableAfter = 150 * time.Millisecond
	f.start()
	c := f.client()
	c.VMStart(bg, api.StartParams{})
	for _, want := range []int{20, 40} {
		syscallKill(t, f.vmmPID())
		if b := f.until("backoff", func(st api.Status) bool { return st.Phase == api.PhaseBackoff }); b.RetryInMS != want {
			t.Fatalf("wait %d ms, want %d", b.RetryInMS, want)
		}
		f.running()
	}
	time.Sleep(200 * time.Millisecond)
	syscallKill(t, f.vmmPID())
	if b := f.until("backoff", func(st api.Status) bool { return st.Phase == api.PhaseBackoff }); b.RetryInMS != 20 {
		t.Errorf("after a stable run the wait is %d ms", b.RetryInMS)
	}
	f.running()
}

func TestAGuestThatStopsItselfIsRestarted(t *testing.T) {
	f := started(t, []string{"exit-after-ms=30", "exit-code=0"}, nil)
	c := f.client()
	h := f.watch(c)
	c.VMStart(bg, api.StartParams{NoWait: true})
	b := f.until("backoff", func(st api.Status) bool { return st.Phase == api.PhaseBackoff })
	if b.LastExit.Code != 0 || b.LastExit.Expected || !strings.Contains(b.Error, "the guest powered itself off") {
		t.Errorf("after the guest stopped itself: %+v, %+v", b, b.LastExit)
	}
	if !contains(b.LastExit.ConsoleTail, "reboot: Power down") {
		t.Errorf("console tail: %q", b.LastExit.ConsoleTail)
	}
	st := f.running()
	if got := h.states(t, st.Seq); got[len(got)-1] != "running" || f.launch.started() != 2 {
		t.Errorf("transitions %v, %d boots", got, f.launch.started())
	}
}

func TestAVMMFromAnotherBuildIsRefusedAndNotRetried(t *testing.T) {
	f := started(t, []string{"vmm-build=abc1234-dirty.00000000"})
	c := f.client()
	_, err := c.VMStart(bg, api.StartParams{})
	if code(err) != api.CodeFailed {
		t.Fatalf("vm.start: %v", err)
	}
	for _, want := range []string{`"` + version.Build + `"`, `"abc1234-dirty.00000000"`, "the VMM is", "build mismatch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q lacks %s", err, want)
		}
	}
	st := f.stopped()
	if st.Error == "" || f.launch.started() != 1 {
		t.Errorf("status %+v after %d boots", st, f.launch.started())
	}
	if alive(f.vmmPID()) {
		t.Error("the mismatched VMM was left running")
	}
}

func TestAGuestFromAnotherBuildIsRefusedAndNotRetried(t *testing.T) {
	f := started(t, []string{"agent-build=fedcba9"})
	c := f.client()
	_, err := c.VMStart(bg, api.StartParams{})
	if code(err) != api.CodeFailed {
		t.Fatalf("vm.start: %v", err)
	}
	for _, want := range []string{`"` + version.Build + `"`, `"fedcba9"`, "guest agent", "build mismatch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q lacks %s", err, want)
		}
	}
	if st := f.stopped(); f.launch.started() != 1 || st.LastExit == nil || st.LastExit.Expected {
		t.Errorf("status %+v after %d boots", st, f.launch.started())
	}
}

func TestAVMMThatDiesBeforeItsFirstTraceLineIsAnOrdinaryFailure(t *testing.T) {
	f := started(t, []string{"exit-early=1"}, nil)
	c := f.client()
	c.VMStart(bg, api.StartParams{NoWait: true})
	b := f.until("backoff", func(st api.Status) bool { return st.Phase == api.PhaseBackoff })
	if strings.Contains(b.Error, "build") || !strings.Contains(b.Error, "exited with status 1") || !contains(b.LastExit.VMMTail, "before the first trace line") {
		t.Errorf("status: %+v, tail %q", b, b.LastExit.VMMTail)
	}
	if st := f.running(); st.Error != "" {
		t.Errorf("after the retry: %+v", st)
	}
}

func TestAGuestThatNeverConnectsOrDropsItsConnectionIsACrash(t *testing.T) {
	t.Run("never connects", func(t *testing.T) {
		f := newFixture(t, []string{"no-agent=1"}, nil)
		f.opts.Timing.StartDeadline = 150 * time.Millisecond
		f.start()
		c := f.client()
		c.VMStart(bg, api.StartParams{NoWait: true})
		kernel := f.until("the kernel phase", func(st api.Status) bool { return st.Phase == api.PhaseKernel })
		pid := f.vmmPID()
		b := f.until("backoff", func(st api.Status) bool { return st.Phase == api.PhaseBackoff })
		if !strings.Contains(b.Error, "was not ready 150ms after its VMM started") || b.BootID != kernel.BootID {
			t.Errorf("status: %+v", b)
		}
		if alive(pid) || b.LastExit.Code != 0 || b.LastExit.Expected {
			t.Errorf("VMM alive=%v, last exit %+v", alive(pid), b.LastExit)
		}
		f.running()
	})
	t.Run("drops its connection", func(t *testing.T) {
		f := started(t, []string{"drop-agent-ms=40"}, nil)
		c := f.client()
		first, _ := c.VMStart(bg, api.StartParams{})
		b := f.until("backoff", func(st api.Status) bool { return st.Phase == api.PhaseBackoff })
		if !strings.Contains(b.Error, "connection to the guest agent ended") {
			t.Errorf("status: %+v", b)
		}
		if st := f.running(); st.BootID == first.BootID {
			t.Error("the same boot is running again")
		}
	})
}

func TestAVMMThatCannotBeStartedIsAFailureLikeAnyOther(t *testing.T) {
	f := newFixture(t)
	f.launch.Process.Bin = "/nonexistent/fevvm"
	f.start()
	c := f.client()
	_, err := c.VMStart(bg, api.StartParams{})
	if code(err) != api.CodeFailed || !strings.Contains(err.Error(), "the VM could not be started") || !strings.Contains(err.Error(), "/nonexistent/fevvm") {
		t.Fatalf("vm.start: %v", err)
	}
	if st := f.stopped(); st.LastExit != nil || f.launch.started() != 5 {
		t.Errorf("status %+v after %d attempts", st, f.launch.started())
	}
}

func TestAStopDuringAStartOrABackoff(t *testing.T) {
	t.Run("while the guest boots", func(t *testing.T) {
		f := started(t, []string{"kernel-ms=5000"})
		c := f.client()
		failed := make(chan error, 1)
		go func() {
			_, err := f.client().VMStart(bg, api.StartParams{})
			failed <- err
		}()
		f.until("the kernel phase", func(st api.Status) bool { return st.Phase == api.PhaseKernel })
		pid := f.vmmPID()
		st, err := c.VMStop(bg, api.StopParams{})
		if err != nil || st.State != api.StateStopped || !st.LastExit.Expected || alive(pid) {
			t.Errorf("stop during a start: %+v %v", st, err)
		}
		if err := <-failed; code(err) != api.CodeFailed || !strings.Contains(err.Error(), "stopped before it was running") {
			t.Errorf("the blocked vm.start returned %v", err)
		}
		if f.launch.started() != 1 {
			t.Errorf("%d boots", f.launch.started())
		}
	})
	t.Run("while waiting to retry", func(t *testing.T) {
		failFile := t.TempDir() + "/fail"
		touch(t, failFile)
		f := newFixture(t, []string{"fail-file=" + failFile})
		f.opts.Timing.Backoff = []time.Duration{time.Hour}
		f.opts.Timing.MaxBackoff = time.Hour
		f.start()
		c := f.client()
		c.VMStart(bg, api.StartParams{NoWait: true})
		f.until("backoff", func(st api.Status) bool { return st.Phase == api.PhaseBackoff })
		st, err := c.VMStop(bg, api.StopParams{})
		if err != nil || st.State != api.StateStopped || st.Phase != "" || st.RetryInMS != 0 || st.Error != "" {
			t.Errorf("stop during a backoff: %+v %v", st, err)
		}
		remove(t, failFile)
		if st, err := c.VMStart(bg, api.StartParams{}); err != nil || st.State != api.StateRunning || f.launch.started() != 2 {
			t.Errorf("start after that: %+v %v, %d boots", st, err, f.launch.started())
		}
	})
}

func TestARestartAppliesPendingConfiguration(t *testing.T) {
	f := started(t)
	c := f.client()
	h := f.watch(c)
	first, _ := c.VMStart(bg, api.StartParams{})
	if spec := f.launch.spec(0); spec.CPUs != 10 || spec.MemMiB != 8192 {
		t.Fatalf("the first boot's spec: %+v", spec)
	}
	changed, err := c.ConfigSet(bg, map[string]any{api.KeyCPU: 4, api.KeyMemoryMiB: 2048})
	if err != nil || len(changed) != 2 || !changed[0].Pending || !changed[1].Pending {
		t.Fatalf("config.set: %+v %v", changed, err)
	}
	st, _ := c.Status(bg)
	if !reflect.DeepEqual(st.PendingRestart, []string{api.KeyCPU, api.KeyMemoryMiB}) || st.State != api.StateRunning || st.Seq != first.Seq+1 {
		t.Errorf("status after the set: %+v", st)
	}

	second, err := c.VMRestart(bg, api.StartParams{})
	if err != nil || second.State != api.StateRunning || second.BootID == first.BootID || len(second.PendingRestart) != 0 {
		t.Fatalf("vm.restart: %+v %v", second, err)
	}
	spec := f.launch.spec(1)
	if spec.CPUs != 4 || spec.MemMiB != 2048 {
		t.Errorf("the second boot's spec: %+v", spec)
	}
	if log := f.logFile("vmm.log"); !strings.Contains(log, "4 vCPUs, 2048 MiB") {
		t.Errorf("the VMM was not given the new values:\n%s", log)
	}
	got := h.states(t, second.Seq)
	want := []string{"starting/vmm", "starting/kernel", "starting/init", "starting/engine", "running",
		"stopping", "starting/vmm", "starting/kernel", "starting/init", "starting/engine", "running"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("transitions: %v", got)
	}
	if tr, err := c.BootTrace(bg); err != nil || tr.CPUs != 4 || tr.MemMiB != 2048 || tr.BootID != second.BootID {
		t.Errorf("boot.trace: %+v %v", tr, err)
	}

	c.VMStop(bg, api.StopParams{})
	if st, err := c.VMRestart(bg, api.StartParams{NoWait: true}); err != nil || st.State != api.StateStarting {
		t.Errorf("vm.restart of a stopped VM, no_wait: %+v %v", st, err)
	}
	f.running()
	if st, err := c.VMRestart(bg, api.StartParams{NoWait: true}); err != nil || st.State != api.StateStopping {
		t.Errorf("vm.restart of a running VM, no_wait: %+v %v", st, err)
	}
	f.until("running after the restart", func(st api.Status) bool { return st.State == api.StateRunning && st.BootID != second.BootID })
}

func TestAStartDuringAStopBootsAgainAfterIt(t *testing.T) {
	f := started(t, []string{"stop-ms=150"})
	c := f.client()
	first, _ := c.VMStart(bg, api.StartParams{})
	c.VMStop(bg, api.StopParams{NoWait: true})
	st, err := c.VMStart(bg, api.StartParams{})
	if err != nil || st.State != api.StateRunning || st.BootID == first.BootID {
		t.Fatalf("vm.start during a stop: %+v %v", st, err)
	}
	c.VMStop(bg, api.StopParams{NoWait: true})
	c.VMStart(bg, api.StartParams{NoWait: true})
	if st, err := c.VMStop(bg, api.StopParams{}); err != nil || st.State != api.StateStopped {
		t.Fatalf("stop: %+v %v", st, err)
	}
	time.Sleep(50 * time.Millisecond)
	if st := f.d.hub.Status(); st.State != api.StateStopped || f.launch.started() != 2 {
		t.Errorf("after stop, start, stop: %+v, %d boots", st, f.launch.started())
	}
}

func TestTheEngineHasItsOwnState(t *testing.T) {
	t.Run("an engine that fails leaves the VM up and the start failed", func(t *testing.T) {
		f := started(t, []string{"engine-error=no space left on device"})
		c := f.client()
		_, err := c.VMStart(bg, api.StartParams{})
		if code(err) != api.CodeFailed || !strings.Contains(err.Error(), "the Docker engine failed: no space left on device") {
			t.Fatalf("vm.start: %v", err)
		}
		st := f.d.hub.Status()
		if st.State != api.StateStarting || st.Phase != api.PhaseEngine || st.Engine != api.EngineFailed || !alive(f.vmmPID()) {
			t.Errorf("status: %+v", st)
		}
		if st, err := c.VMStop(bg, api.StopParams{}); err != nil || st.State != api.StateStopped || st.Error != "" {
			t.Errorf("stop: %+v %v", st, err)
		}
	})
	t.Run("an engine restarted alone changes only its field", func(t *testing.T) {
		f := started(t)
		c := f.client()
		h := f.watch(c)
		running, _ := c.VMStart(bg, api.StartParams{})
		machine, ok := f.d.sup.VM()
		if !ok || machine.BootID() != running.BootID || !strings.HasSuffix(machine.VsockPath(vm.PortDocker), "/run/vm/vsock_2375.sock") {
			t.Fatalf("the running VM as a module sees it: %v %v", machine, ok)
		}
		if _, err := machine.Agent().EngineRestart(bg, agent.EngineStopParams{}); err != nil {
			t.Fatal(err)
		}
		last := f.until("the engine ready again", func(st api.Status) bool { return st.Engine == api.EngineReady && st.Seq > running.Seq })
		h.states(t, last.Seq)
		var engine []string
		h.mu.Lock()
		for _, st := range h.seen {
			if st.Seq > running.Seq {
				if st.State != api.StateRunning {
					t.Errorf("the VM's state followed the engine: %+v", st)
				}
				engine = append(engine, st.Engine)
			}
		}
		h.mu.Unlock()
		if !reflect.DeepEqual(engine, []string{api.EngineStopped, api.EngineStarting, api.EngineReady}) {
			t.Errorf("engine states: %v", engine)
		}
		tr, _ := c.BootTrace(bg)
		for _, s := range tr.Stages {
			if s.Stage == 4 && s.MS > 1000 {
				t.Errorf("stage 4 moved with the engine's restart: %+v", s)
			}
		}
	})
}

func TestEveryBootLeavesARecordAndAStageOverBudgetIsNamed(t *testing.T) {
	f := newFixture(t, nil, []string{"kernel-ms=380"})
	f.opts.Autostart = true
	f.opts.MainNS = boottrace.Now() - 3_000_000
	f.opts.OriginNS = f.opts.MainNS - 4_000_000
	f.start()
	c := f.client()
	first := f.running()

	tr, err := c.BootTrace(bg)
	if err != nil {
		t.Fatal(err)
	}
	if tr.BootID != first.BootID || tr.Origin != "client" || tr.OriginNS != f.opts.OriginNS || tr.Build != version.Build || tr.Clock != "uptime_raw" || tr.CPUs != 10 {
		t.Errorf("head: %+v", tr)
	}
	at := map[string]float64{}
	for _, m := range tr.Marks {
		at[m.Name] = m.MS
		if m.Approx != (m.Name == boottrace.KernelRunInit) {
			t.Errorf("mark %s: approx=%v", m.Name, m.Approx)
		}
	}
	order := []string{boottrace.DaemonMain, boottrace.DaemonAPIListening, boottrace.VMMSpawn, "vmm.main", boottrace.VMMGuestEntry,
		"guest.console_first", boottrace.InitMain, boottrace.InitReady, boottrace.EngineStart, boottrace.EngineReady}
	prev := 0.0
	for _, name := range order {
		ms, ok := at[name]
		if !ok || ms < prev {
			t.Errorf("mark %s at %v ms (present=%v), after one at %v ms", name, ms, ok, prev)
		}
		prev = ms
	}
	if at[boottrace.DaemonMain] != 4 || at[boottrace.AgentConnected] < at[boottrace.VMMGuestEntry] || at[boottrace.KernelRunInit] > at[boottrace.InitMain] {
		t.Errorf("daemon.main %v, agent.connected %v, kernel.run_init %v", at[boottrace.DaemonMain], at[boottrace.AgentConnected], at[boottrace.KernelRunInit])
	}
	if tr.GuestErrorMS <= 0 || tr.GuestErrorMS > 50 || tr.GuestOffsetMS <= 0 {
		t.Errorf("guest clock: offset %v ms, error %v ms", tr.GuestOffsetMS, tr.GuestErrorMS)
	}
	if len(tr.Stages) != 5 || tr.Complete || tr.OK || len(boottrace.Over(*tr)) != 0 {
		t.Errorf("stages: %+v complete=%v ok=%v", tr.Stages, tr.Complete, tr.OK)
	}
	for i, s := range tr.Stages {
		if s.Stage != i || s.MS < 0 || s.Over || s.BudgetMS != boottrace.Budget[i].BudgetMS {
			t.Errorf("stage %d: %+v", i, s)
		}
	}

	second, err := c.VMRestart(bg, api.StartParams{})
	if err != nil {
		t.Fatal(err)
	}
	tr2, _ := c.BootTrace(bg)
	over := boottrace.Over(*tr2)
	if len(over) != 1 || over[0].Stage != 2 || over[0].MS < 380 || over[0].BudgetMS != 350 || tr2.OK {
		t.Errorf("over budget: %+v", over)
	}
	if tr2.Origin != "daemon" || tr2.BootID != second.BootID || tr2.Marks[0].Name != boottrace.VMMSpawn {
		t.Errorf("second boot: origin %s, first mark %+v", tr2.Origin, tr2.Marks[0])
	}
	log := f.logFile("fevd.log")
	if !strings.Contains(log, " daemon 4 boot stage 2 (vmm.guest_entry to init.main) took 38") || !strings.Contains(log, "over its budget of 350 ms boot="+second.BootID) {
		t.Errorf("no warning naming stage 2 in the daemon's log:\n%s", log)
	}
	if strings.Contains(log, "boot stage 0") || strings.Contains(log, "boot stage 4") {
		t.Errorf("a stage within budget was warned about:\n%s", log)
	}

	traces := f.traces()
	if len(traces) != 2 || traces[0].BootID != first.BootID || traces[1].BootID != second.BootID || !reflect.DeepEqual(traces[1], *tr2) {
		t.Errorf("the boot trace log has %d lines", len(traces))
	}
	syscallKill(t, f.vmmPID())
	f.until("running again", func(st api.Status) bool { return st.State == api.StateRunning && st.BootID != second.BootID })
	n := 0
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if n = len(f.traces()); n >= 3 || time.Now().After(deadline) {
			break
		}
	}
	if n != 3 {
		t.Errorf("after a third boot the log has %d lines", n)
	}
}

func TestThereIsNoTraceBeforeTheFirstBoot(t *testing.T) {
	f := started(t)
	c := f.client()
	if _, err := c.BootTrace(bg); code(err) != api.CodeFailed {
		t.Errorf("boot.trace before any boot: %v", err)
	}
	slow := started(t, []string{"kernel-ms=5000"})
	sc := slow.client()
	sc.VMStart(bg, api.StartParams{NoWait: true})
	slow.until("the kernel phase", func(st api.Status) bool { return st.Phase == api.PhaseKernel })
	tr, err := sc.BootTrace(bg)
	if err != nil || tr.Complete || len(tr.Stages) != 2 || len(slow.traces()) != 0 {
		t.Errorf("boot.trace during a boot: %+v %v", tr, err)
	}
	sc.VMStop(bg, api.StopParams{})
	if traces := slow.traces(); len(traces) != 1 || traces[0].Complete || traces[0].BootID != tr.BootID {
		t.Errorf("after stopping a boot part way the log has %d lines", len(traces))
	}
}

func TestABlockedStopReturnsThoughAnotherClientStartsTheVMAgain(t *testing.T) {
	f := started(t, []string{"stop-ms=200"})
	a, b := f.client(), f.client()
	first, err := a.VMStart(bg, api.StartParams{})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		st  *api.Status
		err error
	}
	stopped := make(chan result, 1)
	go func() {
		st, err := a.VMStop(bg, api.StopParams{})
		stopped <- result{st, err}
	}()
	f.until("stopping", func(st api.Status) bool { return st.State == api.StateStopping })
	if st, err := b.VMStart(bg, api.StartParams{}); err != nil || st.State != api.StateRunning || st.BootID == first.BootID {
		t.Fatalf("vm.start during the stop: %+v %v", st, err)
	}
	select {
	case r := <-stopped:
		if r.err != nil || (r.st.State != api.StateStopped && r.st.BootID == first.BootID) {
			t.Errorf("vm.stop: %+v %v", r.st, r.err)
		}
		if r.st != nil && (r.st.LastExit == nil || !r.st.LastExit.Expected) {
			t.Errorf("vm.stop's status does not carry the exit it caused: %+v", r.st)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("vm.stop never returned: the VM it stopped has gone and another is running")
	}
}
