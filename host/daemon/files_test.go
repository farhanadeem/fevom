package daemon

import (
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"fev/host/daemon/responsible"
	"fev/host/daemon/vm"
	"fev/proto/agent"
	api "fev/proto/daemon"
)

type denials struct {
	stub
	mu      sync.Mutex
	folders []string
	granted []string
}

func (d *denials) Denied(folder string) {
	d.mu.Lock()
	d.folders = append(d.folders, folder)
	d.mu.Unlock()
}

func (d *denials) Granted(folder string) {
	d.mu.Lock()
	d.granted = append(d.granted, folder)
	d.mu.Unlock()
}

func TestTheVMIsStartedWithTheShareUnlessTheSettingIsOff(t *testing.T) {
	f := newFixture(t)
	share := filepath.Join(f.layout.Home, "mac")
	f.launch.Process.Share = &vm.Share{Root: share, Deny: []string{f.layout.DataDir, f.layout.RunDir()}}
	f.start()
	c := f.client()
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	if !f.launch.spec(0).Share {
		t.Fatal("the first boot was started without the share")
	}
	guest, ok := f.d.sup.VM()
	if !ok {
		t.Fatal("no VM")
	}
	info, err := guest.Agent().Info(bg)
	if err != nil || info.FileShare == nil || info.FileShare.Tag != vm.ShareTag || info.FileShare.Target != vm.ShareMount ||
		info.FileShare.Feed != agent.FeedConnected {
		t.Fatalf("the guest's view of the share: %+v, %v", info.FileShare, err)
	}
	r, err := guest.Agent().FileShareWatch(bg, []string{share, f.layout.DataDir, filepath.Join(f.layout.RunDir(), "vm"), "/elsewhere"})
	if err != nil || r.Watching != 0 || len(r.Rejected) != 4 || r.Rejected[0].Reason != agent.WatchMissing ||
		r.Rejected[1].Reason != agent.WatchOutside || r.Rejected[2].Reason != agent.WatchOutside || r.Rejected[3].Reason != agent.WatchOutside {
		t.Fatalf("the watch set against the share: %+v, %v", r, err)
	}

	changed, err := c.ConfigSet(bg, map[string]any{api.KeyFilesShare: false})
	if err != nil || len(changed) != 1 || !changed[0].Pending || changed[0].Apply != api.ApplyVMRestart {
		t.Fatalf("config.set: %+v %v", changed, err)
	}
	if st, _ := c.Status(bg); !reflect.DeepEqual(st.PendingRestart, []string{api.KeyFilesShare}) {
		t.Fatalf("pending: %v", st.PendingRestart)
	}
	if _, err := c.VMRestart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	if f.launch.spec(1).Share {
		t.Fatal("the second boot was started with the share")
	}
	guest, _ = f.d.sup.VM()
	if info, err := guest.Agent().Info(bg); err != nil || info.FileShare != nil {
		t.Fatalf("a guest with no share reports one: %+v, %v", info.FileShare, err)
	}
	if _, err := guest.Agent().FileShareWatch(bg, []string{share}); err == nil {
		t.Fatal("a guest with no share took a watch set")
	}
}

func TestAFolderTheMacRefusedIsAWarningUntilTheVMStops(t *testing.T) {
	m := &denials{}
	app := responsible.Describe(41, "/Users/someone/Applications/Fevom.app/Contents/MacOS/Fevom", "")
	was := responsibleSelf
	responsibleSelf = func() responsible.Who { return app }
	t.Cleanup(func() { responsibleSelf = was })
	f := newFixture(t, []string{"denied=/Users/someone/Documents", "denied2=/Users/someone/Documents", "denied3=relative/path"})
	f.launch.Process.Share = &vm.Share{Root: "/"}
	f.opts.Modules = []Module{m}
	f.start()
	c := f.client()
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	st := f.until("the warning", func(st api.Status) bool { return len(st.Warnings) > 0 })
	want := DeniedWarning("/Users/someone/Documents", app)
	if !reflect.DeepEqual(st.Warnings, []string{want}) || !strings.Contains(want, "/Users/someone/Documents") || !strings.Contains(want, "Privacy & Security") ||
		!strings.Contains(want, "Allow it for Fevom, the app fev was started from") {
		t.Fatalf("warnings: %q", st.Warnings)
	}
	if st.State != api.StateRunning && st.State != api.StateStarting {
		t.Fatalf("a denial changed the VM's state: %+v", st)
	}
	m.mu.Lock()
	folders := append([]string{}, m.folders...)
	m.mu.Unlock()
	if !reflect.DeepEqual(folders, []string{"/Users/someone/Documents"}) {
		t.Fatalf("the module was told %q", folders)
	}
	f.running()
	if tr, err := c.BootTrace(bg); err != nil {
		t.Fatal(err)
	} else {
		for _, mark := range tr.Marks {
			if strings.HasPrefix(mark.Name, "fs.") {
				t.Errorf("the denial is in the boot trace as %q", mark.Name)
			}
		}
	}
	if st, err := c.VMStop(bg, api.StopParams{}); err != nil || len(st.Warnings) != 0 {
		t.Fatalf("after the stop: %+v %v", st.Warnings, err)
	}
}

func TestAFolderTheMacAllowsAfterAllIsWithdrawn(t *testing.T) {
	m := &denials{}
	f := newFixture(t, []string{"denied=/Users/someone/Documents", "denied2=/Users/someone/Desktop", "granted-ms=150"})
	f.launch.Process.Share = &vm.Share{Root: "/"}
	f.opts.Modules = []Module{m}
	f.start()
	c := f.client()
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	f.until("both warnings", func(st api.Status) bool { return len(st.Warnings) == 2 })
	st := f.until("the warnings withdrawn", func(st api.Status) bool { return len(st.Warnings) == 0 })
	if st.State != api.StateRunning {
		t.Fatalf("%+v", st)
	}
	m.mu.Lock()
	granted := append([]string{}, m.granted...)
	m.mu.Unlock()
	if !reflect.DeepEqual(granted, []string{"/Users/someone/Documents", "/Users/someone/Desktop"}) {
		t.Fatalf("the module was told of %q", granted)
	}
	if log := f.logFile("fevd.log"); count(log, "macOS now lets the VM read a folder it had refused") != 2 {
		t.Errorf("the daemon's log:\n%s", log)
	}
	b := f.d.sup.cur
	for _, mark := range []vm.Mark{
		{Name: "fs.granted", Path: "/Users/someone/Music"}, {Name: "fs.granted", Path: "relative"},
		{Name: "fs.denied", Path: "/Users/someone/Documents"},
	} {
		b.Mark(mark)
	}
	if st := f.d.hub.Status(); !reflect.DeepEqual(st.Warnings, []string{DeniedWarning("/Users/someone/Documents", f.d.sup.grantee)}) {
		t.Fatalf("after a second refusal: %q", st.Warnings)
	}
}
