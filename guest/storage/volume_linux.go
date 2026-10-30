package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/internal/btrfs"
)

type Config struct {
	Device, Target string
	ToolDir        string
	Run            func(limit time.Duration, path string, args ...string) ([]byte, error)
	Logf           func(format string, args ...any)
	NowMS          func() float64
	Build          string
	DataCow        bool
	Allocated      int64
	Step           func(name string)
}

type Volume struct {
	cfg       Config
	fstype    string
	layout    int
	out       Outcome
	format    string
	root      *os.File
	ctl       *os.File
	sysfs     string
	trimming  sync.Mutex
	checkMu   sync.Mutex
	check     *api.StorageCheck
	checkDone chan struct{}
}

const (
	mkfsLimit   = 60 * time.Second
	rescueLimit = 5 * time.Minute
)

var ErrVerity = errors.New("has had fs-verity files written to it (the engine of a fev build of 2026-10-10 to " +
	"2026-10-13 made one at every start and stored every image that way; a container can have enabled it too) " +
	"and this build's kernel has no fs-verity: any such file still on it cannot be opened. " +
	"The disk was not mounted and not changed. `fev reset` makes a new one " +
	"(its images, containers and volumes are lost; the earlier build still reads it)")

func kernelHasVerity() bool {
	_, err := os.Stat("/sys/fs/btrfs/features/verity")
	return err == nil
}

type device struct {
	cfg Config
}

func (d device) Mount(options string, readOnly bool) error {
	flags := uintptr(unix.MS_NOATIME | unix.MS_NOSUID | unix.MS_NODEV)
	if readOnly {
		flags |= unix.MS_RDONLY
	}
	if err := unix.Mount(d.cfg.Device, d.cfg.Target, "btrfs", flags, options); err != nil {
		return fmt.Errorf("mount -o %s: %w (the kernel log has the reason)", options, err)
	}
	return nil
}

func (d device) Unmount() error { return unix.Unmount(d.cfg.Target, 0) }

func (d device) Tool(args ...string) (string, error) {
	tool := filepath.Join(d.cfg.ToolDir, "btrfs")
	if d.cfg.ToolDir == "" || !isFile(tool) {
		return "", ErrNoTool
	}
	out, err := d.cfg.Run(rescueLimit, tool, append(args, d.cfg.Device)...)
	return string(out), err
}

func (d device) Probe() (Probe, error) { return probePath(d.cfg.Device) }

func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func probePath(path string) (Probe, error) {
	f, err := os.Open(path)
	if err != nil {
		return Probe{}, err
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return Probe{}, err
	}
	return ProbeDevice(f, size)
}

func ProbePath(path string) (Probe, error) { return probePath(path) }

