package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func TestAStopAskedForWhileTheDaemonWasUpStandsAtTheNextStartOnDemand(t *testing.T) {
	f := started(t)
	host := moduleHost{f.d}
	if st := f.d.hub.Status(); st.StopRequested {
		t.Fatalf("%+v", st)
	}
	touch(t, api.StopMarkPath(f.layout.Home))
	if err := host.StartOnDemand(); err != nil {
		t.Fatal(err)
	}
	st := f.d.hub.Status()
	if st.State != api.StateStopped || !st.StopRequested || f.launch.started() != 0 {
		t.Fatalf("a start on demand over a stop mark: %+v, %d boots", st, f.launch.started())
	}
	if log := f.logFile("fevd.log"); !strings.Contains(log, "a stop was asked for") {
		t.Errorf("the daemon's log does not say why nothing started:\n%s", log)
	}
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	if err := (moduleHost{f.d}).StopVM(bg); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	if err := host.StartOnDemand(); err != nil {
		t.Fatal(err)
	}
	f.running()
	if f.launch.started() != 2 {
		t.Fatalf("%d boots", f.launch.started())
	}
}

func TestADaemonStartedForADockerCommandStartsNothingOverAStopMark(t *testing.T) {
	f := newFixture(t)
	if err := f.layout.Create(); err != nil {
		t.Fatal(err)
	}
	mark := api.StopMarkPath(f.layout.Home)
	touch(t, mark)
	f.opts.Autostart, f.opts.OnDemand = true, true
	f.start()
	select {
	case <-f.d.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("a daemon started on demand over a stop mark stayed: %+v", f.d.hub.Status())
	}
	if f.launch.started() != 0 {
		t.Fatalf("%d boots", f.launch.started())
	}
	if _, err := os.Stat(mark); err != nil {
		t.Errorf("the mark is gone: %v", err)
	}
	f.d.release()

	g := newFixture(t)
	g.opts.Autostart, g.opts.OnDemand = true, true
	g.start()
	g.running()
	h := newFixture(t)
	if err := h.layout.Create(); err != nil {
		t.Fatal(err)
	}
	touch(t, api.StopMarkPath(h.layout.Home))
	h.opts.Autostart = true
	h.start()
	h.running()
}

type counter struct {
	stub
	n     int
	known bool
}

func (c *counter) RunningContainers() (int, bool) { return c.n, c.known }

func TestAClientRefusedForItsBuildIsToldWhatIsRunning(t *testing.T) {
	count := &counter{}
	f := newFixture(t)
	f.opts.Modules = []Module{count}
	f.start()
	refused := func() api.BuildMismatch {
		t.Helper()
		err := jsonrpc.NewConn(f.conn(), nil).Call(bg, api.MethodHello, api.HelloParams{APIVersion: api.APIVersion, Build: "0ld8u1d", Client: "stale"}, nil)
		data, ok := api.AsBuildMismatch(err)
		if !ok {
			t.Fatalf("hello from another build: %v", err)
		}
		return data
	}
	got := refused()
	if got.PID != os.Getpid() || got.State != api.StateStopped || got.RunningContainers != nil {
		t.Fatalf("with the VM stopped: %+v", got)
	}
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	if got := refused(); got.State != api.StateRunning || got.RunningContainers != nil {
		t.Fatalf("running, the containers not known: %+v", got)
	}
	count.n, count.known = 3, true
	if got := refused(); got.RunningContainers == nil || *got.RunningContainers != 3 {
		t.Fatalf("running with three containers: %+v", got)
	}
}

func TestAStartThatFailsInTheKernelIsRetriedWithoutQuiet(t *testing.T) {
	f := newFixture(t, []string{"kernel-ms=60000"}, []string{"kernel-ms=60000"}, nil)
	timing := quick()
	timing.StartDeadline = 150 * time.Millisecond
	f.opts.Timing = timing
	f.start()
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	f.running()
	if n := f.launch.started(); n != 3 {
		t.Fatalf("%d boots, want two that failed in the kernel and one that ran", n)
	}
	if a, b, c := f.launch.spec(0).Verbose, f.launch.spec(1).Verbose, f.launch.spec(2).Verbose; a || !b || !c {
		t.Fatalf("verbose kernels: %v %v %v; want the first quiet and every retry after a kernel failure not", a, b, c)
	}
	if log := f.logFile("fevd.log"); !strings.Contains(log, "without quiet") {
		t.Errorf("the daemon's log does not say the retry is verbose:\n%s", log)
	}
	if _, err := f.client().VMRestart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	if f.launch.spec(3).Verbose {
		t.Error("a boot after one that ran was verbose")
	}

	g := newFixture(t, []string{"exit-early=3"}, nil)
	g.start()
	if _, err := g.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	g.running()
	if g.launch.spec(1).Verbose {
		t.Error("a VMM that failed before the guest ran was retried with a verbose kernel")
	}
}
