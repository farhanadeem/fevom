package vm_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"fev/host/daemon/vm"
	"fev/host/daemon/vm/standin"
	"fev/proto/agent"
	"fev/proto/version"
	"fev/proto/vmmctl"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == standin.Arg {
		os.Exit(standin.Main(os.Args[2:]))
	}
	if len(os.Args) > 2 && os.Args[1] == "--a-daemon-that-dies" {
		p := &vm.Process{Bin: os.Args[0], Prefix: []string{standin.Arg}, Dir: os.Args[2], StandIn: true, Extra: []string{"--standin", "ignore-lifeline=1"}}
		r := newRecorder()
		inst, err := p.Start(vm.Spec{CPUs: 1, MemMiB: 512}, r)
		if err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		<-r.conns
		os.Stdout.WriteString(strconv.Itoa(inst.PID()) + "\n")
		os.Exit(0)
	}
	os.Setenv("GORACE", "atexit_sleep_ms=0")
	os.Exit(m.Run())
}

type recorder struct {
	mu        sync.Mutex
	marks     []vm.Mark
	vmm       []string
	console   []string
	conns     chan net.Conn
	exited    chan vm.Exit
	afterExit int
	gone      bool
}

func newRecorder() *recorder {
	return &recorder{conns: make(chan net.Conn, 4), exited: make(chan vm.Exit, 1)}
}

func (r *recorder) note() {
	if r.gone {
		r.afterExit++
	}
}
func (r *recorder) Mark(m vm.Mark) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.note()
	r.marks = append(r.marks, m)
}
func (r *recorder) VMMLine(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.note()
	r.vmm = append(r.vmm, s)
}
func (r *recorder) ConsoleLine(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.note()
	r.console = append(r.console, s)
}
func (r *recorder) AgentConn(c net.Conn) { r.conns <- c }
func (r *recorder) Exited(e vm.Exit) {
	r.mu.Lock()
	r.gone = true
	r.mu.Unlock()
	r.exited <- e
}

func (r *recorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for _, m := range r.marks {
		names = append(names, m.Name)
	}
	return names
}

func standIn(t *testing.T, instructions ...string) *vm.Process {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := &vm.Process{Bin: os.Args[0], Prefix: []string{standin.Arg}, Dir: filepath.Join(dir, "vm"), StandIn: true}
	for _, i := range instructions {
		p.Extra = append(p.Extra, "--standin", i)
	}
	return p
}

