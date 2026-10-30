package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"fev/host/nfsview"
	"fev/host/power"
	api "fev/proto/daemon"
)

var viewStandin struct {
	once sync.Once
	path string
	err  error
}

func nfsStandin(t *testing.T) string {
	t.Helper()
	viewStandin.once.Do(func() {
		dir, err := os.MkdirTemp("", "nfsstandin")
		if err != nil {
			viewStandin.err = err
			return
		}
		viewStandin.path = filepath.Join(dir, "standin")
		cmd := exec.Command("go", "build", "-o", viewStandin.path, ".")
		cmd.Dir = filepath.Join("..", "..", "tests", "fixtures", "nfsstandin")
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly")
		if out, err := cmd.CombinedOutput(); err != nil {
			viewStandin.err = errors.New(string(out))
		}
	})
	if viewStandin.err != nil {
		t.Fatalf("building tests/fixtures/nfsstandin: %v", viewStandin.err)
	}
	d, err := os.MkdirTemp("/tmp", "bv.vt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	sock := filepath.Join(d, "nfs.sock")
	cmd := exec.Command(viewStandin.path, "-lifeline", "-unix", sock)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	if line, _ := bufio.NewReader(out).ReadString('\n'); !strings.HasPrefix(line, "listening ") {
		t.Fatalf("the stand-in said %q", line)
	}
	return sock
}

type shareStub struct{ stub }

func (s *shareStub) Start(h Host) error {
	h.Handle(api.MethodFilesStatus, func(context.Context, json.RawMessage) (any, error) {
		return api.FilesStatus{Share: true, Watch: api.FilesWatch{Paths: []string{}}}, nil
	})
	return nil
}

type viewFixture struct {
	*fixture
	view  *View
	dir   string
	power *power.Injected
}

func newViewFixture(t *testing.T, change func(*viewFixture), script ...string) *viewFixture {
	t.Helper()
	sock := nfsStandin(t)
	f := newFixture(t, append([]string{"view-nfs=" + sock}, script...))
	dir := filepath.Join(t.TempDir(), "Fevom")
	t.Cleanup(func() {
		nfsview.ForceUnmount(dir)
		os.Chmod(dir, 0o755)
	})
	vf := &viewFixture{fixture: f, dir: dir, view: &View{Dir: dir}}
	f.opts.Modules = []Module{&shareStub{}, vf.view}
	if change != nil {
		change(vf)
	}
	f.start()
	return vf
}

func (vf *viewFixture) boot() {
	vf.t.Helper()
	if _, err := vf.client().VMStart(bg, api.StartParams{}); err != nil {
		vf.t.Fatal(err)
	}
	vf.viewIs(api.ViewMounted)
}

func (vf *viewFixture) viewIs(state string) api.Status {
	vf.t.Helper()
	return vf.until("the view "+state, func(st api.Status) bool { return st.View != nil && st.View.State == state })
}

func (vf *viewFixture) mounted() (nfsview.Mounted, bool) {
	vf.t.Helper()
	m, ok, err := nfsview.Find(vf.dir)
	if err != nil {
		vf.t.Fatal(err)
	}
	return m, ok
}

func within(t *testing.T, d time.Duration, what string, fn func() error) (time.Duration, error) {
	t.Helper()
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return time.Since(start), err
	case <-time.After(d):
		t.Fatalf("%s took more than %v", what, d)
	}
	return 0, nil
}

