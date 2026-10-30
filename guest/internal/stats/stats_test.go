package stats

import (
	"os"
	"path/filepath"
	"testing"

	api "fev/proto/agent"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cgroup(t *testing.T, dir string, cpu, mem, active, inactive, pids uint64, io string) {
	t.Helper()
	write(t, filepath.Join(dir, "cpu.stat"), "usage_usec "+itoa(cpu)+"\nuser_usec 1\nsystem_usec 2\n")
	write(t, filepath.Join(dir, "memory.current"), itoa(mem)+"\n")
	write(t, filepath.Join(dir, "memory.stat"), "anon 5\nfile 999999999\nactive_file "+itoa(active)+"\ninactive_file "+itoa(inactive)+"\n")
	write(t, filepath.Join(dir, "pids.current"), itoa(pids)+"\n")
	write(t, filepath.Join(dir, "io.stat"), io)
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

const idA = "1111111111111111111111111111111111111111111111111111111111111111"
const idB = "2222222222222222222222222222222222222222222222222222222222222222"

func tree(t *testing.T) (*Reader, string) {
	t.Helper()
	root := t.TempDir()
	proc, sys, cg := filepath.Join(root, "proc"), filepath.Join(root, "sys"), filepath.Join(root, "cgroup")
	write(t, filepath.Join(proc, "meminfo"), "MemTotal:        8140412 kB\nMemFree:         7000000 kB\nMemAvailable:    7630220 kB\nBuffers:            1000 kB\nCached:           152640 kB\n")
	write(t, filepath.Join(proc, "loadavg"), "0.00 0.01 0.05 2/61 1234\n")
	write(t, filepath.Join(proc, "diskstats"),
		" 254       0 vda 100 0 2000 50 30 0 400 10 0 60 60 0 0 0 0\n"+
			" 254       1 vda1 90 0 1800 50 30 0 400 10 0 60 60\n"+
			" 254      16 vdb 10 0 48 5 3 0 16 1 0 6 6\n"+
			"   7       0 loop0 5 0 1000 0 0 0 0 0 0 0 0\n")
	write(t, filepath.Join(proc, "net", "dev"),
		"Inter-|   Receive                                                |  Transmit\n"+
			" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"+
			"    lo:    5000      10    0    0    0     0          0         0     5000      10    0    0    0     0       0          0\n"+
			"  eth0: 1048576     900    0    0    0     0          0         0    65536     800    0    0    0     0       0          0\n"+
			"docker0:   7000      70    0    0    0     0          0         0     9000      90    0    0    0     0       0          0\n"+
			"veth1a2b:  2206      20    0    0    0     0          0         0     1830      18    0    0    0     0       0          0\n")
	write(t, filepath.Join(sys, "class", "net", "eth0", "device", "vendor"), "0x1af4\n")
	write(t, filepath.Join(sys, "class", "net", "docker0", "bridge", "x"), "")
	write(t, filepath.Join(cg, "cpu.stat"), "usage_usec 9120443\n")
	engine := filepath.Join(cg, "fev.system", "docker")
	cgroup(t, filepath.Join(engine, "engine"), 3000000, 140<<20, 40<<20, 18<<20, 30, "254:0 rbytes=96468992 wbytes=1310720 rios=1 wios=2 dbytes=0 dios=0\n")
	cgroup(t, filepath.Join(engine, "exec"), 312907, 8<<20, 1<<20, 0, 8, "")
	cgroup(t, filepath.Join(engine, "docker", idB), 48211, 9539584, 4000000, 96000, 11,
		"254:0 rbytes=5767168 wbytes=0 rios=9 wios=0\n254:16 rbytes=1000 wbytes=4096 rios=1 wios=1\n")
	cgroup(t, filepath.Join(engine, "docker", idA), 7, 4096, 0, 0, 1, "")
	cgroup(t, filepath.Join(engine, "docker", "buildkit"), 99, 1<<20, 0, 0, 2, "")
	write(t, filepath.Join(engine, "docker", idB, "cgroup.procs"), "4242\n4243\n")
	write(t, filepath.Join(engine, "docker", idA, "cgroup.procs"), "5151\n")
	os.MkdirAll(filepath.Join(proc, "self", "ns"), 0o755)
	os.MkdirAll(filepath.Join(proc, "4242", "ns"), 0o755)
	os.MkdirAll(filepath.Join(proc, "5151", "ns"), 0o755)
	os.Symlink("net:[4026531840]", filepath.Join(proc, "self", "ns", "net"))
	os.Symlink("net:[4026532299]", filepath.Join(proc, "4242", "ns", "net"))
	os.Symlink("net:[4026531840]", filepath.Join(proc, "5151", "ns", "net"))
	container := "Inter-|   Receive |  Transmit\n face |bytes |bytes\n" +
		"    lo:     100       1    0    0    0     0          0         0      100       1    0    0    0     0       0          0\n" +
		"  eth0:    2206      20    0    0    0     0          0         0     1830      18    0    0    0     0       0          0\n"
	write(t, filepath.Join(proc, "4242", "net", "dev"), container)
	write(t, filepath.Join(proc, "5151", "net", "dev"), container)
	data := filepath.Join(root, "data")
	write(t, filepath.Join(data, "containers", idB, "config.v2.json"), `{"ID":"`+idB+`","Name":"/web","Config":{"Image":"nginx"}}`)
	now := 184223.5
	return &Reader{Proc: proc, Sys: sys, Root: cg, Engine: engine, Own: []string{"engine", "exec"}, Containers: "docker", Data: data,
		Now: func() float64 { return now }}, root
}

func TestASampleReadsTheGuestTheEngineAndEachContainer(t *testing.T) {
	r, _ := tree(t)
	s, err := r.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if s.TimeMS != 184223.5 || s.MemTotalKB != 8140412 || s.MemAvailableKB != 7630220 || s.CachedKB != 152640 {
		t.Errorf("the guest's memory: %+v", s)
	}
	want := []api.StatsGroup{
		{Kind: "system", Name: "guest", CPUUsec: 9120443, MemoryBytes: (8140412 - 7000000) << 10, MemoryFileBytes: 152640 << 10,
			DiskReadBytes: (2000 + 48) * 512, DiskWriteBytes: (400 + 16) * 512, NetRxBytes: 1048576, NetTxBytes: 65536, PIDs: 61},
		{Kind: "service", Name: "engine", CPUUsec: 3312907, MemoryBytes: 148 << 20, MemoryFileBytes: 59 << 20, DiskReadBytes: 96468992, DiskWriteBytes: 1310720, PIDs: 38},
		{Kind: "container", Name: idA[:12], ID: idA, CPUUsec: 7, MemoryBytes: 4096, PIDs: 1},
		{Kind: "container", Name: "web", ID: idB, CPUUsec: 48211, MemoryBytes: 9539584, MemoryFileBytes: 4096000,
			DiskReadBytes: 5767168 + 1000, DiskWriteBytes: 4096, NetRxBytes: 2206, NetTxBytes: 1830, PIDs: 11},
	}
	if len(s.Groups) != len(want) {
		t.Fatalf("%d groups: %+v", len(s.Groups), s.Groups)
	}
	for i, g := range s.Groups {
		if g != want[i] {
			t.Errorf("group %d:\n got %+v\nwant %+v", i, g, want[i])
		}
	}
}

func TestNamesAreReadOnceAndAGuestWithNoEngineReportsItself(t *testing.T) {
	r, root := tree(t)
	if _, err := r.Sample(); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "data", "containers", idB, "config.v2.json")
	write(t, config, `{"Name":"/renamed"}`)
	s, _ := r.Sample()
	if s.Groups[3].Name != "web" {
		t.Errorf("the name was read again: %q", s.Groups[3].Name)
	}
	os.RemoveAll(filepath.Join(r.Engine, "docker", idB))
	if s, _ = r.Sample(); len(s.Groups) != 3 || len(r.names) != 1 {
		t.Fatalf("after the container went: %d groups, names %v", len(s.Groups), r.names)
	}
	cgroup(t, filepath.Join(r.Engine, "docker", idB), 1, 1, 0, 0, 1, "")
	if s, _ = r.Sample(); s.Groups[3].Name != "renamed" {
		t.Errorf("a container made again is named %q", s.Groups[3].Name)
	}
	os.RemoveAll(r.Engine)
	s, err := r.Sample()
	if err != nil || len(s.Groups) != 1 || s.Groups[0].Kind != api.StatsSystem {
		t.Fatalf("a guest with no engine: %+v, %v", s.Groups, err)
	}
	os.RemoveAll(r.Proc)
	if _, err := r.Sample(); err == nil {
		t.Error("a guest with no /proc gave a sample")
	}
}

func TestASampleReadsEachRunningMachine(t *testing.T) {
	r, root := tree(t)
	machines := filepath.Join(root, "cgroup", "fev.machines")
	cgroup(t, filepath.Join(machines, "dev"), 5000, 64<<20, 8<<20, 2<<20, 17, "254:0 rbytes=4096 wbytes=8192 rios=1 wios=2\n")
	cgroup(t, filepath.Join(machines, "old-name"), 6000, 32<<20, 0, 0, 9, "")
	write(t, filepath.Join(r.Proc, "700", "net", "dev"),
		"Inter-|   Receive |  Transmit\n face |bytes |bytes\n"+
			"    lo:     100       1    0    0    0     0          0         0      100       1    0    0    0     0       0          0\n"+
			"  eth0:    4000      20    0    0    0     0          0         0     3000      18    0    0    0     0       0          0\n")
	running := []Machine{
		{Name: "web", UUID: "u-web", Group: filepath.Join(machines, "old-name"), PID: 800},
		{Name: "dev", UUID: "u-dev", Group: filepath.Join(machines, "dev"), PID: 700},
		{Name: "gone", UUID: "u-gone", Group: filepath.Join(machines, "gone"), PID: 900},
	}
	r.Machines = func() []Machine { return append([]Machine(nil), running...) }
	s, err := r.Sample()
	if err != nil {
		t.Fatal(err)
	}
	want := []api.StatsGroup{
		{Kind: "machine", Name: "dev", ID: "u-dev", CPUUsec: 5000, MemoryBytes: 64 << 20, MemoryFileBytes: 10 << 20, DiskReadBytes: 4096, DiskWriteBytes: 8192, NetRxBytes: 4000, NetTxBytes: 3000, PIDs: 17},
		{Kind: "machine", Name: "web", ID: "u-web", CPUUsec: 6000, MemoryBytes: 32 << 20, PIDs: 9},
	}
	got := s.Groups[4:]
	if len(s.Groups) != 6 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("machines:\n got %+v\nwant %+v", s.Groups, want)
	}
	os.RemoveAll(r.Engine)
	if s, _ = r.Sample(); len(s.Groups) != 3 || s.Groups[1].Kind != api.StatsMachine {
		t.Fatalf("a guest with no engine and two machines: %+v", s.Groups)
	}
}
