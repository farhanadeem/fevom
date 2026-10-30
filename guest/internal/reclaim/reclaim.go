package reclaim

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var writeFile = rawWrite

func rawWrite(path string, data []byte, _ os.FileMode) error {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_TRUNC|unix.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: path, Err: err}
	}
	defer unix.Close(fd)
	for {
		_, err = unix.Write(fd, data)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		return &fs.PathError{Op: "write", Path: path, Err: err}
	}
	return nil
}

type stat struct {
	active, inactive, slab uint64
	refaults               uint64
}

func (s stat) cold() uint64 { return s.inactive + s.slab }

func (s stat) cache() uint64 { return s.active + s.inactive + s.slab }

func statOf(dir string) (stat, error) {
	data, err := os.ReadFile(filepath.Join(dir, "memory.stat"))
	if err != nil {
		return stat{}, err
	}
	var st stat
	found := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), " ")
		var to *uint64
		switch name {
		case "active_file":
			to = &st.active
		case "inactive_file":
			to = &st.inactive
		case "slab_reclaimable":
			to = &st.slab
		case "workingset_refault_file":
			to = &st.refaults
		}
		if !ok || to == nil {
			continue
		}
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return stat{}, fmt.Errorf("%s/memory.stat: %s %q", dir, name, value)
		}
		*to = n
		if name == "active_file" || name == "inactive_file" {
			found++
		}
	}
	if found != 2 {
		return stat{}, fmt.Errorf("%s/memory.stat has %d of the two file lists", dir, found)
	}
	return st, nil
}

func fileOf(dir string) (uint64, error) {
	st, err := statOf(dir)
	return st.cache(), err
}

func cpuOf(dir string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "usage_usec "); ok {
			return strconv.ParseUint(value, 10, 64)
		}
	}
	return 0, fmt.Errorf("%s/cpu.stat has no usage_usec", dir)
}

func ask(dir string, n uint64) error {
	return writeFile(filepath.Join(dir, "memory.reclaim"), []byte(strconv.FormatUint(n, 10)+" swappiness=0"), 0)
}

func populated(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if name, value, ok := strings.Cut(line, " "); ok && name == "populated" {
			return value == "1"
		}
	}
	return false
}

func children(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

type Tree struct {
	Root       string
	Own        []string
	Containers string
	System     string
	Others     []string
}

type place struct {
	dir    string
	bytes  uint64
	shield []string
}

type Survey struct {
	Total, Live        uint64
	gone, stopped, own []place
	Running            []Running
}

type Running struct {
	Dir                                   string
	InactiveBytes, ActiveBytes, SlabBytes uint64
	Refaults                              uint64
	CPUUsec                               uint64
}

func Trim(dir string, n uint64) error {
	err := ask(dir, n)
	if errors.Is(err, syscall.EAGAIN) || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Survey) Idle() uint64 { return s.Total - min(s.Live, s.Total) }

func (s *Survey) Own() uint64 {
	var n uint64
	for _, p := range s.own {
		n += p.bytes
	}
	return min(n, s.Idle())
}

func (t Tree) Look() (*Survey, error) {
	s := &Survey{}
	if t.Root != "" {
		if err := t.lookEngine(s); err != nil {
			return nil, err
		}
	}
	for _, dir := range t.Others {
		if st, err := statOf(dir); err == nil && st.cold() > 0 {
			s.Total += st.cold()
			s.stopped = append(s.stopped, place{dir: dir, bytes: st.cold()})
		}
	}
	if t.System != "" {
		if counted, err := fileOf(t.System); err == nil {
			kids := children(t.System)
			var seen uint64
			for _, kid := range kids {
				n, _ := fileOf(kid)
				seen += n
			}
			if counted > seen {
				s.Total += counted - seen
				s.gone = append(s.gone, place{dir: t.System, bytes: counted - seen, shield: kids})
			}
		}
	}
	s.Live = min(s.Live, s.Total)
	return s, nil
}

func (t Tree) lookEngine(s *Survey) error {
	total, err := fileOf(t.Root)
	if err != nil {
		return err
	}
	s.Total = total
	own := map[string]bool{}
	for _, name := range t.Own {
		own[filepath.Join(t.Root, name)] = true
	}
	containers := filepath.Join(t.Root, t.Containers)
	unit := func(dir string) uint64 {
		st, err := statOf(dir)
		if err != nil {
			return 0
		}
		n := st.cache()
		switch {
		case own[dir]:
			s.own = append(s.own, place{dir: dir, bytes: n})
		case populated(dir):
			s.Live += n
			usec, _ := cpuOf(dir)
			s.Running = append(s.Running, Running{Dir: dir, InactiveBytes: st.inactive, ActiveBytes: st.active,
				SlabBytes: st.slab, Refaults: st.refaults, CPUUsec: usec})
		default:
			s.stopped = append(s.stopped, place{dir: dir, bytes: n})
		}
		return n
	}
	parent := func(dir string, counted uint64, each func(string) uint64) {
		kids := children(dir)
		var seen uint64
		for _, kid := range kids {
			seen += each(kid)
		}
		if counted > seen {
			s.gone = append(s.gone, place{dir: dir, bytes: counted - seen, shield: kids})
		}
	}
	parent(t.Root, total, func(dir string) uint64 {
		if dir != containers || t.Containers == "" {
			return unit(dir)
		}
		n, err := fileOf(dir)
		if err != nil {
			return 0
		}
		parent(dir, n, unit)
		return n
	})
	return nil
}

const margin = 2 << 20

func (s *Survey) Reclaim(n uint64) error {
	var first error
	ask := func(p place, bytes uint64) {
		if bytes == 0 {
			return
		}
		restore := shield(p.shield)
		err := ask(p.dir, bytes)
		restore()
		if err != nil && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, fs.ErrNotExist) && first == nil {
			first = err
		}
	}
	for _, p := range s.stopped {
		take := min(p.bytes, n)
		ask(p, take)
		n -= take
	}
	for _, p := range s.gone {
		take := min(p.bytes-min(p.bytes, margin), n)
		ask(p, take)
		n -= take
	}
	for _, p := range s.own {
		take := min(p.bytes, n)
		ask(p, take)
		n -= take
	}
	return first
}

func shield(dirs []string) (restore func()) {
	type was struct {
		path string
		low  []byte
	}
	var saved []was
	for _, dir := range dirs {
		path := filepath.Join(dir, "memory.low")
		low, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if writeFile(path, []byte("max"), 0) == nil {
			saved = append(saved, was{path, bytes.TrimSpace(low)})
		}
	}
	return func() {
		for _, w := range saved {
			writeFile(w.path, w.low, 0)
		}
	}
}

func Pressure(path string) (float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "some" {
			continue
		}
		for _, f := range fields[1:] {
			if value, ok := strings.CutPrefix(f, "avg10="); ok {
				return strconv.ParseFloat(value, 64)
			}
		}
	}
	return 0, fmt.Errorf("%s has no avg10 for some", path)
}

func Exists(root string) bool {
	_, err := os.Stat(filepath.Join(root, "memory.stat"))
	return !errors.Is(err, fs.ErrNotExist)
}
