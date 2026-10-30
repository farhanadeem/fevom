package daemon

import (
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"fev/host/daemon/home"
	"fev/host/daemon/launch"
	api "fev/proto/daemon"
)

func TestOfManyStartersOneBecomesTheDaemon(t *testing.T) {
	f := newFixture(t)
	const starters = 20
	type result struct {
		d        *Daemon
		err      error
		ready    launch.Ready
		accepted bool
	}
	results := make([]result, starters)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			opts := f.opts
			w, read := readyPipe(t)
			opts.Ready = w
			results[i].d, results[i].err = Start(opts)
			results[i].ready = read()
			if conn, err := net.Dial("unix", f.layout.Socket()); err == nil {
				results[i].accepted = true
				conn.Close()
			}
		}(i)
	}
	wg.Wait()
	daemons := 0
	for i, r := range results {
		switch {
		case r.err == nil:
			daemons++
			f.d = r.d
			t.Cleanup(r.d.Close)
			if r.ready.Status != launch.StatusReady || r.ready.PID != os.Getpid() || r.ready.Build == "" {
				t.Errorf("the daemon's ready line: %+v", r.ready)
			}
		case r.err == ErrAlreadyRunning:
			if r.ready.Status != launch.StatusAlreadyRunning || r.d != nil {
				t.Errorf("starter %d: ready line %+v", i, r.ready)
			}
		default:
			t.Errorf("starter %d: %v", i, r.err)
		}
		if !r.accepted {
			t.Errorf("starter %d was told to go ahead and could not connect", i)
		}
	}
	if daemons != 1 {
		t.Fatalf("%d of %d starters became the daemon", daemons, starters)
	}
	if st, err := f.client().Status(bg); err != nil || st.State != api.StateStopped {
		t.Errorf("the one daemon: %+v %v", st, err)
	}
}

func TestAStaleSocketDoesNotStopTheNextStart(t *testing.T) {
	f := newFixture(t)
	if err := f.layout.Create(); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", f.layout.Socket())
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	f.layout.WriteInfo(home.Info{PID: 999999, Build: "gone", VMMPID: 999998})
	os.WriteFile(f.layout.StartLock(), nil, 0o600)
	os.WriteFile(f.layout.DaemonLock(), nil, 0o600)
	if _, err := net.Dial("unix", f.layout.Socket()); err == nil {
		t.Fatal("the stale socket accepts; the test's premise is wrong")
	}

	f.start()
	if st, err := f.client().Status(bg); err != nil || st.State != api.StateStopped {
		t.Errorf("status: %+v %v", st, err)
	}
	if info, _ := f.layout.ReadInfo(); info.PID != os.Getpid() || info.VMMPID != 0 {
		t.Errorf("fevd.json was not rewritten: %+v", info)
	}
}

func TestAStarterDuringAShutdownWaitsAndBecomesTheNextDaemon(t *testing.T) {
	f := started(t, []string{"stop-ms=400"})
	c := f.client()
	first, _ := c.VMStart(bg, api.StartParams{})
	old := f.d
	oldPID := f.vmmPID()

	stopping := make(chan struct{})
	go func() {
		old.hub.Wait(bg, func(st api.Status) bool { return st.State == api.StateStopping })
		close(stopping)
	}()
	go old.Close()
	<-stopping

	w, read := readyPipe(t)
	opts := f.opts
	opts.Ready = w
	opts.Autostart = true
	begun := time.Now()
	next, err := Start(opts)
	waited := time.Since(begun)
	if err != nil {
		t.Fatalf("the starter that arrived during shutdown: %v", err)
	}
	t.Cleanup(next.Close)
	if r := read(); r.Status != launch.StatusReady {
		t.Errorf("its ready line: %+v", r)
	}
	if waited < 300*time.Millisecond {
		t.Errorf("the starter waited only %v", waited)
	}
	select {
	case <-old.Done():
	default:
		t.Error("the new daemon started before the old one had finished")
	}
	if alive(oldPID) {
		t.Error("the old daemon's VMM is still running beside the new daemon")
	}
	f.d = next
	second := f.running()
	if second.BootID == first.BootID || !alive(f.vmmPID()) {
		t.Errorf("the new daemon's VM: %+v", second)
	}
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Error("the old daemon's client was left connected")
	}
}

func TestAHomeTooLongIsRefusedBeforeAnythingIsCreated(t *testing.T) {
	dir := t.TempDir() + "/" + strings.Repeat("deep/", 12) + "home"
	layout, _ := home.Resolve(dir, "")
	f := newFixture(t)
	f.opts.Layout = layout
	w, read := readyPipe(t)
	f.opts.Ready = w
	if _, err := Start(f.opts); err == nil || !strings.Contains(err.Error(), "too long for unix sockets") {
		t.Fatalf("Start in %d bytes of home: %v", len(dir), err)
	}
	if r := read(); r.Status != launch.StatusError || !strings.Contains(r.Error, "shorter --home") {
		t.Errorf("ready line: %+v", r)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("the home was created")
	}
}

func TestADaemonStartedOnDemandBootsTheVMAndStaysInsideItsHome(t *testing.T) {
	f := newFixture(t)
	f.opts.Autostart = true
	f.start()
	f.running()
	if _, err := f.client().BootTrace(bg); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.layout.Socket(), f.layout.InfoFile(), f.layout.StartLock(), f.layout.DaemonLock(),
		f.layout.LogDir() + "/fevd.log", f.layout.BootTraceLog(), f.layout.VMDir() + "/host_1025.sock", f.layout.VMDir() + "/vsock_1024.sock"} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%v", err)
		}
		if !strings.HasPrefix(path, f.layout.Home+"/") {
			t.Errorf("%s is outside the home", path)
		}
	}
	if _, err := os.Stat(f.layout.DataDir); err == nil {
		t.Error("a fake VM created a data directory")
	}
	for _, dir := range []string{f.layout.RunDir(), f.layout.VMDir()} {
		if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
			t.Errorf("%s is mode %v", dir, info.Mode().Perm())
		}
	}
}
