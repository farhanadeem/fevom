package machines

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"

	"fev/proto/relay"
)

var viewMu sync.Mutex

const ViewEngine = relay.EngineViewName

func EnsureView(dir, volumes string) error {
	viewMu.Lock()
	defer viewMu.Unlock()
	if err := ensureView(dir); err != nil {
		return err
	}
	if volumes == "" {
		return nil
	}
	if err := os.MkdirAll(volumes, 0o701); err != nil {
		return fmt.Errorf("the engine's volumes: %w", err)
	}
	at := filepath.Join(dir, ViewEngine, "volumes")
	if err := os.MkdirAll(at, 0o755); err != nil {
		return err
	}
	if sameFile(at, volumes) {
		return nil
	}
	if err := unix.Mount(volumes, at, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("show the engine's volumes at /mnt/machines/%s/volumes: %w", ViewEngine, err)
	}
	return nil
}

func sameFile(a, b string) bool {
	var x, y unix.Stat_t
	return unix.Stat(a, &x) == nil && unix.Stat(b, &y) == nil && x.Dev == y.Dev && x.Ino == y.Ino
}

func ensureView(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var here, up unix.Stat_t
	if err := unix.Stat(dir, &here); err != nil {
		return err
	}
	if err := unix.Stat(filepath.Dir(dir), &up); err != nil {
		return err
	}
	if here.Dev == up.Dev {
		if err := unix.Mount("tmpfs", dir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0755,size=1m"); err != nil {
			return fmt.Errorf("mount the machines' view at %s: %w", dir, err)
		}
	}
	if err := unix.Mount("", dir, "", unix.MS_SHARED, ""); err != nil {
		return fmt.Errorf("make the machines' view shared: %w", err)
	}
	return nil
}

func (rt *Runtime) viewAdd(name string) error {
	if rt.o.View == "" {
		return nil
	}
	viewMu.Lock()
	defer viewMu.Unlock()
	at := filepath.Join(rt.o.View, name)
	if err := os.Mkdir(at, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	src := filepath.Join(rt.o.DataDir, name)
	if sameFile(at, src) {
		return nil
	}
	if err := unix.Mount(src, at, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("show %s at /mnt/machines: %w", name, err)
	}
	return nil
}

func (rt *Runtime) viewRemove(name string) error {
	if rt.o.View == "" {
		return nil
	}
	viewMu.Lock()
	defer viewMu.Unlock()
	at := filepath.Join(rt.o.View, name)
	for {
		err := unix.Unmount(at, unix.MNT_DETACH)
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
			break
		}
		if err != nil {
			return fmt.Errorf("take %s out of /mnt/machines: %w", name, err)
		}
	}
	if err := unix.Rmdir(at); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}
