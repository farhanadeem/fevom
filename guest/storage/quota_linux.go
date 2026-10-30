package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/internal/btrfs"
)

var quotaGroup = btrfs.QgroupID(1, 0)

const (
	StepFormatStart    = "format-start"
	StepFormatMade     = "format-made"
	StepFormatClearing = "format-clearing"
)

const (
	StepQuotaEnabled   = "quota-enabled"
	StepGroupCreated   = "group-created"
	StepControlCreated = "control-created"
	StepAssigned       = "assigned"
)

func (v *Volume) step(name string) {
	if v.cfg.Step != nil {
		v.cfg.Step(name)
	}
}

func (v *Volume) EnsureQuota(root string) error {
	mode, err := v.readSysfs("qgroups/mode")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read the quota mode: %w", err)
	}
	switch mode {
	case "squota":
	case "", "disabled":
		if err := btrfs.EnableSimpleQuotas(v.root); err != nil {
			return fmt.Errorf("enable simple quotas: %w", err)
		}
		if mode, err = v.readSysfs("qgroups/mode"); err != nil || mode != "squota" {
			return fmt.Errorf("simple quotas were enabled and the quota mode reads %q (%v)", mode, err)
		}
	default:
		return fmt.Errorf("the filesystem has quotas in mode %q, not simple quotas: someone enabled them by hand; "+
			"disable them (btrfs quota disable) and start again", mode)
	}
	v.step(StepQuotaEnabled)

	if err := btrfs.CreateQgroup(v.root, quotaGroup); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("create the quota group 1/0: %w", err)
	}
	v.step(StepGroupCreated)

	ctlPath := filepath.Join(root, ControlSubvolume)
	if _, err := os.Lstat(ctlPath); errors.Is(err, fs.ErrNotExist) {
		if err := btrfs.CreateSubvolume(v.root, ControlSubvolume); err != nil {
			return fmt.Errorf("create subvolume %s: %w", ControlSubvolume, err)
		}
	} else if err != nil {
		return err
	}
	v.step(StepControlCreated)

	ctl, err := os.Open(ctlPath)
	if err != nil {
		return err
	}
	defer ctl.Close()
	ctlID, err := btrfs.SubvolumeID(ctl)
	if err != nil {
		return fmt.Errorf("read the id of %s: %w", ControlSubvolume, err)
	}
	if ctlID < 256 {
		return fmt.Errorf("%s is a directory of the top level, not a subvolume; remove it and start again", ControlSubvolume)
	}
	ids, err := v.subvolumeGroups()
	if err != nil {
		return fmt.Errorf("list the subvolumes' quota groups: %w", err)
	}
	for _, id := range ids {
		child := btrfs.QgroupID(0, id)
		if id == ctlID {
			if err := btrfs.RemoveQgroup(ctl, child, quotaGroup); err != nil && !errors.Is(err, unix.ENOENT) {
				return fmt.Errorf("take %s out of the quota group: %w", ControlSubvolume, err)
			}
			continue
		}
		if err := btrfs.AssignQgroup(ctl, child, quotaGroup); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("put subvolume %d into the quota group: %w", id, err)
		}
	}
	v.step(StepAssigned)
	return v.Sync()
}

func (v *Volume) openControl() error {
	if v.layout < 2 || v.out.ReadOnly {
		return nil
	}
	v.restoreQuota()
	f, err := os.Open(filepath.Join(v.cfg.Target, ControlSubvolume))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	v.ctl = f
	v.dropStaleLimit()
	return nil
}

const quotaRestoring = "quota-restoring"

func (v *Volume) restoreQuota() {
	ctl := filepath.Join(v.cfg.Target, ControlSubvolume)
	if _, err := os.Lstat(ctl); err != nil {
		return
	}
	mode, err := v.readSysfs("qgroups/mode")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return
	}
	mark := filepath.Join(ctl, quotaRestoring)
	_, markErr := os.Lstat(mark)
	_, groupErr := os.Stat(filepath.Join(v.sysfs, "qgroups", "1_0"))
	var why string
	switch {
	case mode != "squota":
		why = "quotas were off (disabled by hand)"
	case markErr == nil:
		why = "an earlier start was stopped while it put the quota group back"
	case errors.Is(groupErr, fs.ErrNotExist):
		why = "quotas were on and the group over everything was missing"
	default:
		return
	}
	if markErr != nil {
		f, err := os.OpenFile(mark, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			v.cfg.Logf("storage: %s on a filesystem at layout %d and the put-back could not be begun: %v", why, v.layout, err)
			return
		}
		f.Close()
	}
	if err := v.EnsureQuota(v.cfg.Target); err != nil {
		v.cfg.Logf("storage: %s on a filesystem at layout %d and they could not be put back: %v", why, v.layout, err)
		return
	}
	if err := os.Remove(mark); err != nil {
		v.cfg.Logf("storage: the quota group is back and its marker %s could not be removed: %v", mark, err)
		return
	}
	if err := v.Sync(); err != nil {
		v.cfg.Logf("storage: the quota group is back and the removal of its marker was not committed: %v", err)
	}
	v.cfg.Logf("storage: %s on a filesystem at layout %d; simple quotas and the group over everything are back, and what it held is not counted", why, v.layout)
}

