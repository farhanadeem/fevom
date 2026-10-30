package nfsview

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

const overlayMagic = 0x794c7630

func enterMount(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("nfsview: %s: %w", path, err)
	}
	defer unix.Close(fd)
	if err := unix.Setns(fd, unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("nfsview: setns %s: %w", path, err)
	}
	return nil
}

func mountRoot(path string) (bool, error) {
	var a, b unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW|unix.AT_NO_AUTOMOUNT, unix.STATX_MNT_ID, &a); err != nil {
		return false, err
	}
	if err := unix.Statx(unix.AT_FDCWD, path+"/..", unix.AT_SYMLINK_NOFOLLOW|unix.AT_NO_AUTOMOUNT, unix.STATX_MNT_ID, &b); err != nil {
		return false, err
	}
	return a.Mnt_id != b.Mnt_id || (a.Dev_major == b.Dev_major && a.Dev_minor == b.Dev_minor && a.Ino == b.Ino), nil
}

func (n *Namespaces) CloneFrom(nsPath string, pid int, clones []Clone) (errs []error, err error) {
	errs = make([]error, len(clones))
	fds := make([]int, len(clones))
	for i := range fds {
		fds[i] = -1
	}
	defer func() {
		for _, fd := range fds {
			if fd >= 0 {
				unix.Close(fd)
			}
		}
	}()
	ch := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := EnterView(n.NetPath(), n.MntPath()); err != nil {
			ch <- err
			return
		}
		if err := enterMount(nsPath); err != nil {
			ch <- err
			return
		}
		if pid > 0 {
			if err := enterMount("/proc/" + strconv.Itoa(pid) + "/ns/mnt"); err != nil {
				ch <- err
				return
			}
		}
		for i, c := range clones {
			root, err := mountRoot(c.Src)
			if err == nil && !root {
				err = ErrNotMounted
			}
			if err != nil {
				errs[i] = err
				continue
			}
			fd, err := unix.OpenTree(unix.AT_FDCWD, c.Src, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_SYMLINK_NOFOLLOW)
			if err != nil {
				errs[i] = fmt.Errorf("nfsview: open_tree %s: %w", c.Src, err)
				continue
			}
			fds[i] = fd
			if err := checkClone(fd, c); err != nil {
				errs[i] = err
				unix.Close(fd)
				fds[i] = -1
			}
		}
		if err := unix.Setns(int(n.mnt.Fd()), unix.CLONE_NEWNS); err != nil {
			ch <- fmt.Errorf("nfsview: back to the view's mount namespace: %w", err)
			return
		}
		for i, c := range clones {
			if fds[i] < 0 {
				continue
			}
			errs[i] = attach(fds[i], c.Dst)
		}
		ch <- nil
	}()
	err = <-ch
	return errs, err
}

func checkClone(fd int, c Clone) error {
	if c.Overlay {
		var st unix.Statfs_t
		if err := unix.Fstatfs(fd, &st); err != nil {
			return err
		}
		if st.Type != overlayMagic {
			return ErrNotOverlay
		}
	}
	if c.Dir {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return fmt.Errorf("nfsview: %s is not a directory", c.Src)
		}
	}
	return nil
}

func attach(fd int, dst string) error {
	attr := unix.MountAttr{Propagation: unix.MS_PRIVATE}
	if err := unix.MountSetattr(fd, "", unix.AT_EMPTY_PATH|unix.AT_RECURSIVE, &attr); err != nil {
		return fmt.Errorf("nfsview: the copy for %s private: %w", dst, err)
	}
	if err := unix.MoveMount(fd, "", unix.AT_FDCWD, dst, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("nfsview: move the copy to %s: %w", dst, err)
	}
	return nil
}

func bindHere(src, dst string) error {
	fd, err := unix.OpenTree(unix.AT_FDCWD, src, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return fmt.Errorf("nfsview: open_tree %s: %w", src, err)
	}
	defer unix.Close(fd)
	return attach(fd, dst)
}

func detachHere(path string) error {
	for {
		err := unix.Unmount(path, unix.MNT_DETACH)
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("nfsview: detach %s: %w", path, err)
		}
	}
}

func moveHere(from, to string) error {
	if err := unix.MoveMount(unix.AT_FDCWD, from, unix.AT_FDCWD, to, 0); err != nil {
		return fmt.Errorf("nfsview: move %s to %s: %w", from, to, err)
	}
	return nil
}

const UnlockFilesystem = NFSDDir + "/unlock_filesystem"

func releaseHere(path string, whole bool) error {
	root, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("nfsview: %s: %w", path, err)
	}
	defer unix.Close(root)
	if ok, _ := mountRoot(path); !ok {
		return nil
	}
	at := "/proc/self/fd/" + strconv.Itoa(root)
	var rerr error
	if whole {
		rerr = os.WriteFile(UnlockFilesystem, []byte(at+"/.\n"), 0)
	} else {
		rerr = unlockExport(at)
	}
	derr := detachHere(path)
	if rerr != nil {
		rerr = fmt.Errorf("nfsview: revoke the state in %s: %w", path, rerr)
	}
	return errors.Join(rerr, derr)
}

func (n *Namespaces) Bind(src, dst string) error {
	return n.Do(func() error { return bindHere(src, dst) })
}

func (n *Namespaces) Move(from, to string) error {
	return n.Do(func() error { return moveHere(from, to) })
}

func (n *Namespaces) Release(path string, whole bool) error {
	return n.Do(func() error { return releaseHere(path, whole) })
}

func (n *Namespaces) Flush() error {
	return n.Do(func() error { return FlushExports(time.Now()) })
}

func (n *Namespaces) MakeDir(path string) error {
	return n.Do(func() error { return MountPoint(path) })
}

func (n *Namespaces) RemoveDir(path string) error {
	return n.Do(func() error { return RemoveMountPoint(path) })
}
