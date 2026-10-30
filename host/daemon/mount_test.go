package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"fev/host/daemon/vm/standin"
	"fev/proto/agent"
	api "fev/proto/daemon"
)

func mounting(t *testing.T, script ...[]string) (*fixture, *DataImage) {
	t.Helper()
	f := newFixture(t, script...)
	m := &DataImage{Path: f.layout.DataImage(), Margin: 1 << 30}
	m.free = func(string) (uint64, error) { return 10 << 30, nil }
	m.blocks = func(string) (uint64, error) { return 2 << 30, nil }
	f.launch.Process.DataImage = m.Path
	f.opts.Modules = []Module{m}
	f.start()
	return f, m
}

func (f *fixture) boot() api.Status {
	f.t.Helper()
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		f.t.Fatal(err)
	}
	return f.running()
}

func (f *fixture) warning(words ...string) string {
next:
	for _, w := range f.d.hub.Status().Warnings {
		for _, word := range words {
			if !strings.Contains(w, word) {
				continue next
			}
		}
		return w
	}
	return ""
}

func (f *fixture) awaitWarning(want bool, what string, words ...string) string {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := f.warning(words...)
		if (got != "") == want {
			return got
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("%s: warnings %q\n%s", what, f.d.hub.Status().Warnings, f.logFile("fevd.log"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAMountAboveTheFirstRungIsAWarningUntilABootMountsNormally(t *testing.T) {
	for _, c := range []struct {
		rung string
		says []string
		not  []string
	}{
		{"2", []string{"did not mount normally", "step 2 of 5, btrfs rescue super-recover", "Nothing was given up", "no checksum", "`fev restart`", "`fev logs --files --source init`"}, []string{"Data was lost"}},
		{"3", []string{"step 3 of 5, btrfs rescue zero-log", "Data was lost: the stand-in's fsync log was dropped.", "no checksum", "`fev restart`"}, []string{"Nothing was given up"}},
		{"4", []string{"step 4 of 5, read-only rescue=usebackuproot", "mounted read-only as of an older commit", "Nothing on it was changed. ", "Every start of the VM finds it the same way", "`fev salvage DIR`"}, []string{"Data was lost", "was recovered", "`fev restart`", "no command yet"}},
	} {
		t.Run("rung "+c.rung, func(t *testing.T) {
			f, _ := mounting(t, []string{"mount-rung=" + c.rung}, []string{"mount-rung=" + c.rung}, nil)
			st := f.boot()
			got := f.awaitWarning(true, "no warning after a mount on rung "+c.rung, "data disk")
			for _, want := range append(c.says, "boot "+st.BootID) {
				if !strings.Contains(got, want) {
					t.Errorf("the warning does not say %q:\n%s", want, got)
				}
			}
			for _, not := range c.not {
				if strings.Contains(got, not) {
					t.Errorf("the warning says %q:\n%s", not, got)
				}
			}
			if log := f.logFile("fevd.log"); !strings.Contains(log, "the data disk did not mount normally") {
				t.Errorf("the daemon's log does not have it:\n%s", log)
			}
			c := f.client()
			if _, err := c.VMStop(bg, api.StopParams{}); err != nil {
				t.Fatal(err)
			}
			f.stopped()
			if f.warning("data disk") != got {
				t.Fatalf("the warning went with the stop: %q", f.d.hub.Status().Warnings)
			}
			st = f.boot()
			f.awaitWarning(true, "the second recovery was not reported as its own", "data disk", "boot "+st.BootID)
			if _, err := c.VMStop(bg, api.StopParams{}); err != nil {
				t.Fatal(err)
			}
			f.stopped()
			f.boot()
			f.awaitWarning(false, "the warning outlived a normal mount", "data disk")
		})
	}
}

func TestTheRecoverysWarningStaysUntilTheEngineIsUpOnANormalMount(t *testing.T) {
	f, m := mounting(t, []string{"mount-rung=3"}, []string{"engine-error=its database is not readable"}, nil)
	f.boot()
	got := f.awaitWarning(true, "no warning after a mount on rung 3", "data disk")
	for _, want := range []string{"If the engine does not come up", "`fev reset`", "every image, container and volume"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not say %q:\n%s", want, got)
		}
	}
	c := f.client()
	if _, err := c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	if _, err := c.VMStart(bg, api.StartParams{}); err == nil {
		t.Fatal("the start succeeded with an engine that fails")
	}
	st := f.until("the engine's failure", func(st api.Status) bool { return st.Engine == api.EngineFailed })
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.rmu.Lock()
		answered := m.normalBoot == st.BootID
		m.rmu.Unlock()
		if answered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second boot's mount was never reported\n%s", f.logFile("fevd.log"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if f.warning("data disk") != got {
		t.Fatalf("the warning went at a normal mount whose engine never came up: %q", f.d.hub.Status().Warnings)
	}
	if _, err := c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	f.boot()
	f.awaitWarning(false, "the warning outlived an engine that is up on a normal mount", "data disk")
}

func TestTheRecoverysWarningOutlivesTheDaemon(t *testing.T) {
	f, m := mounting(t, []string{"mount-rung=3"})
	f.boot()
	got := f.awaitWarning(true, "no warning after a mount on rung 3", "data disk")
	if _, err := f.client().VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	f.d.Close()

	g := &fixture{t: t, layout: f.layout}
	g.launch = &scripted{Process: f.launch.Process}
	m2 := &DataImage{Path: m.Path, Margin: 1 << 30, free: m.free, blocks: m.blocks}
	g.opts = f.opts
	g.opts.Launcher, g.opts.Modules = g.launch, []Module{m2}
	g.start()
	if w := g.warning("data disk"); w != got {
		t.Fatalf("the new daemon does not hold the warning: %q", g.d.hub.Status().Warnings)
	}
	g.boot()
	g.awaitWarning(false, "the warning outlived a normal mount with the engine up", "data disk")
	if _, err := os.Stat(m2.recoveredPath()); err == nil {
		t.Fatal("the warning's file outlived the warning")
	}
}

func TestTheSalvageMountSaysWhereTheDiskIsAndWhatAResetCosts(t *testing.T) {
	f, m := mounting(t, []string{"mount-rung=5"})
	f.boot()
	got := f.awaitWarning(true, "no warning after a read-only mount", "data disk")
	for _, want := range []string{"read-only", "step 5 of 5, read-only rescue=all", "Nothing on it was changed", m.Path, "`fev stop`", "`fev reset`", "every image, container and volume"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not say %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "`fev restart`") {
		t.Errorf("a restart does not clear a disk that will not mount:\n%s", got)
	}
	m.forget()
	if w := f.warning("data disk"); w != "" {
		t.Errorf("the warning outlived the reset: %q", w)
	}
}

func TestANormalMountIsNoWarningAndIsAskedOnceABoot(t *testing.T) {
	f, m := mounting(t)
	st := f.boot()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.rmu.Lock()
		answered := m.mountBoot == st.BootID
		device := m.device
		m.rmu.Unlock()
		if answered {
			if device != "vda" {
				t.Fatalf("the data disk's device is %q", device)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the guest was never asked how the disk was mounted\n%s", f.logFile("fevd.log"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if w := f.warning("data disk"); w != "" {
		t.Fatalf("a warning after a normal mount: %q", w)
	}
	m.rmu.Lock()
	m.mountBoot = "asked-again?"
	m.rmu.Unlock()
	m.Trimmed()
	m.follow(f.d.hub.Status())
	time.Sleep(100 * time.Millisecond)
	m.rmu.Lock()
	again := m.mountBoot
	m.rmu.Unlock()
	if again != "asked-again?" {
		t.Fatalf("storage.info was asked a second time in one boot")
	}
}

func TestARepairedMetadataBlockIsAWarningForItsBoot(t *testing.T) {
	f, m := mounting(t, []string{"repaired=2"}, nil)
	st := f.boot()
	got := f.awaitWarning(true, "no warning after the kernel repaired metadata", "2 metadata blocks")
	for _, want := range []string{"2 metadata blocks", "boot " + st.BootID, "Nothing was lost", "File contents are kept once and are not checked", "`fev logs --files --source kernel`", "`fev restart`"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not say %q:\n%s", want, got)
		}
	}
	m.KernelLine(st.BootID, standin.RepairLine)
	f.awaitWarning(true, "a repair after the mount was not counted", "3 metadata blocks")
	for _, line := range []string{
		"BTRFS info (device loop0): read error corrected: ino 0 off 31145984 (dev /dev/loop0 sector 77216)",
		"BTRFS info (device vda): read error corrected: ino 257 off 4096 (dev /dev/vda sector 9000)",
		"BTRFS error (device vda): bad tree block start, mirror 1 want 31145984 have 0",
		"something BTRFS info (device vda): read error corrected: ino 0 off 1 (dev /dev/vda sector 1)",
	} {
		m.KernelLine(st.BootID, line)
	}
	if w := f.warning("3 metadata blocks"); w == "" {
		t.Errorf("a line that is not the data disk's metadata was counted: %q", f.d.hub.Status().Warnings)
	}
	c := f.client()
	if _, err := c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	if f.warning("rewrote it from the second copy") == "" {
		t.Fatal("the warning went with the stop")
	}
	f.boot()
	f.awaitWarning(false, "the warning outlived a boot with no repair", "rewrote it from the second copy")
}

func TestNoDataFilesystemIsNotABudgetRefused(t *testing.T) {
	f, m := mounting(t, []string{"no-data=1"})
	if _, err := f.client().VMStart(bg, api.StartParams{}); err == nil {
		t.Fatal("the start succeeded with no data filesystem")
	}
	st := f.until("the engine's failure", func(st api.Status) bool { return st.Engine == api.EngineFailed })
	if !strings.Contains(st.Error, "the data disk cannot be used") {
		t.Fatalf("the status's error: %q", st.Error)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(f.logFile("fevd.log"), "no budget: the guest has no data filesystem that can be written") || !m.budgetIdle() {
		if time.Now().After(deadline) {
			t.Fatalf("the budget was never refused:\n%s", f.logFile("fevd.log"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if w := f.d.hub.Status().Warnings; len(w) != 0 {
		t.Fatalf("warnings beside the error: %q", w)
	}
}

func TestTheRecoveredWarningIsBuiltFromTheLaddersAccount(t *testing.T) {
	if got := RecoveredWarning(&agent.StorageInfo{MountRung: 1, Steps: []agent.MountStep{{Rung: 1, Name: "mount"}}}, "b1", "/x/data.img"); got != "" {
		t.Errorf("a normal mount: %q", got)
	}
	info := &agent.StorageInfo{MountRung: 4, ReadOnly: true, Steps: []agent.MountStep{
		{Rung: 1, Name: "mount", Error: "input/output error"},
		{Rung: 2, Name: "btrfs rescue super-recover", Skipped: "the first superblock is valid"},
		{Rung: 3, Name: "btrfs rescue zero-log", Skipped: "the filesystem has no fsync log to replay"},
		{Rung: 4, Name: "read-only rescue=usebackuproot"},
	}}
	got := RecoveredWarning(info, "b2", "/x/data.img")
	for _, want := range []string{"(boot b2)", "mounted read-only as of an older commit", "step 4 of 5, read-only rescue=usebackuproot",
		"the Docker engine is not running", "a database copied from it needs its own check", "Nothing on it was changed. ",
		"Every start of the VM finds it the same way until it is reset", "`fev stop` and copy that file", "/x/data.img",
		"`fev reset` then deletes it", "every image, container and volume on it is lost"} {
		if !strings.Contains(got, want) {
			t.Errorf("rung 4 read-only: no %q in %q", want, got)
		}
	}
	for _, not := range []string{"checks off", "Data was lost", "was recovered", "`fev restart`"} {
		if strings.Contains(got, not) {
			t.Errorf("rung 4 read-only says %q: %q", not, got)
		}
	}
	info.Steps[2] = agent.MountStep{Rung: 3, Name: "btrfs rescue zero-log", Error: "input/output error", Lost: "the log"}
	got = RecoveredWarning(info, "b2", "/x/data.img")
	if !strings.Contains(got, "Nothing on it was changed by this mount, but an earlier step of the recovery changed it: the log.") ||
		strings.Contains(got, "Nothing on it was changed. ") {
		t.Errorf("rung 4 after a dropped log: %q", got)
	}
	got = RecoveredWarning(&agent.StorageInfo{MountRung: 5, ReadOnly: true, Steps: append(info.Steps[:2:2],
		agent.MountStep{Rung: 3, Name: "btrfs rescue zero-log", Skipped: "the filesystem has no fsync log to replay"},
		agent.MountStep{Rung: 4, Name: "read-only rescue=usebackuproot", Error: "input/output error"},
		agent.MountStep{Rung: 5, Name: "read-only rescue=all"})}, "b4", "/x/data.img")
	if !strings.Contains(got, "read-only with its checks off") || !strings.Contains(got, "step 5 of 5, read-only rescue=all") ||
		strings.Contains(got, "older commit") || !strings.Contains(got, "`fev stop` and copy that file") || !strings.Contains(got, "`fev salvage DIR`") {
		t.Errorf("rung 5: %q", got)
	}
	if got := RecoveredWarning(&agent.StorageInfo{MountRung: 3}, "b3", "/x/data.img"); !strings.Contains(got, "(step 3 of 5)") {
		t.Errorf("no steps: %q", got)
	}
}

func TestADamagedSectorTheCheckFindsIsAWarningNamingSalvage(t *testing.T) {
	for _, c := range []struct {
		name     string
		script   []string
		followed bool
	}{
		{"ended before the daemon asked", []string{"mount-rung=2", "check-uncorrectable=8"}, false},
		{"still running when asked", []string{"mount-rung=3", "check-uncorrectable=8", "check-ms=2000"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, m := mounting(t, c.script, nil)
			st := f.boot()
			got := f.awaitWarning(true, "no warning after a check that found damage", "no good copy")
			for _, want := range []string{"8 sectors", "boot " + st.BootID, "after the data disk needed a recovery to mount", "`fev salvage DIR`", "`fev reset`", "`fev logs --files --source init`"} {
				if !strings.Contains(got, want) {
					t.Errorf("the warning does not say %q:\n%s", want, got)
				}
			}
			if !strings.Contains(strings.Join(f.d.hub.Status().Warnings, "\n"), "did not mount normally") {
				t.Errorf("the recovery's own warning is not beside it: %q", f.d.hub.Status().Warnings)
			}
			if followed := strings.Contains(f.logFile("fevd.log"), "check after the recovery is running; following it"); followed != c.followed {
				t.Errorf("the running check followed: %v, want %v\n%s", followed, c.followed, f.logFile("fevd.log"))
			}
			if _, err := f.client().VMStop(bg, api.StopParams{}); err != nil {
				t.Fatal(err)
			}
			f.stopped()
			f.boot()
			f.awaitWarning(false, "the check's warning outlived the next boot", "no good copy")
			m.host.Warn(WarnCheck, "held")
			m.forget()
			if w := f.warning("held"); w != "" {
				t.Errorf("the check's warning outlived a reset: %q", w)
			}
		})
	}
	t.Run("nothing found", func(t *testing.T) {
		f, m := mounting(t, []string{"mount-rung=2", "check-ms=200"})
		st := f.boot()
		f.awaitWarning(true, "no recovery warning", "did not mount normally")
		deadline := time.Now().Add(5 * time.Second)
		for {
			m.rmu.Lock()
			answered := m.mountBoot == st.BootID
			m.rmu.Unlock()
			if answered {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the mount was never reported")
			}
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(400 * time.Millisecond)
		if w := f.warning("no good copy"); w != "" {
			t.Errorf("a warning for a check that found nothing: %q", w)
		}
	})
}

func TestTheChecksWarningIsBuiltFromItsResult(t *testing.T) {
	for _, c := range []struct {
		check *agent.StorageCheck
		want  string
	}{
		{nil, ""},
		{&agent.StorageCheck{State: agent.OperationRunning, Auto: true}, ""},
		{&agent.StorageCheck{State: agent.OperationDone, Auto: true, Corrected: 4}, ""},
		{&agent.StorageCheck{State: agent.OperationFailed, Auto: true, Uncorrectable: 2, Error: "cancelled"}, ""},
		{&agent.StorageCheck{State: agent.OperationDone, Auto: true, Uncorrectable: 1}, "has a sector that failed its checksum"},
		{&agent.StorageCheck{State: agent.OperationDone, Uncorrectable: 3, ReadOnly: true}, "3 sectors that failed its checksum and had no good copy to repair it from (the check of the whole disk fev ran when asked, boot b9)"},
	} {
		got := CheckWarning(c.check, "b9")
		if (got == "") != (c.want == "") || !strings.Contains(got, c.want) {
			t.Errorf("%+v: %q, want %q", c.check, got, c.want)
		}
	}
}
