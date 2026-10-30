package stats

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	api "fev/proto/agent"
)

type Reader struct {
	Proc, Sys    string
	Root, Engine string
	Own          []string
	Containers   string
	Data         string
	Now          func() float64
	Machines     func() []Machine

	names map[string]string
}

type Machine struct {
	Name, UUID, Group string
	PID               int
}

func lines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func keyed(path string, want ...string) []uint64 {
	out := make([]uint64, len(want))
	for _, line := range lines(path) {
		name, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		for i, w := range want {
			if name == w {
				out[i], _ = strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			}
		}
	}
	return out
}

func number(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return n
}

func ioOf(dir string) (read, written uint64) {
	for _, line := range lines(filepath.Join(dir, "io.stat")) {
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "rbytes="); ok {
				n, _ := strconv.ParseUint(v, 10, 64)
				read += n
			} else if v, ok := strings.CutPrefix(f, "wbytes="); ok {
				n, _ := strconv.ParseUint(v, 10, 64)
				written += n
			}
		}
	}
	return read, written
}

func group(dir string) api.StatsGroup {
	g := api.StatsGroup{
		CPUUsec:     keyed(filepath.Join(dir, "cpu.stat"), "usage_usec")[0],
		MemoryBytes: number(filepath.Join(dir, "memory.current")),
		PIDs:        int(number(filepath.Join(dir, "pids.current"))),
	}
	file := keyed(filepath.Join(dir, "memory.stat"), "active_file", "inactive_file")
	g.MemoryFileBytes = min(file[0]+file[1], g.MemoryBytes)
	g.DiskReadBytes, g.DiskWriteBytes = ioOf(dir)
	return g
}

func netDev(path string, keep func(name string) bool) (rx, tx uint64) {
	for _, line := range lines(path) {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if len(f) < 9 || !keep(name) {
			continue
		}
		r, _ := strconv.ParseUint(f[0], 10, 64)
		t, _ := strconv.ParseUint(f[8], 10, 64)
		rx, tx = rx+r, tx+t
	}
	return rx, tx
}

func isID(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, c := range name {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (r *Reader) name(id string) string {
	if n, ok := r.names[id]; ok {
		return n
	}
	name := id[:12]
	if r.Data != "" {
		if data, err := os.ReadFile(filepath.Join(r.Data, "containers", id, "config.v2.json")); err == nil && len(data) < 1<<20 {
			var config struct{ Name string }
			if json.Unmarshal(data, &config) == nil && strings.TrimPrefix(config.Name, "/") != "" {
				name = strings.TrimPrefix(config.Name, "/")
			}
		}
	}
	if r.names == nil {
		r.names = map[string]string{}
	}
	r.names[id] = name
	return name
}

func (r *Reader) containerNet(dir string) (rx, tx uint64) {
	procs := lines(filepath.Join(dir, "cgroup.procs"))
	if len(procs) == 0 || procs[0] == "" {
		return 0, 0
	}
	pid := procs[0]
	theirs, err := os.Readlink(filepath.Join(r.Proc, pid, "ns", "net"))
	ours, _ := os.Readlink(filepath.Join(r.Proc, "self", "ns", "net"))
	if err != nil || theirs == ours {
		return 0, 0
	}
	return netDev(filepath.Join(r.Proc, pid, "net", "dev"), func(name string) bool { return name != "lo" })
}

func (r *Reader) Sample() (api.StatsSample, error) {
	s := api.StatsSample{TimeMS: r.Now(), Groups: []api.StatsGroup{}}
	meminfo, err := os.ReadFile(filepath.Join(r.Proc, "meminfo"))
	if err != nil {
		return s, err
	}
	var free int64
	for sc := bufio.NewScanner(bytes.NewReader(meminfo)); sc.Scan(); {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		kb, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			s.MemTotalKB = kb
		case "MemAvailable:":
			s.MemAvailableKB = kb
		case "MemFree:":
			free = kb
		case "Cached:":
			s.CachedKB = kb
		}
	}

	guest := api.StatsGroup{Kind: api.StatsSystem, Name: "guest",
		CPUUsec:     keyed(filepath.Join(r.Root, "cpu.stat"), "usage_usec")[0],
		MemoryBytes: uint64(max(s.MemTotalKB-free, 0)) << 10,
	}
	guest.MemoryFileBytes = min(uint64(max(s.CachedKB, 0))<<10, guest.MemoryBytes)
	for _, line := range lines(filepath.Join(r.Proc, "diskstats")) {
		f := strings.Fields(line)
		if len(f) < 10 || !strings.HasPrefix(f[2], "vd") || strings.ContainsAny(f[2], "0123456789") {
			continue
		}
		read, _ := strconv.ParseUint(f[5], 10, 64)
		written, _ := strconv.ParseUint(f[9], 10, 64)
		guest.DiskReadBytes += read * 512
		guest.DiskWriteBytes += written * 512
	}
	guest.NetRxBytes, guest.NetTxBytes = netDev(filepath.Join(r.Proc, "net", "dev"), func(name string) bool {
		_, err := os.Stat(filepath.Join(r.Sys, "class", "net", name, "device"))
		return err == nil
	})
	if f := strings.Fields(firstLine(filepath.Join(r.Proc, "loadavg"))); len(f) >= 4 {
		if _, total, ok := strings.Cut(f[3], "/"); ok {
			guest.PIDs, _ = strconv.Atoi(total)
		}
	}
	s.Groups = append(s.Groups, guest)

	if _, err := os.Stat(r.Engine); err != nil {
		r.names = nil
		return r.machines(s), nil
	}
	engine := api.StatsGroup{Kind: api.StatsService, Name: "engine"}
	for _, own := range r.Own {
		g := group(filepath.Join(r.Engine, own))
		engine.CPUUsec += g.CPUUsec
		engine.MemoryBytes += g.MemoryBytes
		engine.MemoryFileBytes += g.MemoryFileBytes
		engine.DiskReadBytes += g.DiskReadBytes
		engine.DiskWriteBytes += g.DiskWriteBytes
		engine.PIDs += g.PIDs
	}
	s.Groups = append(s.Groups, engine)

	entries, _ := os.ReadDir(filepath.Join(r.Engine, r.Containers))
	seen := map[string]bool{}
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || !isID(id) {
			continue
		}
		dir := filepath.Join(r.Engine, r.Containers, id)
		g := group(dir)
		g.Kind, g.ID, g.Name = api.StatsContainer, id, r.name(id)
		g.NetRxBytes, g.NetTxBytes = r.containerNet(dir)
		s.Groups = append(s.Groups, g)
		seen[id] = true
	}
	for id := range r.names {
		if !seen[id] {
			delete(r.names, id)
		}
	}
	return r.machines(s), nil
}

func (r *Reader) machines(s api.StatsSample) api.StatsSample {
	if r.Machines == nil {
		return s
	}
	ms := r.Machines()
	sort.Slice(ms, func(a, b int) bool { return ms[a].Name < ms[b].Name })
	for _, m := range ms {
		if _, err := os.Stat(m.Group); err != nil {
			continue
		}
		g := group(m.Group)
		g.Kind, g.Name, g.ID = api.StatsMachine, m.Name, m.UUID
		if m.PID > 0 {
			g.NetRxBytes, g.NetTxBytes = netDev(filepath.Join(r.Proc, strconv.Itoa(m.PID), "net", "dev"), func(name string) bool { return name != "lo" })
		}
		s.Groups = append(s.Groups, g)
	}
	return s
}

func firstLine(path string) string {
	if l := lines(path); len(l) > 0 {
		return l[0]
	}
	return ""
}
