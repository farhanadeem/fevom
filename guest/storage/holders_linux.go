package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type Holder struct {
	PID  int
	Comm string
	At   string
}

func (h Holder) String() string { return fmt.Sprintf("pid %d (%s) at %s", h.PID, h.Comm, h.At) }

type HeldError struct {
	Name    string
	Holders []Holder
}

func (e *HeldError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is not deleted: it is mounted in another namespace (", e.Name)
	for i, h := range e.Holders {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(h.String())
	}
	b.WriteString("): ")
	b.WriteString(unix.EBUSY.Error())
	return b.String()
}

func (e *HeldError) Unwrap() error { return unix.EBUSY }

type mountEntry struct{ dev, root, at, shared, master string }

func parseMountinfo(s string) []mountEntry {
	var out []mountEntry
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		e := mountEntry{dev: f[2], root: unescapeMountinfo(f[3]), at: unescapeMountinfo(f[4])}
		for _, o := range f[6:] {
			if o == "-" {
				break
			}
			if v, ok := strings.CutPrefix(o, "shared:"); ok {
				e.shared = v
			} else if v, ok := strings.CutPrefix(o, "master:"); ok {
				e.master = v
			}
		}
		out = append(out, e)
	}
	return out
}

func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func within(root, p string) bool {
	return root == p || strings.HasPrefix(root, strings.TrimSuffix(p, "/")+"/")
}

func (m *Machines) fsPath(mounts []mountEntry, path string) (dev, p string, err error) {
	target := filepath.Clean(m.v.cfg.Target)
	for i := len(mounts) - 1; i >= 0; i-- {
		if mounts[i].at == target {
			rel, err := filepath.Rel(target, path)
			if err != nil {
				return "", "", err
			}
			return mounts[i].dev, filepath.Join(mounts[i].root, rel), nil
		}
	}
	return "", "", fmt.Errorf("the data filesystem is not mounted at %s in this namespace", target)
}

func unmountOwn(mounts []mountEntry, dev, p string) error {
	for i := len(mounts) - 1; i >= 0; i-- {
		e := mounts[i]
		if e.dev != dev || !within(e.root, p) {
			continue
		}
		if err := unix.Unmount(e.at, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("unmount %s: %w", e.at, err)
		}
	}
	return nil
}

func holders(dev, p string, copies map[string]bool) []Holder {
	self, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return nil
	}
	seen := map[string]bool{self: true}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []Holder
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		ns, err := os.Readlink(filepath.Join(dir, "ns/mnt"))
		if err != nil || seen[ns] {
			continue
		}
		mi, err := os.ReadFile(filepath.Join(dir, "mountinfo"))
		if err != nil {
			continue
		}
		seen[ns] = true
		for _, me := range parseMountinfo(string(mi)) {
			if me.dev == dev && within(me.root, p) && !(me.master != "" && copies[me.master]) {
				comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
				out = append(out, Holder{PID: pid, Comm: strings.TrimSpace(string(comm)), At: me.at})
				break
			}
		}
	}
	return out
}

func (m *Machines) release(name, path string, held bool) ([]Holder, error) {
	mi, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	mounts := parseMountinfo(string(mi))
	dev, p, err := m.fsPath(mounts, path)
	if err != nil {
		return nil, err
	}
	copies := map[string]bool{}
	for _, e := range mounts {
		if e.dev == dev && within(e.root, p) && e.shared != "" {
			copies[e.shared] = true
		}
	}
	if !held {
		if hs := holders(dev, p, copies); len(hs) > 0 {
			return nil, &HeldError{Name: name, Holders: hs}
		}
	}
	if err := unmountOwn(mounts, dev, p); err != nil {
		return nil, fmt.Errorf("take %s's mounts in init's namespace away: %w", name, err)
	}
	hs := holders(dev, p, nil)
	if len(hs) > 0 && !held {
		return nil, &HeldError{Name: name, Holders: hs}
	}
	return hs, nil
}
