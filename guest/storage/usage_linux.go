package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/internal/btrfs"
)

func (v *Volume) Usage() (*api.StorageUsage, error) {
	if v.out.ReadOnly {
		return nil, errors.New(api.DataReadOnly)
	}
	if v.ctl == nil {
		return nil, fmt.Errorf("the data filesystem is at layout %d and has no quota groups", v.layout)
	}
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(v.ctl.Fd()), &st); err != nil {
		return nil, err
	}
	u := &api.StorageUsage{UsedBytes: int64(st.Blocks-st.Bfree) * st.Bsize, Machines: []api.MachineUsage{}}
	refs, err := btrfs.Subvolumes(v.root)
	if err != nil {
		return nil, fmt.Errorf("list the subvolumes: %w", err)
	}
	group := func(level, id uint64) int64 {
		n, err := v.readSysfsUint(fmt.Sprintf("qgroups/%d_%d/referenced", level, id))
		if err != nil {
			return 0
		}
		return int64(n)
	}
	top := v.cfg.Target
	if id, err := subvolumeID(filepath.Join(top, "@docker")); err == nil {
		u.EngineBytes = group(0, id)
		for _, x := range descendants(refs, id) {
			u.EngineBytes += group(0, x)
		}
	}
	machines := filepath.Join(top, MachinesSubvolume)
	if entries, err := os.ReadDir(machines); err == nil {
		for _, e := range entries {
			if api.CheckMachineName(e.Name()) != nil || !e.IsDir() {
				continue
			}
			id, err := subvolumeID(filepath.Join(machines, e.Name()))
			if err != nil {
				continue
			}
			u.Machines = append(u.Machines, api.MachineUsage{Name: e.Name(), DiskBytes: group(1, id)})
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	sort.Slice(u.Machines, func(i, j int) bool { return u.Machines[i].Name < u.Machines[j].Name })
	images := filepath.Join(machines, ImagesDir)
	counted := map[uint64]bool{}
	filepath.WalkDir(images, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(images, path)
		if rel == "." || !d.IsDir() {
			return nil
		}
		if strings.Count(rel, string(filepath.Separator)) < 4 {
			return nil
		}
		if id, err := subvolumeID(path); err == nil {
			u.ImagesBytes += group(0, id)
			counted[id] = true
		}
		return filepath.SkipDir
	})
	for _, id := range retiredBuilds(filepath.Join(images, retiredFile)) {
		if !counted[id] {
			counted[id] = true
			u.ImagesBytes += group(0, id)
		}
	}
	u.DebugBytes = v.DebugBytes()
	u.Kubernetes = v.kubernetesUsage()
	return u, nil
}

func (v *Volume) kubernetesUsage() *api.KubernetesUsage {
	_, errK := subvolumeID(filepath.Join(v.cfg.Target, K8sSubvolume))
	_, errC := subvolumeID(filepath.Join(v.cfg.Target, K8sCacheSubvolume))
	if errK != nil && errC != nil {
		return nil
	}
	k := &api.KubernetesUsage{ClusterBytes: v.K8sBytes(), CacheBytes: v.K8sCacheBytes()}
	k.Bytes = k.ClusterBytes + k.CacheBytes
	return k
}