func Open(cfg Config) (*Volume, error) {
	t0 := cfg.NowMS()
	first, err := probePath(cfg.Device)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", cfg.Device, err)
	}
	if err := os.MkdirAll(cfg.Target, 0o755); err != nil {
		return nil, err
	}
	v := &Volume{cfg: cfg, fstype: "btrfs"}
	formatted := false
	if why := BlankButOccupied(first, cfg.Allocated); why != "" {
		return nil, fmt.Errorf("%s %s. It was not formatted and not changed. %s %s", cfg.Device, why, KeepRemedy, ResetRemedy)
	}
	if first.Kind == Interrupted && first.Unfinished {
		if first, err = clearUnfinished(cfg, first); err != nil {
			return nil, err
		}
		v.format = api.FormatRepeated
	}
	switch first.Kind {
	case Blank:
		mkfs := filepath.Join(cfg.ToolDir, "mkfs.btrfs")
		if cfg.ToolDir == "" || !isFile(mkfs) {
			return nil, fmt.Errorf("%s is blank and there is no system image to format it with", cfg.Device)
		}
		v.step(StepFormatStart)
		out, err := cfg.Run(mkfsLimit, mkfs, append(append([]string{}, MkfsOptions...), cfg.Device)...)
		if err != nil {
			return nil, fmt.Errorf("format %s: %w: %s", cfg.Device, err, lastLine(string(out)))
		}
		if first, err = probePath(cfg.Device); err != nil || !first.PrimaryValid {
			return nil, fmt.Errorf("format %s: no valid superblock afterwards (%v)", cfg.Device, err)
		}
		formatted = true
		if v.format == "" {
			v.format = api.FormatNew
		}
		cfg.Logf("storage: %s was blank; formatted in %.1f ms", cfg.Device, cfg.NowMS()-t0)
		v.step(StepFormatMade)
	case Btrfs:
		if first.Verity && !kernelHasVerity() {
			return nil, fmt.Errorf("%s %w", cfg.Device, ErrVerity)
		}
	case Interrupted:
		return nil, fmt.Errorf("%s holds %s, and it cannot be shown that nothing else was ever on it (%s); it was not touched. %s",
			cfg.Device, first.What, first.NotProven, ResetRemedy)
	case Foreign:
		return nil, fmt.Errorf("%s holds %s, not a fev data filesystem; it was not touched. %s", cfg.Device, first.What, ResetRemedy)
	}

	t1 := cfg.NowMS()
	options := MountOptions
	if cfg.DataCow {
		options = MountOptionsCow
		cfg.Logf("storage: fev.data_cow is set: new files are copied on write and checksummed")
	}
	v.out, err = Climb(device{cfg}, first, options, cfg.Logf)
	if err != nil {
		return nil, err
	}
	if err := v.openRoot(); err != nil {
		unix.Unmount(cfg.Target, 0)
		return nil, err
	}
	t2 := cfg.NowMS()
	if stats, err := btrfs.GetDevStats(v.root, 1); err == nil && stats != (btrfs.DevStats{}) {
		cfg.Logf("storage: the filesystem has recorded errors: %+v", stats)
	}
	if v.out.ReadOnly {
		if s, err := ReadStamp(cfg.Target); err == nil {
			v.layout = s.Layout
		}
		v.autoCheck()
		return v, nil
	}
	fail := func(err error) (*Volume, error) {
		v.Close()
		unix.Unmount(cfg.Target, 0)
		return nil, err
	}
	if fsi, err := btrfs.GetFSInfo(v.root); err == nil {
		v.sysfs = "/sys/fs/btrfs/" + formatFSID(fsi.FSID)
	}
	stamp := Stamp{CreatedBy: cfg.Build, Mkfs: strings.Join(MkfsOptions, " ")}
	laidOut := false
	if v.layout, laidOut, err = ensureLayout(cfg.Target, v, stamp, cfg.Logf); err != nil {
		return fail(err)
	}
	if laidOut && !formatted {
		v.format = api.FormatFinished
		cfg.Logf("storage: an earlier first start stopped after the format and before the layout was whole; finished it")
	}
	if err := v.openControl(); err != nil {
		return fail(err)
	}
	if err := v.grow(); err != nil {
		return fail(err)
	}
	v.checkMachines()
	t3 := cfg.NowMS()
	if formatted || laidOut {
		r, err := v.Trim(0)
		if err != nil {
			cfg.Logf("storage: first trim failed: %v", err)
		} else if !formatted {
			cfg.Logf("storage: mounted on rung %d in %.1f ms, first layout finished in %.1f ms, trim %.1f ms (%d bytes)",
				v.out.Rung, t2-t1, t3-t2, r.DurationMS, r.TrimmedBytes)
		} else {
			cfg.Logf("storage: first boot: format and mount %.1f ms, layout %.1f ms, trim %.1f ms (%d bytes)",
				t2-t0, t3-t2, r.DurationMS, r.TrimmedBytes)
		}
	} else {
		cfg.Logf("storage: mounted on rung %d in %.1f ms, layout %d checked in %.1f ms", v.out.Rung, t2-t1, v.layout, t3-t2)
	}
	v.autoCheck()
	return v, nil
}

func (v *Volume) checkMachines() {
	ms, err := v.Machines()
	if err != nil {
		return
	}
	said := ms.ClearScratch()
	more, err := ms.CheckMembership()
	said = append(said, more...)
	for _, s := range said {
		v.cfg.Logf("storage: %s", s)
	}
	if err != nil {
		v.cfg.Logf("storage: the machines' quota groups were not checked: %v", err)
	}
}

func (v *Volume) autoCheck() {
	if AutoCheck(v.out) {
		v.StartCheck(true)
		v.cfg.Logf("storage: mounted on rung %d: checking the whole filesystem once, at idle I/O priority (storage.info has its progress)", v.out.Rung)
	}
}

func clearUnfinished(cfg Config, first Probe) (Probe, error) {
	if cfg.ToolDir == "" || !isFile(filepath.Join(cfg.ToolDir, "mkfs.btrfs")) {
		return first, fmt.Errorf("%s holds %s and there is no system image to format it again with", cfg.Device, first.What)
	}
	t0 := cfg.NowMS()
	refuse := func(what string, err error) (Probe, error) {
		return first, fmt.Errorf("%s holds %s and could not be cleared to format it again (%s: %v). %s",
			cfg.Device, first.What, what, err, ResetRemedy)
	}
	f, err := os.OpenFile(cfg.Device, os.O_RDWR|unix.O_EXCL, 0)
	if err != nil {
		return refuse("open", err)
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return refuse("size", err)
	}
	for i, r := range [][2]uint64{{headLen, uint64(size) - headLen}, {0, headLen}} {
		if i == 1 && cfg.Step != nil {
			cfg.Step(StepFormatClearing)
		}
		if err := btrfs.DiscardRange(f, r[0], r[1]); err != nil {
			f.Close()
			return refuse(fmt.Sprintf("discard %d bytes at %d", r[1], r[0]), err)
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return refuse("flush", err)
		}
	}
	if err := f.Close(); err != nil {
		return refuse("close", err)
	}
	now, err := probePath(cfg.Device)
	if err != nil {
		return refuse("read it again", err)
	}
	if now.Kind != Blank {
		return refuse("read it again", fmt.Errorf("after the discard it holds %s, not zeros", now.What))
	}
	cfg.Logf("storage: %s held %s: an earlier first start was stopped while it formatted, so nothing was ever on it; "+
		"cleared in %.1f ms, formatting again", cfg.Device, first.What, cfg.NowMS()-t0)
	return now, nil
}

