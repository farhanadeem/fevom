package machines

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

const (
	runitStopit = "run/runit/stopit"
	runitReboot = "run/runit/reboot"
)

func markRunitHalt(rootFd int) error {
	mark := func(name string, set bool) error {
		fd, err := unix.Openat2(rootFd, name, &unix.OpenHow{
			Flags:   unix.O_RDONLY | unix.O_NONBLOCK | unix.O_NOFOLLOW | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
		})
		if errors.Is(err, unix.ENOENT) {
			if !set {
				return nil
			}
			dir, err := unix.Openat2(rootFd, "run/runit", &unix.OpenHow{
				Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
				Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
			})
			if err != nil {
				return fmt.Errorf("/run/runit: %w", err)
			}
			defer unix.Close(dir)
			fd, err = unix.Openat(dir, "stopit", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o100)
			if err != nil {
				return fmt.Errorf("/%s: %w", name, err)
			}
			defer unix.Close(fd)
			return unix.Fchmod(fd, 0o100)
		}
		if err != nil {
			return fmt.Errorf("/%s: %w", name, err)
		}
		defer unix.Close(fd)
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			return fmt.Errorf("/%s is not a file", name)
		}
		mode := st.Mode & 0o7777
		want := mode &^ 0o100
		if set {
			want = mode | 0o100
		}
		if want == mode {
			return nil
		}
		return unix.Fchmod(fd, want)
	}
	if err := mark(runitStopit, true); err != nil {
		return err
	}
	return mark(runitReboot, false)
}

func (b *boot) markRunitHaltIn() error {
	b.nsMu.RLock()
	defer b.nsMu.RUnlock()
	if b.ns == nil {
		return errors.New("the machine has stopped")
	}
	mnt := int(b.ns["mnt"].Fd())
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			done <- err
			return
		}
		if err := unix.Setns(mnt, unix.CLONE_NEWNS); err != nil {
			done <- fmt.Errorf("enter the machine's mount namespace: %w", err)
			return
		}
		root, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			done <- err
			return
		}
		defer unix.Close(root)
		done <- markRunitHalt(root)
	}()
	return <-done
}

func runitMarksFor(root string) error {
	f, err := os.Open(root)
	if err != nil {
		return err
	}
	defer f.Close()
	return markRunitHalt(int(f.Fd()))
}
