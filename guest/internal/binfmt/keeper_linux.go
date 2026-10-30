package binfmt

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"
)

const binfmtMagic = 0x42494e4d

func Mount(dir string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err == nil && st.Type == binfmtMagic {
		return nil
	}
	if err := unix.Mount("binfmt_misc", dir, "binfmt_misc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("binfmt: mount %s: %w", dir, err)
	}
	return nil
}

type table interface {
	Entries() (bool, []api.EmulationEntry, error)
	Has(name string) bool
	Register(Spec) error
	Remove(name string) error
}

type Keeper struct {
	Table table
	Dir   string
	Specs []Spec
	Logf  func(format string, args ...any)

	mu       sync.Mutex
	want     bool
	restored int
	had      map[string]bool
	err      error
	watched  bool
}

func (k *Keeper) Start(want bool) error {
	k.mu.Lock()
	k.want = want
	k.mu.Unlock()
	if f, err := k.watch(); err != nil {
		k.Logf("binfmt: the table %s is not watched, so an entry removed from it is not put back: %v", k.Dir, err)
	} else {
		go k.follow(f)
	}
	if want {
		_, err := k.ensure(false)
		return err
	}
	return nil
}

func (k *Keeper) Set(want bool) error {
	k.mu.Lock()
	k.want = want
	k.mu.Unlock()
	if want {
		_, err := k.ensure(false)
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.err, k.had = nil, nil
	_, entries, err := k.Table.Entries()
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		for _, s := range k.Specs {
			if s.Is(e) {
				if err := k.Table.Remove(e.Name); err != nil && k.Table.Has(e.Name) {
					errs = append(errs, fmt.Errorf("binfmt: remove %s: %w", e.Name, err))
				}
			}
		}
	}
	return errors.Join(errs...)
}

func (k *Keeper) Wanted() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.want
}

func (k *Keeper) State() (enabled bool, entries []api.EmulationEntry, registered bool, restored int, err error) {
	if k.Wanted() {
		k.ensure(true)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	enabled, entries, err = k.Table.Entries()
	if err != nil {
		return false, []api.EmulationEntry{}, false, k.restored, err
	}
	n := 0
	for i := range entries {
		for _, s := range k.Specs {
			if s.Is(entries[i]) {
				entries[i].Fevom = true
				if entries[i].Enabled {
					n++
				}
			}
		}
	}
	return enabled, entries, n == len(k.Specs), k.restored, k.err
}

func (k *Keeper) ensure(restore bool) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.want {
		return 0, nil
	}
	n := 0
	var errs []error
	var entries []api.EmulationEntry
	read := false
	for _, s := range k.Specs {
		if k.Table.Has(s.Name) {
			continue
		}
		if !read {
			_, entries, _ = k.Table.Entries()
			read = true
		}
		if len(Shadowing(entries, s)) > 0 {
			continue
		}
		if err := k.Table.Register(s); err != nil {
			if k.Table.Has(s.Name) {
				continue
			}
			errs = append(errs, fmt.Errorf("binfmt: register %s: %w", s.Name, err))
			continue
		}
		n++
		if restore && k.had[s.Name] {
			k.restored++
			k.Logf("binfmt: %s had gone from the table; registered again", s.Name)
		}
		if k.had == nil {
			k.had = map[string]bool{}
		}
		k.had[s.Name] = true
	}
	k.err = errors.Join(errs...)
	return n, k.err
}

func (k *Keeper) watch() (*os.File, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	if _, err := unix.InotifyAddWatch(fd, k.Dir, unix.IN_DELETE|unix.IN_DELETE_SELF|unix.IN_UNMOUNT); err != nil {
		unix.Close(fd)
		return nil, err
	}
	k.mu.Lock()
	k.watched = true
	k.mu.Unlock()
	return os.NewFile(uintptr(fd), "binfmt-watch"), nil
}

func (k *Keeper) Watched() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.watched
}

func (k *Keeper) follow(f *os.File) {
	defer f.Close()
	buf := make([]byte, 64*(unix.SizeofInotifyEvent+unix.NAME_MAX+1))
	for {
		n, err := f.Read(buf)
		if err != nil {
			k.mu.Lock()
			k.watched = false
			k.mu.Unlock()
			k.Logf("binfmt: the watch of %s ended: %v", k.Dir, err)
			return
		}
		lost, gone := false, false
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			name := ""
			if ev.Len > 0 {
				raw := buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+int(ev.Len)]
				name = string(bytes.TrimRight(raw, "\x00"))
			}
			off += unix.SizeofInotifyEvent + int(ev.Len)
			switch {
			case ev.Mask&unix.IN_Q_OVERFLOW != 0:
				lost = true
			case ev.Mask&(unix.IN_DELETE_SELF|unix.IN_UNMOUNT|unix.IN_IGNORED) != 0:
				gone = true
			case ev.Mask&unix.IN_DELETE != 0 && name != "":
				lost = true
			}
		}
		if lost {
			k.ensure(true)
		}
		if gone {
			k.mu.Lock()
			k.watched = false
			k.mu.Unlock()
			k.Logf("binfmt: the table %s was unmounted; its entries are no longer kept", k.Dir)
			return
		}
	}
}

func MemoryModel() bool {
	const prGetMemModel = 0x6d4d444c
	_, err := unix.PrctlRetInt(prGetMemModel, 0, 0, 0, 0)
	return err == nil
}
