package boottrace

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/daemon"
)

func Now() uint64 {
	var ts unix.Timespec
	unix.ClockGettime(unix.CLOCK_UPTIME_RAW, &ts)
	return uint64(ts.Sec)*1e9 + uint64(ts.Nsec)
}

const (
	Origin             = "origin"
	DaemonMain         = "daemon.main"
	DaemonAPIListening = "daemon.api_listening"
	VMMSpawn           = "vmm.spawn"
	VMMGuestEntry      = "vmm.guest_entry"
	KernelRunInit      = "kernel.run_init"
	InitMain           = "init.main"
	AgentConnected     = "agent.connected"
	InitReady          = "init.ready"
	EngineStart        = "engine.start"
	EngineReady        = "engine.ready"
	DockerReleased     = "docker.released"
)

type StageDef struct {
	Stage    int
	From, To string
	BudgetMS float64
}

var Budget = []StageDef{
	{0, Origin, VMMSpawn, 50},
	{1, VMMSpawn, VMMGuestEntry, 100},
	{2, VMMGuestEntry, InitMain, 350},
	{3, InitMain, InitReady, 200},
	{4, InitReady, EngineReady, 1100},
	{5, EngineReady, DockerReleased, 200},
}

const TotalBudgetMS = 2000

const Version = 1

type Recorder struct {
	mu        sync.Mutex
	head      api.Trace
	originNS  uint64
	host      map[string]uint64
	guest     map[string]guestMark
	offsetSet bool
	ended     bool
}

type guestMark struct {
	ms     float64
	approx bool
}

func New(bootID, build, origin string, originNS uint64, cpus, memMiB int) *Recorder {
	wall := time.Now().Add(-time.Duration(Now() - originNS)).UTC()
	return &Recorder{
		head: api.Trace{
			V: Version, BootID: bootID, Build: build,
			Wall: wall.Format("2006-01-02T15:04:05.000Z07:00"), Clock: "uptime_raw",
			Origin: origin, OriginNS: originNS, CPUs: cpus, MemMiB: memMiB,
			BudgetMS: TotalBudgetMS,
		},
		originNS: originNS,
		host:     map[string]uint64{},
		guest:    map[string]guestMark{},
	}
}

func (r *Recorder) Mark(name string) { r.MarkAt(name, Now()) }

func (r *Recorder) MarkAt(name string, ns uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.host[name]; !ok && !r.ended {
		r.host[name] = ns
	}
}

func (r *Recorder) MarkGuest(name string, guestMS float64, approx bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.guest[name]; !ok && !r.ended {
		r.guest[name] = guestMark{guestMS, approx}
	}
}

func (r *Recorder) SetGuestClock(sentNS, receivedNS uint64, guestNowMS float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.offsetSet {
		return
	}
	mid := float64(sentNS)/2 + float64(receivedNS)/2
	r.head.GuestOffsetMS = round((mid-float64(r.originNS))/1e6 - guestNowMS)
	r.head.GuestErrorMS = round(float64(receivedNS-sentNS) / 2e6)
	r.offsetSet = true
}

func (r *Recorder) GuestToWall(guestMS float64) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.offsetSet {
		return time.Time{}, false
	}
	origin, err := time.Parse(time.RFC3339Nano, r.head.Wall)
	if err != nil {
		return time.Time{}, false
	}
	return origin.Add(time.Duration((guestMS + r.head.GuestOffsetMS) * 1e6)), true
}

func (r *Recorder) End() {
	r.mu.Lock()
	r.ended = true
	r.mu.Unlock()
}

func (r *Recorder) Complete() bool { return r.Trace().Complete }

func round(ms float64) float64 { return float64(int64(ms*1000+0.5*sign(ms))) / 1000 }

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

func (r *Recorder) Trace() api.Trace {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.head
	at := map[string]float64{Origin: 0}
	t.Marks = []api.Mark{}
	for name, ns := range r.host {
		ms := round((float64(ns) - float64(r.originNS)) / 1e6)
		at[name] = ms
		t.Marks = append(t.Marks, api.Mark{Name: name, MS: ms})
	}
	if r.offsetSet {
		for name, g := range r.guest {
			if _, taken := at[name]; taken {
				continue
			}
			ms := round(g.ms + t.GuestOffsetMS)
			at[name] = ms
			t.Marks = append(t.Marks, api.Mark{Name: name, MS: ms, Approx: g.approx})
		}
	}
	sort.SliceStable(t.Marks, func(i, j int) bool {
		if t.Marks[i].MS != t.Marks[j].MS {
			return t.Marks[i].MS < t.Marks[j].MS
		}
		return t.Marks[i].Name < t.Marks[j].Name
	})

	t.Stages = []api.Stage{}
	t.OK = true
	for _, def := range Budget {
		from, okFrom := at[def.From]
		to, okTo := at[def.To]
		if !okFrom || !okTo {
			continue
		}
		s := api.Stage{Stage: def.Stage, From: def.From, To: def.To, MS: round(to - from), BudgetMS: def.BudgetMS}
		s.Over = s.MS > s.BudgetMS
		t.OK = t.OK && !s.Over
		t.Stages = append(t.Stages, s)
		t.TotalMS = to
	}
	t.Complete = len(t.Stages) == len(Budget)
	t.OK = t.OK && t.Complete && t.TotalMS <= t.BudgetMS
	return t
}

func Over(t api.Trace) []api.Stage {
	var over []api.Stage
	for _, s := range t.Stages {
		if s.Over {
			over = append(over, s)
		}
	}
	return over
}

func Append(path string, t api.Trace) error { return AppendRotating(path, t, 0, 0) }

func AppendRotating(path string, t api.Trace, max int64, generations int) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if info, serr := os.Stat(path); max > 0 && generations > 0 && serr == nil && info.Size() > 0 && info.Size()+int64(len(data))+1 > max {
		os.Remove(fmt.Sprintf("%s.%d", path, generations))
		for g := generations - 1; g >= 1; g-- {
			os.Rename(fmt.Sprintf("%s.%d", path, g), fmt.Sprintf("%s.%d", path, g+1))
		}
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
