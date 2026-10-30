package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"fev/proto/agent"
	api "fev/proto/daemon"
)

const lateVerdict = "the Docker engine was not ready "

func (f *fixture) steadyFailure(d time.Duration) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(bg, d)
	defer cancel()
	st, err := f.d.hub.Wait(ctx, func(st api.Status) bool {
		return st.Engine != api.EngineFailed || !strings.Contains(st.Error, lateVerdict)
	})
	if err == nil {
		f.t.Fatalf("the deadline's verdict was erased by the engine's next start: %+v", st)
	}
}

func crashLine(text string) bool {
	for _, line := range []string{"unexpected fault address 0x", "fatal error: fault", "[signal SIGBUS: bus error"} {
		if strings.Contains(text, "; the engine's last error: "+line) {
			return true
		}
	}
	return false
}

func TestAnEngineThatDiesAtEveryStartStaysFailedWithItsLastError(t *testing.T) {
	f := newFixture(t, []string{"engine=flap", "engine-ms=40"})
	f.opts.Timing.EngineDeadline = 200 * time.Millisecond
	f.start()
	c := f.client()
	_, err := c.VMStart(bg, api.StartParams{})
	if err == nil || !strings.Contains(err.Error(), lateVerdict+"200ms after it was started") {
		t.Fatalf("vm.start with an engine that never comes up: %v", err)
	}
	starts := strings.Count(f.logFile("guest.log"), "engine 6 Starting up")
	f.steadyFailure(800 * time.Millisecond)
	if more := strings.Count(f.logFile("guest.log"), "engine 6 Starting up"); more < starts+10 {
		t.Fatalf("the stand-in's engine was not started again and again: %d starts, then %d", starts, more)
	}
	st := f.d.hub.Status()
	if st.State != api.StateStarting || st.Phase != api.PhaseEngine {
		t.Fatalf("the VM: %+v", st)
	}
	if !crashLine(st.Error) {
		t.Errorf("the verdict does not carry the engine's last error: %q", st.Error)
	}
	if strings.Contains(st.Error, "fev reset") {
		t.Errorf("fev reset named with no recovery held: %q", st.Error)
	}
	if _, err := c.VMStart(bg, api.StartParams{}); err == nil || !crashLine(err.Error()) {
		t.Fatalf("a second vm.start: %v", err)
	}

	vm, ok := f.d.sup.VM()
	if !ok {
		t.Fatal("no VM to restart the engine of")
	}
	if _, err := vm.Agent().EngineRestart(bg, agent.EngineStopParams{}); err != nil {
		t.Fatal(err)
	}
	f.until("the verdict cleared by the restart", func(st api.Status) bool {
		return st.Engine == api.EngineStarting && st.Error == ""
	})
	f.until("the deadline's verdict on the restarted engine", func(st api.Status) bool {
		return st.Engine == api.EngineFailed && strings.Contains(st.Error, lateVerdict)
	})
	f.steadyFailure(300 * time.Millisecond)
}

func TestAnEngineThatNeverComesUpOnARecoveredDiskNamesFevomReset(t *testing.T) {
	f := newFixture(t, []string{"mount-rung=3", "engine=flap", "engine-ms=40"})
	f.opts.Timing.EngineDeadline = 300 * time.Millisecond
	m := &DataImage{Path: f.layout.DataImage(), Margin: 1 << 30}
	m.free = func(string) (uint64, error) { return 10 << 30, nil }
	m.blocks = func(string) (uint64, error) { return 2 << 30, nil }
	f.launch.Process.DataImage = m.Path
	f.opts.Modules = []Module{m}
	f.start()
	if _, err := f.client().VMStart(bg, api.StartParams{}); err == nil {
		t.Fatal("the start succeeded with an engine that never comes up")
	}
	st := f.until("fev reset in the verdict", func(st api.Status) bool {
		return st.Engine == api.EngineFailed && strings.Contains(st.Error, "`fev reset`")
	})
	if !crashLine(st.Error) {
		t.Errorf("the verdict does not carry the engine's last error: %q", st.Error)
	}
	for _, want := range []string{lateVerdict, "recovered when it was mounted", "every image, container and volume"} {
		if !strings.Contains(st.Error, want) {
			t.Errorf("the verdict does not say %q: %q", want, st.Error)
		}
	}
	f.steadyFailure(300 * time.Millisecond)
}
