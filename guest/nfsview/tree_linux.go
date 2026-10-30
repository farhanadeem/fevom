package nfsview

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const rootSizeMiB = 4

func (t *Tree) MakeRoot() error {
	if err := os.MkdirAll(t.root, 0o700); err != nil {
		return err
	}
	opts := fmt.Sprintf("mode=0700,uid=%d,gid=%d,size=%dm,nr_inodes=65536", t.owner.UID, t.owner.GID, rootSizeMiB)
	if err := unix.Mount("tmpfs", t.root, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, opts); err != nil {
		return fmt.Errorf("nfsview: mount the tree's root at %s: %w", t.root, err)
	}
	for _, f := range RootFiles() {
		p := filepath.Join(t.root, f.Name)
		var err error
		if f.Dir {
			err = os.Mkdir(p, os.FileMode(f.Mode))
		} else {
			err = os.WriteFile(p, []byte(f.Data), os.FileMode(f.Mode))
		}
		if err == nil {
			err = os.Chmod(p, os.FileMode(f.Mode))
		}
		if err != nil {
			return fmt.Errorf("nfsview: %s: %w", p, err)
		}
	}
	return nil
}

func MountPoint(path string) error {
	if err := os.Mkdir(path, 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

func RemoveMountPoint(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
