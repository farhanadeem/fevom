package macexec

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"

	"fev/guest/internal/binfmt"
	api "fev/proto/agent"
)

const Dir = "/proc/sys/fs/binfmt_misc"

func Mount(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "register")); err == nil {
		return nil
	}
	if err := unix.Mount("binfmt_misc", dir, "binfmt_misc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil && !errors.Is(err, unix.EBUSY) {
		return fmt.Errorf("mount binfmt_misc at %s: %w", dir, err)
	}
	return nil
}

func Ensure(dir, interp string, entries []Entry) ([]string, error) {
	var added []string
	var errs []error
	var table []api.EmulationEntry
	read := false
	for _, e := range entries {
		if _, err := os.Lstat(filepath.Join(dir, e.Name)); err == nil {
			continue
		}
		if !read {
			_, table, _ = binfmt.Table{Dir: dir}.Entries()
			read = true
		}
		if ahead(table, e) {
			continue
		}
		f, err := os.OpenFile(filepath.Join(dir, "register"), os.O_WRONLY, 0)
		if err != nil {
			return added, err
		}
		_, err = f.WriteString(e.Line(interp))
		f.Close()
		switch {
		case err == nil:
			added = append(added, e.Name)
		case errors.Is(err, unix.EEXIST):
		default:
			errs = append(errs, fmt.Errorf("register %s: %w", e.Name, err))
		}
	}
	return added, errors.Join(errs...)
}

func ahead(table []api.EmulationEntry, e Entry) bool {
	for _, t := range table {
		if t.Name != e.Name && t.Enabled && binfmt.Overlaps(t, e.Offset, e.Magic, e.Mask) {
			return true
		}
	}
	return false
}

func Watch(dir, interp string, entries []Entry, logf func(format string, args ...any)) error {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return err
	}
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_DELETE|unix.IN_MODIFY); err != nil {
		unix.Close(fd)
		return fmt.Errorf("watch %s: %w", dir, err)
	}
	f := os.NewFile(uintptr(fd), "binfmt_misc-watch")
	defer f.Close()
	buf := make([]byte, 64*(unix.SizeofInotifyEvent+unix.NAME_MAX+1))
	for {
		n, err := f.Read(buf)
		if err != nil {
			return err
		}
		if !touches(buf[:n]) {
			continue
		}
		added, err := Ensure(dir, interp, entries)
		if len(added) > 0 {
			logf("binfmt_misc: put back %v", added)
		}
		if err != nil {
			logf("binfmt_misc: %v", err)
		}
	}
}

func touches(b []byte) bool {
	for len(b) >= unix.SizeofInotifyEvent {
		ev := (*unix.InotifyEvent)(unsafe.Pointer(&b[0]))
		end := unix.SizeofInotifyEvent + int(ev.Len)
		if end > len(b) {
			return false
		}
		name := string(bytes.TrimRight(b[unix.SizeofInotifyEvent:end], "\x00"))
		if (name != "" && name != "register") || ev.Mask&unix.IN_Q_OVERFLOW != 0 {
			return true
		}
		b = b[end:]
	}
	return false
}