func (v *Volume) openRoot() error {
	f, err := os.Open(v.cfg.Target)
	if err != nil {
		return err
	}
	v.root = f
	return nil
}

func (v *Volume) grow() error {
	dev, err := os.Open(v.cfg.Device)
	if err != nil {
		return err
	}
	size, err := dev.Seek(0, io.SeekEnd)
	dev.Close()
	if err != nil {
		return err
	}
	info, err := btrfs.GetDevInfo(v.root, 1)
	if err != nil {
		return fmt.Errorf("read device size from the filesystem: %w", err)
	}
	if want := uint64(size) &^ 4095; info.TotalBytes < want {
		if err := btrfs.GrowToDevice(v.root, 1); err != nil {
			return fmt.Errorf("grow the filesystem to %d bytes: %w", want, err)
		}
		v.cfg.Logf("storage: grew the filesystem from %d to %d bytes", info.TotalBytes, want)
	}
	return nil
}

func (v *Volume) CreateSubvolume(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return btrfs.CreateSubvolume(dir, filepath.Base(path))
}

func (v *Volume) Sync() error {
	return btrfs.Sync(v.root)
}

func (v *Volume) Outcome() Outcome { return v.out }

func (v *Volume) FSType() string { return v.fstype }

func (v *Volume) Info() (*api.StorageInfo, error) {
	info := &api.StorageInfo{
		Device: v.cfg.Device, FSType: v.fstype, Layout: v.layout,
		MountRung: v.out.Rung, ReadOnly: v.out.ReadOnly, Format: v.format,
	}
	for _, s := range v.out.Steps {
		info.Steps = append(info.Steps, api.MountStep{Rung: s.Rung, Name: s.Name, Error: s.Err, Skipped: s.Skipped, Lost: s.Lost})
	}
	at := v.root
	if v.ctl != nil {
		at = v.ctl
	}
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(at.Fd()), &st); err != nil {
		return nil, err
	}
	info.SizeBytes = int64(st.Blocks) * st.Bsize
	info.UsedBytes = int64(st.Blocks-st.Bfree) * st.Bsize
	fsi, err := btrfs.GetFSInfo(v.root)
	if err != nil {
		return nil, err
	}
	info.FSID = formatFSID(fsi.FSID)
	if v.ctl != nil {
		if g, err := v.group(); err == nil {
			info.Quota = &api.StorageQuota{Limited: g.limited, ReferencedBytes: int64(g.referenced), ReservedBytes: int64(g.reserved)}
			if g.limited {
				info.Quota.LimitBytes = int64(g.limit)
			}
		}
	}
	stats, err := btrfs.GetDevStats(v.root, 1)
	if err != nil {
		return nil, err
	}
	info.Errors = api.StorageErrors{
		Write: stats.Write, Read: stats.Read, Flush: stats.Flush,
		Corruption: stats.Corruption, Generation: stats.Generation,
	}
	info.Check = v.Check()
	if swap, err := SwapDevices("/proc", "/sys"); err == nil {
		info.Swap = swap
	}
	return info, nil
}

func (v *Volume) Trim(minLen uint64) (*api.TrimResult, error) {
	if v.out.ReadOnly {
		return nil, errors.New(api.DataReadOnly)
	}
	v.trimming.Lock()
	defer v.trimming.Unlock()
	start := v.cfg.NowMS()
	if err := v.Sync(); err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}
	if err := btrfs.WaitDeletedSubvolumes(v.root); err != nil {
		return nil, fmt.Errorf("wait for deleted subvolumes: %w", err)
	}
	if err := v.Sync(); err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}
	n, err := btrfs.Trim(v.root, minLen)
	if err != nil {
		return nil, fmt.Errorf("trim: %w", err)
	}
	return &api.TrimResult{TrimmedBytes: n, DurationMS: v.cfg.NowMS() - start}, nil
}

func (v *Volume) Close() {
	v.stopCheck()
	if v.ctl != nil {
		v.ctl.Close()
		v.ctl = nil
	}
	v.root.Close()
}

func formatFSID(u [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
