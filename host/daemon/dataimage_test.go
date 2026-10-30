package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"fev/host/daemon/home"
	"fev/host/storage"
	api "fev/proto/daemon"
)

func TestTheDataImageExistsBeforeEachBootAndFollowsItsSetting(t *testing.T) {
	f := newFixture(t)
	image := f.layout.DataImage()
	if !strings.HasPrefix(image, f.layout.Home+"/data/") {
		t.Fatalf("a moved home keeps its data image at %s", image)
	}
	f.launch.Process.DataImage = image
	f.opts.Modules = []Module{&DataImage{Path: image}}
	f.start()
	if _, err := os.Stat(image); err == nil {
		t.Fatal("the image was made before any VM was started")
	}
	c := f.client()
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(image)
	if err != nil {
		t.Fatal(err)
	}
	if blocks := info.Sys().(*syscall.Stat_t).Blocks; info.Size() != storage.DefaultSize || blocks*512 > 1<<20 {
		t.Errorf("the image is %d bytes and occupies %d", info.Size(), blocks*512)
	}
	if excluded, err := storage.BackupExcluded(image); err != nil || !excluded {
		t.Errorf("a new image is excluded from backups: %v %v", excluded, err)
	}
	if log := f.logFile("fevd.log"); !strings.Contains(log, "created the data image") {
		t.Error("the daemon's log does not record the creation")
	}

	excluded := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			got, err := storage.BackupExcluded(image)
			if err == nil && got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("excluded from backups: %v (%v), want %v", got, err, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	changed, err := c.ConfigSet(bg, map[string]any{api.KeyDataAllowBackup: true})
	if err != nil || len(changed) != 1 || changed[0].Pending || changed[0].Apply != api.ApplyLive {
		t.Fatalf("config.set: %+v %v", changed, err)
	}
	excluded(false)
	c.ConfigReset(bg, api.KeyDataAllowBackup)
	excluded(true)

	os.WriteFile(image+".marker", nil, 0o600)
	before, _ := os.Stat(image)
	c.VMRestart(bg, api.StartParams{})
	if after, _ := os.Stat(image); !os.SameFile(before, after) {
		t.Error("an existing image was replaced at a restart")
	}
	c.VMStop(bg, api.StopParams{})
	os.Remove(image)
	c.ConfigSet(bg, map[string]any{api.KeyDataAllowBackup: true})
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.BackupExcluded(image); err != nil || got {
		t.Errorf("an image made with backups allowed: excluded=%v %v", got, err)
	}
}

func TestABootFailsWithTheReasonIfTheDataImageCannotBeMade(t *testing.T) {
	f := newFixture(t)
	blocked := f.layout.Home + "/blocked"
	os.MkdirAll(f.layout.Home, 0o755)
	os.WriteFile(blocked, []byte("a file where the data directory should be"), 0o644)
	f.opts.Modules = []Module{&DataImage{Path: blocked + "/data.img"}}
	f.start()
	_, err := f.client().VMStart(bg, api.StartParams{})
	if code(err) != api.CodeFailed || !strings.Contains(err.Error(), "the VM could not be started") || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("vm.start: %v", err)
	}
	if f.launch.started() != 0 {
		t.Errorf("the VMM was started %d times without a data image", f.launch.started())
	}
}

func TestAResetRemovesTheDataAndKeepsTheConfiguration(t *testing.T) {
	f := newFixture(t)
	image := f.layout.DataImage()
	f.launch.Process.DataImage = image
	f.opts.Modules = []Module{&DataImage{Path: image}}
	f.start()
	c := f.client()
	if _, err := c.ConfigSet(bg, map[string]any{api.KeyDataAllowBackup: true}); err != nil {
		t.Fatal(err)
	}
	first, err := c.VMStart(bg, api.StartParams{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(image, []byte("linux data"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(image)
	r, err := c.DataReset(bg)
	if err != nil {
		t.Fatal(err)
	}
	if r.FreedBytes <= 0 {
		t.Errorf("freed %d bytes", r.FreedBytes)
	}
	st, err := c.VMStart(bg, api.StartParams{})
	if err != nil || st.State != api.StateRunning || st.BootID == first.BootID {
		t.Fatalf("after the reset: %+v %v (the boot before was %s)", st, err, first.BootID)
	}
	after, err := os.Stat(image)
	if err != nil || os.SameFile(before, after) || after.Size() != storage.DefaultSize {
		t.Fatalf("the image after a reset: %v %v", after, err)
	}
	if head := make([]byte, 10); true {
		fh, _ := os.Open(image)
		fh.Read(head)
		fh.Close()
		if string(head) == "linux data" {
			t.Error("the old data is still there")
		}
	}
	if values, err := c.ConfigGet(bg, api.KeyDataAllowBackup); err != nil || values[api.KeyDataAllowBackup].Value != true {
		t.Errorf("the configuration after a reset: %v %v", values, err)
	}

	if _, err := c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	r, err = c.DataReset(bg)
	if err != nil || r.Status.State != api.StateStopped {
		t.Fatalf("a reset while stopped: %+v %v", r, err)
	}
	if _, err := os.Stat(image); err == nil {
		t.Error("the image is still there after a reset while stopped")
	}
}

func TestAPlacedDataDirectoryThatIsGoneIsRefused(t *testing.T) {
	f := newFixture(t)
	ext := filepath.Join(t.TempDir(), "external", "fev")
	if err := f.layout.WritePlacement(ext); err != nil {
		t.Fatal(err)
	}
	l, err := home.Resolve(f.layout.Home, "")
	if err != nil || !l.Placed || l.DataDir != ext {
		t.Fatalf("%+v %v", l, err)
	}
	f.layout, f.opts.Layout = l, l
	f.launch.Process.DataImage = l.DataImage()
	f.opts.Modules = []Module{&DataImage{Path: l.DataImage()}}
	f.start()
	c := f.client()
	_, err = c.VMStart(bg, api.StartParams{})
	if err == nil || !strings.Contains(err.Error(), ext+" is not there: is its disk connected?") {
		t.Fatalf("vm.start: %v", err)
	}
	if f.launch.started() != 0 {
		t.Errorf("the VMM was started %d times", f.launch.started())
	}
	if _, err := os.Stat(ext); !os.IsNotExist(err) {
		t.Errorf("the placed directory was made: %v", err)
	}
	os.MkdirAll(ext, 0o700)
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.DataImage()); err != nil {
		t.Errorf("no image in the placed directory: %v", err)
	}
}