func (v *Volume) dropStaleLimit() {
	g, err := v.group()
	if err != nil || !g.limited {
		return
	}
	if err := btrfs.LimitQgroup(v.ctl, quotaGroup, btrfs.NoLimit); err != nil {
		v.cfg.Logf("storage: the limit the last run left on the quota group (%d MiB) could not be removed: %v", g.limit>>20, err)
		return
	}
	v.cfg.Logf("storage: the limit the last run left (%d MiB over %d referenced) is removed; the daemon's budget sets the next", g.limit>>20, g.referenced>>20)
}

func (v *Volume) readSysfs(name string) (string, error) {
	if v.sysfs == "" {
		return "", errors.New("the filesystem's directory under /sys/fs/btrfs is not known")
	}
	b, err := os.ReadFile(filepath.Join(v.sysfs, name))
	return strings.TrimSpace(string(b)), err
}

func (v *Volume) readSysfsUint(name string) (uint64, error) {
	s, err := v.readSysfs(name)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(s, 10, 64)
}

func (v *Volume) subvolumeGroups() ([]uint64, error) {
	if v.sysfs == "" {
		return nil, errors.New("the filesystem's directory under /sys/fs/btrfs is not known")
	}
	entries, err := os.ReadDir(filepath.Join(v.sysfs, "qgroups"))
	if err != nil {
		return nil, err
	}
	var ids []uint64
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), "0_")
		if !ok || !e.IsDir() {
			continue
		}
		id, err := strconv.ParseUint(rest, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

type groupState struct {
	referenced, reserved, limit uint64
	limited                     bool
}

func (v *Volume) group() (groupState, error) {
	var g groupState
	var err error
	read := func(name string) uint64 {
		n, e := v.readSysfsUint("qgroups/1_0/" + name)
		if e != nil && err == nil {
			err = e
		}
		return n
	}
	g.referenced = read("referenced")
	g.reserved = read("rsv_data") + read("rsv_meta_pertrans") + read("rsv_meta_prealloc")
	g.limited = read("limit_flags")&1 != 0
	g.limit = read("max_referenced")
	return g, err
}

func (v *Volume) usedBytes() (uint64, error) {
	var total uint64
	for _, kind := range []string{"data", "metadata", "system"} {
		n, err := v.readSysfsUint("allocation/" + kind + "/disk_used")
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func (v *Volume) SetBudget(p api.BudgetParams) (*api.Budget, error) {
	start := v.cfg.NowMS()
	switch {
	case v.out.ReadOnly:
		return nil, errors.New(api.DataReadOnly)
	case v.ctl == nil:
		return nil, fmt.Errorf("the data filesystem is at layout %d and has no quota group", v.layout)
	}
	g, err := v.group()
	if err != nil {
		return nil, fmt.Errorf("read the quota group: %w", err)
	}
	used, err := v.usedBytes()
	if err != nil {
		return nil, fmt.Errorf("read the filesystem's used bytes: %w", err)
	}
	r := &api.Budget{ReferencedBytes: int64(g.referenced), ReservedBytes: int64(g.reserved), UsedBytes: int64(used)}
	if !p.Unlimited && p.ReclaimBytes > 0 && api.ReclaimableBytes(p.ImageBytes, int64(used), int64(g.reserved)) >= p.ReclaimBytes {
		r.Deferred = true
		r.Limited = g.limited
		if g.limited {
			r.LimitBytes = int64(g.limit)
			r.BudgetBytes = max(int64(g.limit)-int64(g.referenced), 0)
		}
		r.DurationMS = v.cfg.NowMS() - start
		return r, nil
	}
	limit := btrfs.NoLimit
	if !p.Unlimited {
		r.Limited = true
		r.BudgetBytes = api.BudgetBytes(p.AvailableBytes, p.MarginBytes, p.ImageBytes, int64(used), int64(g.reserved))
		r.LimitBytes = int64(g.referenced) + r.BudgetBytes
		limit = uint64(r.LimitBytes)
	}
	if err := btrfs.LimitQgroup(v.ctl, quotaGroup, limit); err != nil {
		return nil, fmt.Errorf("set the quota group's limit: %w", err)
	}
	r.DurationMS = v.cfg.NowMS() - start
	return r, nil
}