func wait(t *testing.T, r *recorder) vm.Exit {
	t.Helper()
	select {
	case e := <-r.exited:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("the stand-in did not exit")
		return vm.Exit{}
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestAChildIsStartedOnItsPipesAndItsGuestConnects(t *testing.T) {
	p := standIn(t)
	os.MkdirAll(p.Dir, 0o755)
	os.WriteFile(filepath.Join(p.Dir, "vsock_1024.sock"), nil, 0o644)
	r := newRecorder()
	inst, err := p.Start(vm.Spec{BootID: "b1", CPUs: 3, MemMiB: 2048}, r)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Fake() || inst.PID() <= 0 || !alive(inst.PID()) {
		t.Fatalf("instance: fake=%v pid=%d", p.Fake(), inst.PID())
	}
	if info, _ := os.Stat(p.Dir); info.Mode().Perm() != 0o700 {
		t.Errorf("the VM's socket directory is mode %v", info.Mode().Perm())
	}
	var conn net.Conn
	select {
	case conn = <-r.conns:
	case <-time.After(10 * time.Second):
		t.Fatal("the guest never connected to host port 1025")
	}
	client, hello, err := agent.NewClient(t.Context(), conn, "test")
	if err != nil {
		t.Fatal(err)
	}
	if hello.AgentVersion != version.Build {
		t.Errorf("hello: %+v", hello)
	}
	if _, err := client.BootTrace(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if inst.VsockPath(vm.PortAgent) != filepath.Join(p.Dir, "vsock_1024.sock") {
		t.Errorf("vsock path: %s", inst.VsockPath(vm.PortAgent))
	}
	direct, err := net.Dial("unix", inst.VsockPath(vm.PortAgent))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.NewClient(t.Context(), direct, "test"); err != nil {
		t.Errorf("the guest's agent port: %v", err)
	}
	docker, err := net.Dial("unix", inst.VsockPath(vm.PortDocker))
	if err != nil {
		t.Fatal(err)
	}
	docker.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := docker.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Errorf("the Docker port answered: n=%d err=%v", n, err)
	}

	inst.Stop()
	exit := wait(t, r)
	if exit.Code != 0 || exit.Signal != "" || exit.At.IsZero() {
		t.Errorf("exit: %+v", exit)
	}
	names := r.names()
	want := []string{"vmm.main", "vmm.memory_mapped", "vmm.devices_ready", "vmm.kernel_loaded", "vmm.guest_entry", "guest.console_first", "vmm.exit"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("marks: %v", names)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.marks[0].Build != version.Build || r.marks[len(r.marks)-1].Outcome != "stopped" {
		t.Errorf("first and last marks: %+v %+v", r.marks[0], r.marks[len(r.marks)-1])
	}
	for i := 1; i < len(r.marks); i++ {
		if r.marks[i].NS < r.marks[i-1].NS {
			t.Errorf("marks go back in time: %+v", r.marks)
		}
	}
	if len(r.vmm) < 2 || !strings.Contains(r.vmm[0], "3 vCPUs, 2048 MiB") || !strings.Contains(r.vmm[len(r.vmm)-1], "lifeline closed") {
		t.Errorf("VMM log: %q", r.vmm)
	}
	if len(r.console) != 1 || !strings.Contains(r.console[0], "ms] fev-init: init: ready") {
		t.Errorf("console: %q", r.console)
	}
	if r.afterExit != 0 {
		t.Errorf("%d events were delivered after Exited", r.afterExit)
	}
	if _, err := net.Dial("unix", p.AgentSocket()); err == nil {
		t.Error("host port 1025 still accepts after the VM has gone")
	}
}

func TestTheStandInTellsOfAResetAndAnswersAKill(t *testing.T) {
	boot := func(t *testing.T) (*recorder, vm.Instance, *agent.Client) {
		t.Helper()
		p := standIn(t)
		p.Control = true
		r := newRecorder()
		inst, err := p.Start(vm.Spec{BootID: "b1", CPUs: 2, MemMiB: 1024}, r)
		if err != nil {
			t.Fatal(err)
		}
		if inst.Control() == nil {
			t.Fatal("the stand-in was started without a control socket")
		}
		select {
		case conn := <-r.conns:
			client, _, err := agent.NewClient(t.Context(), conn, "test")
			if err != nil {
				t.Fatal(err)
			}
			return r, inst, client
		case <-time.After(10 * time.Second):
			t.Fatal("the guest never connected")
		}
		return nil, nil, nil
	}

	r, inst, guest := boot(t)
	resets := make(chan vmmctl.Reset, 4)
	inst.Control().OnReset(func(n vmmctl.Reset) { resets <- n })
	before, err := inst.Control().Stats(t.Context())
	if err != nil || before.Resets != 0 {
		t.Fatalf("stats before any reset: %+v %v", before, err)
	}
	if err := guest.Shutdown(t.Context(), agent.ShutdownParams{Reboot: true}, nil); err != nil {
		t.Fatal(err)
	}
	exit := wait(t, r)
	select {
	case n := <-resets:
		if n.Count != 1 || n.AtNS == 0 {
			t.Errorf("the reset told of: %+v", n)
		}
	default:
		t.Error("the guest reset itself and the control socket said nothing")
	}
	r.mu.Lock()
	if last := r.marks[len(r.marks)-1]; exit.Code != 0 || last.Outcome != "reset" {
		t.Errorf("exit %+v, last mark %+v", exit, last)
	}
	r.mu.Unlock()

	r, inst, _ = boot(t)
	if err := inst.Control().Kill(t.Context()); err != nil {
		t.Errorf("kill: %v", err)
	}
	exit = wait(t, r)
	if exit.Signal != "SIGKILL" {
		t.Errorf("after kill on the control socket: %+v", exit)
	}
	for _, name := range r.names() {
		if name == "vmm.exit" {
			t.Error("a killed VMM wrote an exit mark")
		}
	}
}

func TestAKilledChildIsReportedWithItsSignal(t *testing.T) {
	p := standIn(t, "ignore-lifeline=1")
	r := newRecorder()
	inst, err := p.Start(vm.Spec{CPUs: 1, MemMiB: 512}, r)
	if err != nil {
		t.Fatal(err)
	}
	<-r.conns
	inst.Stop()
	inst.Stop()
	select {
	case e := <-r.exited:
		t.Fatalf("a stand-in told to ignore its lifeline exited: %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
	inst.Kill()
	if exit := wait(t, r); exit.Code != -1 || exit.Signal != "SIGKILL" {
		t.Errorf("exit: %+v", exit)
	}
}

func TestAChildThatFailsIsReportedWithItsStatusAndItsLastWords(t *testing.T) {
	p := standIn(t, "exit-early=3")
	r := newRecorder()
	if _, err := p.Start(vm.Spec{CPUs: 1, MemMiB: 512}, r); err != nil {
		t.Fatal(err)
	}
	if exit := wait(t, r); exit.Code != 3 || exit.Signal != "" {
		t.Errorf("exit: %+v", exit)
	}
	r.mu.Lock()
	if len(r.marks) != 0 || len(r.vmm) != 1 || !strings.Contains(r.vmm[0], "status 3") {
		t.Errorf("marks %v, log %q", r.marks, r.vmm)
	}
	r.mu.Unlock()

	p = standIn(t)
	p.Bin = "/nonexistent/fevvm"
	if _, err := p.Start(vm.Spec{CPUs: 1, MemMiB: 512}, newRecorder()); err == nil || !strings.Contains(err.Error(), "/nonexistent/fevvm") {
		t.Errorf("err: %v", err)
	}
	if _, err := net.Dial("unix", p.AgentSocket()); err == nil {
		t.Error("a listener was left behind by a start that failed")
	}
}

func TestNoOtherChildInheritsTheLifeline(t *testing.T) {
	p := standIn(t)
	r := newRecorder()
	inst, err := p.Start(vm.Spec{CPUs: 1, MemMiB: 512}, r)
	if err != nil {
		t.Fatal(err)
	}
	<-r.conns
	bystander := exec.Command("/bin/sleep", "60")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		bystander.Process.Kill()
		bystander.Wait()
	}()
	second := standIn(t)
	r2 := newRecorder()
	inst2, err := second.Start(vm.Spec{CPUs: 1, MemMiB: 512}, r2)
	if err != nil {
		t.Fatal(err)
	}
	<-r2.conns

	inst.Stop()
	if exit := wait(t, r); exit.Code != 0 {
		t.Errorf("exit: %+v", exit)
	}
	if !alive(bystander.Process.Pid) || !alive(inst2.PID()) {
		t.Error("the bystanders were supposed to outlive the first VM")
	}
	inst2.Stop()
	wait(t, r2)
}

func TestASecondVMIsNotStartedWhileTheFirstStillRuns(t *testing.T) {
	p := standIn(t, "ignore-lifeline=1")
	r := newRecorder()
	inst, err := p.Start(vm.Spec{CPUs: 1, MemMiB: 512}, r)
	if err != nil {
		t.Fatal(err)
	}
	<-r.conns
	inst.Stop()

	next := &vm.Process{Bin: p.Bin, Prefix: p.Prefix, Dir: p.Dir, StandIn: true}
	r2 := newRecorder()
	inst2, err := next.Start(vm.Spec{CPUs: 1, MemMiB: 512}, r2)
	if err == nil {
		inst2.Kill()
		wait(t, r2)
		inst.Kill()
		wait(t, r)
		t.Fatal("a second VM was started in the directory of one that is still running")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("error: %v", err)
	}
	if _, serr := os.Stat(inst.VsockPath(vm.PortAgent)); serr != nil {
		t.Errorf("the running VM's sockets were removed: %v", serr)
	}

	inst.Kill()
	wait(t, r)
	r3 := newRecorder()
	inst3, err := next.Start(vm.Spec{CPUs: 1, MemMiB: 512}, r3)
	if err != nil {
		t.Fatalf("after the first VM had gone: %v", err)
	}
	<-r3.conns
	inst3.Stop()
	wait(t, r3)
}

func TestStaleSocketsAreClearedInADirectoryWithPatternCharacters(t *testing.T) {
	p := standIn(t)
	p.Dir = filepath.Join(filepath.Dir(p.Dir), "v[1]")
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"host_1025.sock", "vsock_1024.sock"} {
		l, err := net.Listen("unix", filepath.Join(p.Dir, name))
		if err != nil {
			t.Fatal(err)
		}
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		l.Close()
	}
	r := newRecorder()
	inst, err := p.Start(vm.Spec{CPUs: 1, MemMiB: 512}, r)
	if err != nil {
		t.Fatalf("over stale sockets: %v", err)
	}
	select {
	case <-r.conns:
	case <-time.After(5 * time.Second):
		t.Error("the guest never connected")
	}
	inst.Stop()
	wait(t, r)
}

func TestTheShareIsOneFlagAndOnlyWhenAsked(t *testing.T) {
	arg, err := vm.Share{Root: "/", Deny: []string{"/Users/someone/Library/Application Support/dev.fevom/data", "/Users/someone/.fev/run"}}.Arg()
	if err != nil || arg != "tag=mac,path=/,deny=/Users/someone/Library/Application Support/dev.fevom/data,deny=/Users/someone/.fev/run" {
		t.Fatalf("the flag: %q, %v", arg, err)
	}
	for _, bad := range []vm.Share{
		{Root: "/Users/a,b"}, {Root: "/", Deny: []string{"/Users/a,b/data"}}, {Root: "relative"}, {Root: "/", Deny: []string{"data"}}, {Root: ""},
	} {
		if arg, err := bad.Arg(); err == nil {
			t.Errorf("%+v was turned into %q", bad, arg)
		}
	}

	for _, asked := range []bool{true, false} {
		p := standIn(t)
		root := filepath.Dir(p.Dir)
		data := filepath.Join(root, "not", "yet", "data")
		p.Share = &vm.Share{Root: root, Deny: []string{data}}
		r := newRecorder()
		inst, err := p.Start(vm.Spec{BootID: "t", CPUs: 1, MemMiB: 64, Share: asked}, r)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		client, _, err := agent.NewClient(ctx, <-r.conns, "test")
		if err != nil {
			t.Fatal(err)
		}
		info, err := client.Info(ctx)
		cancel()
		if err != nil || (info.FileShare != nil) != asked {
			t.Errorf("asked %v: the guest reports %+v, %v", asked, info.FileShare, err)
		}
		if _, err := os.Stat(data); (err == nil) != asked {
			t.Errorf("asked %v: the hidden directory: %v", asked, err)
		}
		client.Close()
		inst.Stop()
		wait(t, r)
	}

	p := standIn(t)
	p.Share = &vm.Share{Root: "/tmp/a,b"}
	if _, err := p.Start(vm.Spec{BootID: "t", CPUs: 1, MemMiB: 64, Share: true}, newRecorder()); err == nil || !strings.Contains(err.Error(), "comma") {
		t.Fatalf("started with a path the flag cannot carry: %v", err)
	}
}

func TestAnOrphanVMMThatIgnoresItsLifelineIsKilledByTheNextDaemon(t *testing.T) {
	old := vm.OrphanGrace
	vm.OrphanGrace = 300 * time.Millisecond
	defer func() { vm.OrphanGrace = old }()
	dir, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	out, err := exec.Command(os.Args[0], "--a-daemon-that-dies", dir).Output()
	if err != nil {
		t.Fatalf("the daemon that dies: %v", err)
	}
	orphan, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || !alive(orphan) {
		t.Fatalf("no orphan VMM (%q, %v)", out, err)
	}
	defer syscall.Kill(orphan, syscall.SIGKILL)

	next := &vm.Process{Bin: os.Args[0], Prefix: []string{standin.Arg}, Dir: dir, StandIn: true}
	_, err = next.Start(vm.Spec{CPUs: 1, MemMiB: 512}, newRecorder())
	if err == nil || !strings.Contains(err.Error(), "still running") || !alive(orphan) {
		t.Fatalf("the first try: %v (the orphan alive: %v)", err, alive(orphan))
	}
	time.Sleep(vm.OrphanGrace + 50*time.Millisecond)
	r := newRecorder()
	inst, err := next.Start(vm.Spec{CPUs: 1, MemMiB: 512}, r)
	if err != nil {
		t.Fatalf("after the grace: %v", err)
	}
	<-r.conns
	if alive(orphan) {
		t.Error("the orphan VMM is still alive beside the new one")
	}
	inst.Stop()
	wait(t, r)
}

func TestAVerboseBootTakesQuietOffTheKernelsCommandLine(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"console=hvc0 quiet", "console=hvc0"},
		{"quiet console=hvc0 loglevel=3", "console=hvc0 loglevel=3"},
		{"console=hvc0", "console=hvc0"},
		{"console=hvc0 quiet_but_not_this", "console=hvc0 quiet_but_not_this"},
		{"", ""},
	} {
		if got := vm.Cmdline(c.in, true); got != c.want {
			t.Errorf("Cmdline(%q, verbose) = %q, want %q", c.in, got, c.want)
		}
		if got := vm.Cmdline(c.in, false); got != c.in {
			t.Errorf("Cmdline(%q) = %q, want it unchanged", c.in, got)
		}
	}
}

func TestEachBootSaysWhatTheDataImageOccupies(t *testing.T) {
	logged := func(p *vm.Process) string {
		t.Helper()
		r := newRecorder()
		inst, err := p.Start(vm.Spec{CPUs: 1, MemMiB: 512}, r)
		if err != nil {
			t.Fatal(err)
		}
		<-r.conns
		inst.Stop()
		wait(t, r)
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, l := range r.vmm {
			if _, line, ok := strings.Cut(l, "kernel command line: "); ok {
				return line
			}
		}
		t.Fatalf("the stand-in logged no command line: %q", r.vmm)
		return ""
	}
	p := standIn(t)
	p.Cmdline = "console=hvc0 quiet"
	if got := logged(p); got != "console=hvc0 quiet" {
		t.Errorf("with no data image: %q", got)
	}

	image := filepath.Join(filepath.Dir(p.Dir), "data.img")
	f, err := os.Create(image)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(make([]byte, 1<<20), 4<<20); err != nil {
		t.Fatal(err)
	}
	f.Sync()
	f.Close()
	p.DataImage = image
	got := logged(p)
	word := ""
	for _, w := range strings.Fields(got) {
		if v, ok := strings.CutPrefix(w, vm.AllocatedParam+"="); ok {
			word = v
		}
	}
	n, err := strconv.ParseInt(word, 10, 64)
	if err != nil || n < 1<<20 || n >= 1<<30 {
		t.Errorf("command line %q: want %s= the blocks written (1 MiB or a little more)", got, vm.AllocatedParam)
	}
	if !strings.HasPrefix(got, "console=hvc0 quiet ") {
		t.Errorf("the base command line changed: %q", got)
	}

	f, _ = os.OpenFile(image, os.O_WRONLY, 0)
	f.WriteAt(make([]byte, 8<<20), 100<<20)
	f.Sync()
	f.Close()
	got = logged(p)
	if !strings.Contains(got, vm.AllocatedParam+"=") || strings.Contains(got, vm.AllocatedParam+"="+word+" ") || strings.HasSuffix(got, "="+word) {
		t.Errorf("the second boot's figure is the first's: %q (first %s)", got, word)
	}
}