func (vf *viewFixture) saw(words ...string) {
	vf.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, line := range strings.Split(vf.log(), "\n") {
			all := true
			for _, w := range words {
				all = all && strings.Contains(line, w)
			}
			if all {
				return
			}
		}
		if time.Now().After(deadline) {
			vf.t.Fatalf("no line of the log says %q:\n%s", words, vf.log())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (vf *viewFixture) log() string {
	data, _ := os.ReadFile(filepath.Join(vf.layout.LogDir(), "fevd.log"))
	return string(data)
}

func TestTheViewIsMountedWhileTheVMRunsAndUnmountedBeforeItStops(t *testing.T) {
	vf := newViewFixture(t, nil)
	st := vf.d.hub.Status()
	if st.View == nil || st.View.State != api.ViewUnmounted || st.View.Path != vf.dir || st.View.Socket != vf.view.endpoint.String() || vf.view.endpoint.Port == 0 {
		t.Fatalf("the view's block at the start: %+v", st.View)
	}
	if data, err := os.ReadFile(filepath.Join(vf.dir, "README.txt")); err != nil || string(data) != nfsview.StoppedREADME {
		t.Fatalf("the stopped folder's README: %q %v", data, err)
	}
	vf.boot()
	m, ok := vf.mounted()
	if !ok || m.From != nfsview.Source(vf.view.endpoint) || m.From != nfsview.ServerName+":/" || m.Hidden() {
		t.Fatalf("mounted: %v %+v", ok, m)
	}
	if data, err := os.ReadFile(filepath.Join(vf.dir, "README.txt")); err != nil || string(data) != "fev view stand-in\n" {
		t.Fatalf("the view's README: %q %v", data, err)
	}
	fs, err := vf.client().FilesStatus(bg)
	if err != nil || fs.View == nil || fs.View.State != api.ViewMounted || !fs.Share {
		t.Fatalf("files.status: %+v %v", fs, err)
	}
	if _, err := vf.client().VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	vf.stopped()
	if _, ok := vf.mounted(); ok {
		t.Fatal("still mounted after the stop")
	}
	if st := vf.d.hub.Status(); st.View.State != api.ViewUnmounted {
		t.Fatalf("after the stop: %+v", st.View)
	}
	vf.saw("the VM has stopped")
	log := vf.log()
	un, stopped := strings.Index(log, "view: unmounted"), strings.Index(log, "the VM has stopped")
	if un < 0 || stopped < 0 || un > stopped || !strings.Contains(log[un:], "forced=false") {
		t.Fatalf("the unmount is not a plain one before the VM's stop:\n%s", log)
	}
	if data, err := os.ReadFile(filepath.Join(vf.dir, "README.txt")); err != nil || string(data) != nfsview.StoppedREADME {
		t.Fatalf("the stopped folder after the stop: %q %v", data, err)
	}
	vf.boot()
}

func TestTheViewIsForcedOffTheMomentTheVMMExits(t *testing.T) {
	vf := newViewFixture(t, nil)
	vf.boot()
	info, err := vf.layout.ReadInfo()
	if err != nil || info.VMMPID == 0 {
		t.Fatalf("no VMM pid: %+v %v", info, err)
	}
	f, err := os.Create(filepath.Join(vf.dir, "docker", "volumes", "w"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	killed := time.Now()
	syscall.Kill(info.VMMPID, syscall.SIGKILL)
	took, _ := within(t, 5*time.Second, "a write after the VMM's death", func() error {
		_, err := f.Write([]byte("x"))
		if err == nil {
			err = syscall.Fsync(int(f.Fd()))
		}
		return err
	})
	t.Logf("the write after the kill returned in %v", took)
	for {
		if _, ok := vf.mounted(); !ok {
			break
		}
		if time.Since(killed) > 5*time.Second {
			t.Fatal("still mounted 5 s after the VMM's death")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("unmounted %v after the kill", time.Since(killed))
	if _, err := within(t, 5*time.Second, "a look at the folder", func() error {
		_, err := os.ReadDir(vf.dir)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	vf.saw("view: unmounted", "forced=true")
}

func TestASilentGuestHasItsViewForcedOff(t *testing.T) {
	old := nfsview.StallAfter
	nfsview.StallAfter = 500 * time.Millisecond
	defer func() { nfsview.StallAfter = old }()
	vf := newViewFixture(t, nil)
	vf.boot()
	info, _ := vf.layout.ReadInfo()
	if err := syscall.Kill(info.VMMPID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(info.VMMPID, syscall.SIGCONT)
	took, _ := within(t, 5*time.Second, "a create in the view of a silent guest", func() error {
		return os.WriteFile(filepath.Join(vf.dir, "docker", "volumes", "made"), []byte("x"), 0o644)
	})
	t.Logf("the create returned in %v", took)
	st := vf.viewIs(api.ViewFailed)
	if !strings.Contains(st.View.Error, "the VM did not answer") {
		t.Fatalf("the reason: %q", st.View.Error)
	}
	if _, ok := vf.mounted(); ok {
		t.Fatal("still mounted")
	}
	syscall.Kill(info.VMMPID, syscall.SIGCONT)
	if _, err := vf.client().VMRestart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	vf.viewIs(api.ViewMounted)
}

func TestAPausedVMsViewIsAnsweredByTheDaemon(t *testing.T) {
	old := nfsview.StallAfter
	nfsview.StallAfter = 300 * time.Millisecond
	t.Cleanup(func() { nfsview.StallAfter = old })
	vf := newViewFixture(t, func(vf *viewFixture) {
		vf.power = power.NewInjected()
		vf.opts.Power = vf.power
	})
	vf.boot()
	send[api.PowerSleep](t, vf.power, power.WillSleep)
	vf.until("paused", func(st api.Status) bool { return st.State == api.StatePaused })
	before := vf.view.srv.Answered()
	done := make(chan error, 1)
	go func() { done <- os.WriteFile(filepath.Join(vf.dir, "docker", "volumes", "slept"), []byte("x"), 0o644) }()
	time.Sleep(4 * nfsview.StallAfter)
	if _, ok := vf.mounted(); !ok {
		t.Fatal("the view was unmounted while the VM was paused")
	}
	if st := vf.d.hub.Status(); st.View.State != api.ViewMounted {
		t.Fatalf("the view while paused: %+v", st.View)
	}
	if n := vf.view.srv.Answered() - before; n == 0 {
		t.Fatal("no call of the Mac's was answered in the guest's place while paused")
	}
	select {
	case err := <-done:
		t.Fatalf("the write made in the pause ended while paused: %v", err)
	default:
	}
	send[api.PowerWake](t, vf.power, power.HasPoweredOn)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the write made in the pause: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the write made in the pause did not end after the wake")
	}
	if st := vf.d.hub.Status(); st.View.State != api.ViewMounted {
		t.Fatalf("the view after the wake: %+v", st.View)
	}
	after := vf.view.srv.Answered()
	if _, err := within(t, 2*time.Second, "a stat after the wake", func() error {
		_, err := os.Stat(filepath.Join(vf.dir, "docker", "volumes", "slept"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := vf.view.srv.Answered(); n != after {
		t.Fatalf("%d calls answered in the guest's place after the wake", n-after)
	}
}

func TestHidingTheViewRemountsItAtOnce(t *testing.T) {
	vf := newViewFixture(t, nil)
	vf.boot()
	c := vf.client()
	f, err := os.Open(filepath.Join(vf.dir, "README.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, hidden := range []bool{true, false, true} {
		if _, err := c.ConfigSet(bg, map[string]any{api.KeyFilesViewHidden: hidden}); err != nil {
			t.Fatal(err)
		}
		vf.until("hidden", func(st api.Status) bool { return st.View != nil && st.View.Hidden == hidden })
		m, ok := vf.mounted()
		if !ok || m.Hidden() != hidden || m.Flags&unix.MNT_NOATIME == 0 {
			t.Fatalf("hidden %v: the mount %v %+v", hidden, ok, m)
		}
	}
	buf := make([]byte, 3)
	if _, err := f.ReadAt(buf, 0); err != nil || string(buf) != "fev" {
		t.Fatalf("the file held open: %q %v", buf, err)
	}
	if _, err := c.VMRestart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	vf.until("mounted again", func(st api.Status) bool {
		return st.View != nil && st.View.State == api.ViewMounted && st.BootID != ""
	})
	if m, ok := vf.mounted(); !ok || !m.Hidden() {
		t.Fatalf("the next mount: %v %+v", ok, m)
	}
}

func TestAViewLeftByAKilledDaemonIsRemovedAtTheStart(t *testing.T) {
	sock := nfsStandin(t)
	dir := filepath.Join(t.TempDir(), "Fevom")
	t.Cleanup(func() { nfsview.ForceUnmount(dir); os.Chmod(dir, 0o755) })
	if err := nfsview.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("/sbin/mount_nfs", nfsview.MountArgs(nfsview.Endpoint{Socket: sock}, dir, true)...).Run(); err != nil {
		t.Fatal(err)
	}
	vf := newViewFixture(t, func(vf *viewFixture) { vf.dir = dir; vf.view.Dir = dir })
	if m, ok := vf.mounted(); ok {
		t.Fatalf("the stale view is still there: %+v", m)
	}
	if !strings.Contains(vf.log(), "removed the view an earlier daemon left mounted") {
		t.Fatal("not said")
	}
	vf.boot()
}

func TestAFolderOfTheUsersFilesIsNotMountedOver(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Fevom")
	os.Mkdir(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o644)
	vf := newViewFixture(t, func(vf *viewFixture) { vf.dir = dir; vf.view.Dir = dir })
	st := vf.d.hub.Status()
	if st.View.State != api.ViewFailed || !strings.Contains(st.View.Error, "notes.txt") {
		t.Fatalf("the block: %+v", st.View)
	}
	if _, err := vf.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	vf.running()
	time.Sleep(300 * time.Millisecond)
	if _, ok := vf.mounted(); ok {
		t.Fatal("mounted over the user's files")
	}
	if st := vf.d.hub.Status(); st.View.State != api.ViewFailed || !strings.Contains(st.View.Error, "notes.txt") {
		t.Fatalf("the block while running: %+v", st.View)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "notes.txt")); string(data) != "mine" {
		t.Fatal("the user's file changed")
	}
}

func TestAnObjectsEndNamesItsPlaceInTheView(t *testing.T) {
	for _, c := range []struct {
		event any
		want  []string
	}{
		{api.DockerEvent{Type: "container", Action: "die", Name: "web"}, []string{"docker/containers/web"}},
		{api.DockerEvent{Type: "container", Action: "destroy", Attributes: map[string]string{"name": "db"}}, []string{"docker/containers/db"}},
		{api.DockerEvent{Type: "container", Action: "rename", Name: "new", Attributes: map[string]string{"oldName": "/old"}}, []string{"docker/containers/old"}},
		{api.DockerEvent{Type: "container", Action: "start", Name: "web"}, nil},
		{api.DockerEvent{Type: "volume", Action: "destroy", ID: "data"}, []string{"docker/volumes/data"}},
		{api.DockerEvent{Type: "image", Action: "delete", Name: "ghcr.io/org/app:1"}, []string{"docker/images/ghcr.io_org_app:1"}},
		{api.MachinesChanged{Machine: api.Machine{Name: "ubuntu"}, Removed: true}, []string{"ubuntu"}},
		{api.MachinesChanged{Machine: api.Machine{Name: "ubuntu"}}, nil},
	} {
		got := goneFromView(c.event)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%+v: %v, want %v", c.event, got, c.want)
		}
	}
}
