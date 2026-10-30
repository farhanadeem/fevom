package storage

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

type fakeDevice struct {
	superBad    bool
	logBad      bool
	rootBad     bool
	backupsGone bool
	hopeless    bool
	noTool      bool
	stuck       bool
	calls       []string
}

func (f *fakeDevice) Mount(options string, readOnly bool) error {
	f.calls = append(f.calls, fmt.Sprintf("mount %s ro=%v", options, readOnly))
	salvage := readOnly && strings.Contains(options, "rescue=all")
	switch {
	case f.hopeless, f.superBad:
		return errors.New("EINVAL")
	case salvage:
		return nil
	case f.rootBad && (f.backupsGone || !strings.Contains(options, "usebackuproot")):
		return errors.New("EIO")
	case f.logBad && !strings.Contains(options, "nologreplay") && !strings.Contains(options, "usebackuproot"):
		return errors.New("EIO")
	}
	return nil
}

func (f *fakeDevice) Unmount() error {
	f.calls = append(f.calls, "unmount")
	if f.stuck {
		return errors.New("EBUSY")
	}
	return nil
}

func (f *fakeDevice) Tool(args ...string) (string, error) {
	f.calls = append(f.calls, "btrfs "+strings.Join(args, " "))
	if f.noTool {
		return "", ErrNoTool
	}
	switch strings.Join(args, " ") {
	case "rescue super-recover -y":
		f.superBad = false
		return "Recovered bad superblocks successful", exitStatus(2)
	case "rescue zero-log":
		f.logBad = false
	default:
		return "unknown", errors.New("exit status 1")
	}
	return "", nil
}

type exitStatus int

func (e exitStatus) Error() string   { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitStatus) ExitStatus() int { return int(e) }

func (f *fakeDevice) Probe() (Probe, error) {
	p := Probe{Kind: Btrfs, PrimaryValid: !f.superBad, MirrorValid: true}
	if f.logBad {
		p.LogRoot = 4096
	}
	return p, nil
}

func TestLadderStopsAtTheLeastCostlyRungThatWorks(t *testing.T) {
	cases := []struct {
		name     string
		dev      fakeDevice
		rung     int
		lossy    bool
		readOnly bool
		fails    bool
	}{
		{name: "healthy", rung: RungNormal},
		{name: "bad first superblock", dev: fakeDevice{superBad: true}, rung: RungSuperRecover},
		{name: "bad log", dev: fakeDevice{logBad: true}, rung: RungZeroLog, lossy: true},
		{name: "bad tree root", dev: fakeDevice{rootBad: true}, rung: RungBackupRoot, readOnly: true},
		{name: "bad tree root, backups gone", dev: fakeDevice{rootBad: true, backupsGone: true}, rung: RungSalvage, readOnly: true},
		{name: "bad log and bad tree root", dev: fakeDevice{logBad: true, rootBad: true}, rung: RungBackupRoot, readOnly: true},
		{name: "nothing mounts", dev: fakeDevice{hopeless: true}, fails: true},
		{name: "bad first superblock, no tool", dev: fakeDevice{superBad: true, noTool: true}, fails: true},
		{name: "bad log, no tool", dev: fakeDevice{logBad: true, noTool: true}, rung: RungBackupRoot, readOnly: true},
	}
	for _, c := range cases {
		dev := c.dev
		first, _ := dev.Probe()
		var log []string
		out, err := Climb(&dev, first, MountOptions, func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) })
		if c.fails {
			if err == nil || !strings.Contains(err.Error(), "left as it is") || out.Rung != 0 {
				t.Errorf("%s: out=%+v err=%v", c.name, out, err)
			}
			continue
		}
		if err != nil || out.Rung != c.rung || out.ReadOnly != c.readOnly {
			t.Errorf("%s: rung %d readOnly %v err %v; calls %q", c.name, out.Rung, out.ReadOnly, err, dev.calls)
			continue
		}
		last := out.Steps[len(out.Steps)-1]
		if last.Rung != c.rung || last.Lossy != c.lossy || last.Err != "" {
			t.Errorf("%s: last step %+v", c.name, last)
		}
		for i, s := range out.Steps {
			if s.Rung != i+1 {
				t.Errorf("%s: step %d is rung %d", c.name, i, s.Rung)
			}
			if i < len(out.Steps)-1 && s.Err == "" && s.Skipped == "" {
				t.Errorf("%s: rung %d neither failed nor was skipped", c.name, s.Rung)
			}
		}
		if loud := strings.Contains(strings.Join(log, "\n"), "DATA MAY BE LOST"); loud != c.lossy {
			t.Errorf("%s: lossy=%v but the log is %q", c.name, c.lossy, log)
		}
		if c.readOnly && !strings.Contains(strings.Join(log, "\n"), "READ-ONLY") {
			t.Errorf("%s: salvage not announced: %q", c.name, log)
		}
	}
}

