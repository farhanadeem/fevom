package storage

import (
	"errors"
	"fmt"
	"path/filepath"

	"fev/guest/internal/btrfs"
)

const DebugSubvolume = "@debug"

func (v *Volume) EnsureDebugSubvolume() (path string, created bool, err error) {
	if v.out.ReadOnly {
		return "", false, errors.New("the data disk is mounted read-only")
	}
	if v.ctl == nil {
		return "", false, fmt.Errorf("the data filesystem is at layout %d and has no quota groups", v.layout)
	}
	path = filepath.Join(v.cfg.Target, DebugSubvolume)
	if _, err := subvolumeID(path); err == nil {
		return path, false, nil
	}
	dir, err := openDir(v.cfg.Target)
	if err != nil {
		return "", false, err
	}
	defer dir.Close()
	if err := btrfs.CreateSubvolumeIn(dir, DebugSubvolume, nil); err != nil {
		return "", false, fmt.Errorf("make the debug subvolume: %w", err)
	}
	return path, true, nil
}

func (v *Volume) RemoveDebugSubvolume() error {
	if v.out.ReadOnly {
		return errors.New("the data disk is mounted read-only")
	}
	path := filepath.Join(v.cfg.Target, DebugSubvolume)
	if _, err := subvolumeID(path); err != nil {
		return nil
	}
	dir, err := openDir(v.cfg.Target)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := btrfs.DeleteSubvolume(dir, DebugSubvolume); err != nil {
		return fmt.Errorf("remove the debug subvolume: %w", err)
	}
	return nil
}

func (v *Volume) DebugBytes() int64 {
	id, err := subvolumeID(filepath.Join(v.cfg.Target, DebugSubvolume))
	if err != nil {
		return 0
	}
	n, err := v.readSysfsUint(fmt.Sprintf("qgroups/0_%d/referenced", id))
	if err != nil {
		return 0
	}
	return int64(n)
}
