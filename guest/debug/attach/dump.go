package attach

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const MaxSeccompLayers = 64

var (
	errNoFilter = errors.New("not in seccomp filter mode")
	errNoIndex  = errors.New("no filter at this index")
)

func readStack(get func(idx int) ([]Insn, error), mode, filters int) ([][]Insn, error) {
	var layers [][]Insn
	for idx := 0; ; idx++ {
		if idx >= MaxSeccompLayers {
			return nil, fmt.Errorf("the target has more than %d seccomp filters", MaxSeccompLayers)
		}
		layer, err := get(idx)
		switch {
		case err == nil:
			if len(layer) == 0 {
				return nil, fmt.Errorf("filter %d of the target is empty", idx)
			}
			layers = append(layers, layer)
			continue
		case idx == 0 && errors.Is(err, errNoFilter):
			if mode != 0 || filters != 0 {
				return nil, fmt.Errorf("the kernel gave no filter for a target whose status says seccomp mode %d with %d filters", mode, filters)
			}
			return nil, nil
		case idx > 0 && errors.Is(err, errNoIndex):
		default:
			return nil, fmt.Errorf("reading the target's seccomp filter %d: %w", idx, err)
		}
		break
	}
	if len(layers) != filters || mode != 2 {
		return nil, fmt.Errorf("read %d seccomp filters of the target, whose status says mode %d with %d", len(layers), mode, filters)
	}
	return layers, nil
}

func seccompStatus(b []byte) (mode, filters int, err error) {
	mode, filters = -1, -1
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "Seccomp:"); ok {
			mode, _ = strconv.Atoi(strings.TrimSpace(v))
		}
		if v, ok := strings.CutPrefix(line, "Seccomp_filters:"); ok {
			filters, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if mode < 0 || filters < 0 {
		return 0, 0, errors.New("the status has no Seccomp and Seccomp_filters lines")
	}
	return mode, filters, nil
}

func LastNSpid(status []byte) (int, error) {
	for _, line := range strings.Split(string(status), "\n") {
		if rest, ok := strings.CutPrefix(line, "NSpid:"); ok {
			f := strings.Fields(rest)
			if len(f) == 0 {
				break
			}
			return strconv.Atoi(f[len(f)-1])
		}
	}
	return 0, errors.New("the status has no NSpid line")
}

func StartTime(stat []byte) (uint64, error) {
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, errors.New("a stat line with no comm")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return 0, errors.New("a stat line too short")
	}
	return strconv.ParseUint(f[19], 10, 64)
}

func nsIno(link string) (uint64, bool) {
	a := strings.IndexByte(link, '[')
	b := strings.LastIndexByte(link, ']')
	if a < 0 || b <= a {
		return 0, false
	}
	n, err := strconv.ParseUint(link[a+1:b], 10, 64)
	return n, err == nil
}

func findTarget(proc string, pidnsIno uint64, nspid int, start uint64) (int, error) {
	ents, err := os.ReadDir(proc)
	if err != nil {
		return 0, err
	}
	for _, e := range ents {
		name := e.Name()
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		link, err := os.Readlink(filepath.Join(proc, name, "ns", "pid"))
		if err != nil {
			continue
		}
		if ino, ok := nsIno(link); !ok || ino != pidnsIno {
			continue
		}
		st, err := os.ReadFile(filepath.Join(proc, name, "status"))
		if err != nil {
			continue
		}
		if n, err := LastNSpid(st); err != nil || n != nspid {
			continue
		}
		sb, err := os.ReadFile(filepath.Join(proc, name, "stat"))
		if err != nil {
			continue
		}
		if t, err := StartTime(sb); err != nil || t != start {
			return 0, fmt.Errorf("the target's process %d is not the one resolved (it started at %d, not %d)", nspid, t, start)
		}
		return pid, nil
	}
	return 0, fmt.Errorf("the target's process (pid %d of its namespace) is not running", nspid)
}