func TestLadderRunsAToolOnlyOnEvidence(t *testing.T) {
	ran := func(dev *fakeDevice, what string) bool {
		first, _ := dev.Probe()
		Climb(dev, first, MountOptions, func(string, ...any) {})
		for _, c := range dev.calls {
			if strings.Contains(c, what) {
				return true
			}
		}
		return false
	}
	if ran(&fakeDevice{rootBad: true}, "super-recover") {
		t.Error("super-recover ran with a valid first superblock")
	}
	if ran(&fakeDevice{rootBad: true}, "zero-log") {
		t.Error("zero-log ran on a filesystem with no log")
	}
	if ran(&fakeDevice{logBad: true, rootBad: true}, "zero-log") {
		t.Error("zero-log ran although the filesystem does not mount without the log either")
	}
	stuck := &fakeDevice{logBad: true, stuck: true}
	first, _ := stuck.Probe()
	if _, err := Climb(stuck, first, MountOptions, func(string, ...any) {}); err == nil {
		t.Error("the ladder went on after an unmount failed")
	}
	for _, c := range stuck.calls {
		if strings.HasPrefix(c, "btrfs") {
			t.Errorf("ran %q with the trial mount still in place", c)
		}
	}
	healthy := &fakeDevice{}
	first, _ = healthy.Probe()
	Climb(healthy, first, MountOptions, func(string, ...any) {})
	if len(healthy.calls) != 1 {
		t.Errorf("a healthy mount took %q", healthy.calls)
	}
}

func TestMkfsOptionsAreExplicit(t *testing.T) {
	got := strings.Join(MkfsOptions, " ")
	for _, want := range []string{"-n 16384", "-s 4096", "--csum crc32c", "-m dup", "-d single",
		"free-space-tree", "block-group-tree", "no-holes", "-K"} {
		if !strings.Contains(got, want) {
			t.Errorf("mkfs options %q lack %q", got, want)
		}
	}
	for _, o := range []string{MountOptions, MountOptionsCow} {
		if strings.Contains(o, "discard=sync") || !strings.Contains(o, "discard=async") {
			t.Errorf("mount options %q", o)
		}
	}
}

type errnoDevice struct {
	fakeDevice
	first error
}

func (d *errnoDevice) Mount(options string, readOnly bool) error {
	if len(d.calls) == 0 {
		d.calls = append(d.calls, "mount "+options)
		return fmt.Errorf("mount -o %s: %w", options, d.first)
	}
	return d.fakeDevice.Mount(options, readOnly)
}

func TestLadderDoesNotClimbWhenTheDiskIsNotAtFault(t *testing.T) {
	for _, errno := range []error{unix.ENOMEM, unix.EBUSY, unix.ENODEV, unix.EINTR} {
		dev := &errnoDevice{first: errno}
		var log []string
		out, err := Climb(dev, Probe{Kind: Btrfs, PrimaryValid: true, MirrorValid: true, LogRoot: 4096}, MountOptions,
			func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) })
		if err == nil || out.Rung != 0 || len(dev.calls) != 1 {
			t.Errorf("%v: out=%+v err=%v calls=%q", errno, out, err, dev.calls)
			continue
		}
		if !strings.Contains(err.Error(), "not a sign of damage") || strings.Contains(strings.Join(log, "\n"), "DATA MAY BE LOST") {
			t.Errorf("%v: err=%v log=%q", errno, err, log)
		}
	}
	dev := &errnoDevice{first: unix.EIO}
	if out, err := Climb(dev, Probe{Kind: Btrfs, PrimaryValid: true}, MountOptions, func(string, ...any) {}); err != nil || out.Rung != RungBackupRoot || !out.ReadOnly {
		t.Errorf("EIO: out=%+v err=%v", out, err)
	}
}

func TestAnOlderTreeRootIsOnlyEverMountedReadOnly(t *testing.T) {
	for _, dev := range []fakeDevice{{rootBad: true}, {logBad: true, rootBad: true}, {rootBad: true, backupsGone: true}} {
		d := dev
		first, _ := d.Probe()
		var log []string
		out, _ := Climb(&d, first, MountOptions, func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) })
		for _, c := range d.calls {
			if !strings.Contains(c, "usebackuproot") {
				continue
			}
			if !strings.HasSuffix(c, "ro=true") || !strings.Contains(c, "nologreplay") {
				t.Errorf("%+v: %s", dev, c)
			}
		}
		for _, s := range out.Steps {
			if s.Rung == RungBackupRoot && (s.Lossy || s.Lost != "") {
				t.Errorf("%+v: rung 4 gave something up: %+v", dev, s)
			}
		}
		if out.Rung == RungBackupRoot && !strings.Contains(strings.Join(log, "\n"), ViewRoot) {
			t.Errorf("%+v: the log does not say what the view is: %q", dev, log)
		}
	}
}

func TestEveryReadWriteMountRewritesDataInPlace(t *testing.T) {
	for _, dev := range []fakeDevice{{}, {superBad: true}, {logBad: true}, {rootBad: true}, {rootBad: true, backupsGone: true}} {
		for _, options := range []string{MountOptions, MountOptionsCow} {
			d := dev
			first, _ := d.Probe()
			Climb(&d, first, options, func(string, ...any) {})
			for _, c := range d.calls {
				if !strings.HasPrefix(c, "mount ") || strings.HasSuffix(c, "ro=true") {
					continue
				}
				inPlace := strings.Contains(c, "nodatacow") && strings.Contains(c, "nodatasum")
				if inPlace != (options == MountOptions) || !strings.Contains(c, "discard=async") {
					t.Errorf("%+v with %q: %s", dev, options, c)
				}
			}
		}
	}
	if strings.Contains(MountOptions, "compress") || strings.Contains(MountOptionsCow, "nodata") {
		t.Errorf("mount options %q and %q", MountOptions, MountOptionsCow)
	}
}
