package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func topSample(sec int) api.StatsSample {
	at := time.Date(2026, 10, 19, 10, 0, sec, 0, time.UTC).Format(time.RFC3339Nano)
	n := uint64(sec)
	group := func(kind, name, id string, cpuPerSec, memMiB, net uint64, pids int) api.StatsGroup {
		return api.StatsGroup{Kind: kind, Name: name, ID: id, CPUUsec: cpuPerSec * n, MemoryBytes: memMiB << 20,
			DiskReadBytes: 1 << 20 * n, DiskWriteBytes: 512 << 10 * n, NetRxBytes: net * n, NetTxBytes: net / 2 * n, PIDs: pids}
	}
	return api.StatsSample{Subscription: 5, Time: at, State: api.StateRunning,
		VMM: &api.VMMStats{GuestBytes: 8 << 30, FootprintBytes: 900 << 20, ReleasedBytes: 2 << 30},
		Guest: &api.GuestStats{MemTotalKB: 8 << 20, MemAvailableKB: 6 << 20, CachedKB: 300 << 10, Groups: []api.StatsGroup{
			group(api.StatsSystem, "guest", "", 1500000, 1200, 4<<20, 90),
			group(api.StatsService, "engine", "", 250000, 150, 0, 38),
			group(api.StatsMachine, "dev", "", 100000, 200, 64<<10, 12),
			group(api.StatsContainer, "web", "c1", 500000, 40, 1<<20, 5),
			group(api.StatsContainer, "db", "c2", 200000, 300, 128<<10, 20),
			group(api.StatsContainer, "scratch", "c3", 0, 2, 0, 1),
		}}}
}

func TestTopsScreenGroupsAndRates(t *testing.T) {
	local := time.Local
	time.Local = time.UTC
	defer func() { time.Local = local }()
	names := map[string]ctrInfo{"c1": {Name: "shop-web-1", Project: "shop", Service: "web"}, "c2": {Name: "shop-db-1", Project: "shop", Service: "db"}}
	s0, s2 := topSample(10), topSample(12)
	golden(t, "top.first", topFrame(nil, &s0, names, 0, 0))
	golden(t, "top", topFrame(&s0, &s2, names, 0, 0))
	short := topFrame(&s0, &s2, names, 60, 10)
	if !strings.Contains(short, "(3 more rows; make the window taller)") {
		t.Errorf("a short window:\n%s", short)
	}
	for _, line := range strings.Split(short, "\n") {
		if len(line) > 60 {
			t.Errorf("a line wider than the window: %q", line)
		}
	}
	stopped := api.StatsSample{Time: s0.Time, State: api.StateStopped}
	if got := topFrame(nil, &stopped, nil, 0, 0); !strings.Contains(got, "Nothing runs") {
		t.Errorf("stopped:\n%s", got)
	}
}

type fakeEngine struct {
	mu    sync.Mutex
	lists []string
	asked atomic.Int32
}

func (e *fakeEngine) serve(t *testing.T, path string) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			r := bufio.NewReader(conn)
			line, _ := r.ReadString('\n')
			for {
				h, err := r.ReadString('\n')
				if err != nil || h == "\r\n" {
					break
				}
			}
			n := int(e.asked.Add(1))
			e.mu.Lock()
			body := e.lists[min(n, len(e.lists))-1]
			e.mu.Unlock()
			if !strings.HasPrefix(line, "GET /containers/json HTTP/1.0") {
				fmt.Fprintf(conn, "HTTP/1.0 400 Bad Request\r\n\r\n")
			} else {
				fmt.Fprintf(conn, "HTTP/1.0 200 OK\r\nContent-Type: application/json\r\n\r\n%s", body)
			}
			conn.Close()
		}
	}()
}

func TestTopFollowsTheStreamAndARename(t *testing.T) {
	f := newFake(t)
	engine := &fakeEngine{lists: []string{
		`[{"Id":"c1","Names":["/shop-web-1"],"Labels":{"com.docker.compose.project":"shop"}}]`,
		`[{"Id":"c1","Names":["/storefront"],"Labels":{"com.docker.compose.project":"shop"}}]`,
	}}
	engine.serve(t, filepath.Join(f.home, "run", "docker.sock"))
	var events *jsonrpc.Conn
	got := make(chan struct{})
	f.then(api.MethodEventsSubscribe, func(c *jsonrpc.Conn, _ json.RawMessage) { events = c; close(got) })
	f.then(api.MethodStatsSubscribe, func(c *jsonrpc.Conn, _ json.RawMessage) {
		<-got
		c.Notify(api.NotifyStatsSample, topSample(10))
		c.Notify(api.NotifyStatsSample, topSample(12))
		time.Sleep(100 * time.Millisecond)
		events.Notify(api.NotifyDockerEvent, api.DockerEvent{Subscription: 2, Type: "container", Action: "rename", ID: "c1", Name: "storefront", Time: "2026-10-19T10:00:13Z"})
		time.Sleep(100 * time.Millisecond)
		c.Notify(api.NotifyStatsSample, topSample(14))
	})
	r := f.run("top", "-n", "3", "--interval", "1").want(t, 0)
	blocks := strings.Split(r.out, "\n\nfev ")
	if len(blocks) != 3 {
		t.Fatalf("%d blocks:\n%s", len(blocks), r.out)
	}
	if !strings.Contains(blocks[1], "shop-web-1") || strings.Contains(blocks[1], "storefront") || !strings.Contains(blocks[2], "  storefront") {
		t.Errorf("the rename is not followed:\n%s", r.out)
	}
	if n := engine.asked.Load(); n != 2 {
		t.Errorf("the engine's list was read %d times, want 2", n)
	}
	if got := f.params(api.MethodStatsSubscribe); got != `{"interval_ms":1000}` {
		t.Errorf("asked %s", got)
	}

	f2 := newFake(t)
	f2.then(api.MethodStatsSubscribe, func(c *jsonrpc.Conn, _ json.RawMessage) {
		c.Notify(api.NotifyStatsSample, topSample(10))
		c.Notify(api.NotifyStatsSample, topSample(12))
	})
	r = f2.run("--format", "json", "top", "-n", "2").want(t, 0)
	lines := strings.Split(strings.TrimSpace(r.out), "\n")
	var s api.StatsSample
	if len(lines) != 2 || json.Unmarshal([]byte(lines[1]), &s) != nil || s.Time != topSample(12).Time {
		t.Errorf("json:\n%s", r.out)
	}

	started := noDaemon(t)
	if r := fev(t, scratchHome(t), "", "top"); r.code != ExitNotRunning {
		t.Errorf("with no daemon: %+v", r)
	}
	if _, err := os.Stat(started); err == nil {
		t.Error("top started a daemon")
	}
	if r := f2.run("top", "--interval", "0.1"); r.code != ExitUsage {
		t.Errorf("a short interval: %+v", r)
	}
}
