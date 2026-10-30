package storage

import (
	"errors"
	"fmt"
	"path/filepath"

	"fev/guest/internal/btrfs"
)

const K8sSubvolume = "@k8s"

func (v *Volume) EnsureK8sSubvolume() (path string, created bool, err error) {
	return v.ensureTop(K8sSubvolume, "the cluster")
}

func (v *Volume) RemoveK8sSubvolume() error {
	return v.removeTop(K8sSubvolume, "the cluster")
}

func (v *Volume) K8sBytes() int64 { return v.topBytes(K8sSubvolume) }

const K8sCacheSubvolume = "@k8s-cache"

func (v *Volume) EnsureK8sCacheSubvolume() (path string, created bool, err error) {
	return v.ensureTop(K8sCacheSubvolume, "the cluster's cache")
}

func (v *Volume) RemoveK8sCacheSubvolume() error {
	return v.removeTop(K8sCacheSubvolume, "the cluster's cache")
}

func (v *Volume) K8sCacheBytes() int64 { return v.topBytes(K8sCacheSubvolume) }

func (v *Volume) ensureTop(name, what string) (path string, created bool, err error) {
	if v.out.ReadOnly {
		return "", false, errors.New("the data disk is mounted read-only")
	}
	if v.ctl == nil {
		return "", false, fmt.Errorf("the data filesystem is at layout %d and has no quota groups", v.layout)
	}
	path = filepath.Join(v.cfg.Target, name)
	if _, err := subvolumeID(path); err == nil {
		return path, false, nil
	}
	dir, err := openDir(v.cfg.Target)
	if err != nil {
		return "", false, err
	}
	defer dir.Close()
	if err := btrfs.CreateSubvolumeIn(dir, name, nil); err != nil {
		return "", false, fmt.Errorf("make %s's subvolume: %w", what, err)
	}
	return path, true, nil
}

func (v *Volume) removeTop(name, what string) error {
	if v.out.ReadOnly {
		return errors.New("the data disk is mounted read-only")
	}
	path := filepath.Join(v.cfg.Target, name)
	if _, err := subvolumeID(path); err != nil {
		return nil
	}
	dir, err := openDir(v.cfg.Target)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := btrfs.DeleteSubvolume(dir, name); err != nil {
		return fmt.Errorf("remove %s's subvolume: %w", what, err)
	}
	return nil
}

func (v *Volume) topBytes(name string) int64 {
	id, err := subvolumeID(filepath.Join(v.cfg.Target, name))
	if err != nil {
		return 0
	}
	n, err := v.readSysfsUint(fmt.Sprintf("qgroups/0_%d/referenced", id))
	if err != nil {
		return 0
	}
	return int64(n)
}
