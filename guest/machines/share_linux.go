package machines

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/internal/logbuf"
)

const fuseMagic = 0x65735546

func shareMounted() bool {
	var st unix.Statfs_t
	return unix.Statfs(api.ShareTarget, &st) == nil && st.Type == fuseMagic
}

func bindShare(root string) error {
	if !shareMounted() {
		fmt.Fprintln(os.Stderr, "fev: this VM has no file share; the machine has no /mnt/mac")
		return nil
	}
	bind := func(source, target string) error {
		t := root + target
		if st, err := os.Lstat(t); err == nil && st.Mode()&os.ModeSymlink != 0 {
			fmt.Fprintf(os.Stderr, "fev: %s is a link in the machine; the Mac's %s is not bound there\n", target, source)
			return nil
		}
		if err := mountPoint(source, t, false); err != nil {
			return err
		}
		if err := unix.Mount(source, t, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("bind the Mac's %s at the machine's %s: %w", source, target, err)
		}
		return nil
	}
	if err := bind(api.ShareTarget, api.ShareTarget); err != nil {
		return err
	}
	for _, p := range api.MachineSharePaths {
		source := api.ShareTarget + p.Source
		if st, err := os.Lstat(source); err != nil || !st.IsDir() {
			continue
		}
		if err := bind(source, p.Target); err != nil {
			return err
		}
	}
	return nil
}

var ErrNoShare = errors.New("the machine was started without the Mac's files (files.share)")

func (rt *Runtime) ShareBind(names []string) ([]api.MachineShareBound, error) {
	if err := rt.available(); err != nil {
		return nil, err
	}
	type target struct {
		name string
		r    *run
		b    *boot
	}
	var targets []target
	pick := func(name string, asked bool) error {
		m := rt.get(name)
		m.mu.Lock()
		r := m.cur
		state := m.state
		m.mu.Unlock()
		var b *boot
		if r != nil {
			b = r.boot()
		}
		switch {
		case r == nil || b == nil || state != api.MachineRunning:
			if asked {
				return fmt.Errorf("%s is not running (%s)", name, state)
			}
		case !r.macFiles():
			if asked {
				return fmt.Errorf("%s: %w", name, ErrNoShare)
			}
		default:
			targets = append(targets, target{name, r, b})
		}
		return nil
	}
	if len(names) == 0 {
		rt.mu.Lock()
		for name := range rt.byName {
			names = append(names, name)
		}
		rt.mu.Unlock()
		sort.Strings(names)
		for _, name := range names {
			pick(name, false)
		}
	} else {
		for _, name := range names {
			if _, err := ReadRecord(rt.o.DataDir, name); err != nil {
				return nil, err
			}
			if err := pick(name, true); err != nil {
				return nil, err
			}
		}
		sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })
	}
	if !shareMounted() {
		if len(targets) == 0 {
			return nil, nil
		}
		return nil, errors.New("this VM has no file share")
	}
	var out []api.MachineShareBound
	for _, t := range targets {
		bound, unbound, err := t.b.bindLate()
		if err != nil {
			return out, fmt.Errorf("%s: %w", t.name, err)
		}
		if len(bound) > 0 {
			t.r.m.runtimeLogf(logbuf.Info, "bound the Mac's %v", bound)
		}
		out = append(out, api.MachineShareBound{Machine: t.name, Bound: append([]string{}, bound...), Unbound: append([]string{}, unbound...)})
	}
	return out, nil
}

func (b *boot) bindLate() (bound, unbound []string, err error) {
	b.nsMu.RLock()
	defer b.nsMu.RUnlock()
	if b.ns == nil {
		return nil, nil, errors.New("the machine has stopped")
	}
	mnt := int(b.ns["mnt"].Fd())
	type result struct {
		bound, unbound []string
		err            error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		var res result
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			res.err = err
			done <- res
			return
		}
		if err := unix.Setns(mnt, unix.CLONE_NEWNS); err != nil {
			res.err = fmt.Errorf("enter the machine's mount namespace: %w", err)
			done <- res
			return
		}
		if err := unix.Chdir("/"); err != nil {
			res.err = err
			done <- res
			return
		}
		var share unix.Stat_t
		if err := unix.Stat(api.ShareTarget, &share); err != nil {
			res.err = fmt.Errorf("the machine has no %s: %w", api.ShareTarget, err)
			done <- res
			return
		}
		for _, p := range api.MachineSharePaths {
			source := api.ShareTarget + p.Source
			var src, dst unix.Stat_t
			if unix.Lstat(source, &src) != nil || src.Mode&unix.S_IFMT != unix.S_IFDIR {
				res.unbound = append(res.unbound, p.Target)
				continue
			}
			if unix.Lstat(p.Target, &dst) == nil {
				if dst.Dev == src.Dev && dst.Ino == src.Ino {
					continue
				}
				if dst.Mode&unix.S_IFMT != unix.S_IFDIR {
					res.unbound = append(res.unbound, p.Target)
					continue
				}
			} else if err := unix.Mkdir(p.Target, 0o755); err != nil {
				res.unbound = append(res.unbound, p.Target)
				continue
			}
			if err := unix.Mount(source, p.Target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
				res.unbound = append(res.unbound, p.Target)
				continue
			}
			res.bound = append(res.bound, p.Target)
		}
		done <- res
	}()
	res := <-done
	return res.bound, res.unbound, res.err
}
