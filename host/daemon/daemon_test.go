package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"fev/host/daemon/config"
	"fev/host/daemon/home"
	"fev/host/daemon/launch"
	"fev/host/daemon/vm"
	"fev/host/daemon/vm/standin"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/version"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == standin.Arg {
		os.Exit(standin.Main(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == churnArg {
		churn()
	}
	os.Setenv("GORACE", "atexit_sleep_ms=0")
	os.Exit(m.Run())
}

var bg = context.Background()

func quick() *Timing {
	return &Timing{
		StartDeadline: 3 * time.Second,
		HelloTimeout:  3 * time.Second,
		LifelineGrace: 300 * time.Millisecond,
		Backoff:       []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 60 * time.Millisecond, 80 * time.Millisecond},
		MaxBackoff:    70 * time.Millisecond,
		MaxFailures:   5,
		StableAfter:   time.Hour,
	}
}

type scripted struct {
	*vm.Process
	mu     sync.Mutex
	script [][]string
	boots  int
	specs  []vm.Spec
}

func (s *scripted) Start(spec vm.Spec, ev vm.Events) (vm.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Process.Extra = nil
	if len(s.script) > 0 {
		for _, instruction := range s.script[min(s.boots, len(s.script)-1)] {
			s.Process.Extra = append(s.Process.Extra, "--standin", instruction)
		}
	}
	s.boots++
	s.specs = append(s.specs, spec)
	return s.Process.Start(spec, ev)
}

func (s *scripted) spec(i int) vm.Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.specs[i]
}

func (s *scripted) started() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boots
}

type fixture struct {
	t      *testing.T
	layout home.Layout
	launch *scripted
	opts   Options
	d      *Daemon
}

func newFixture(t *testing.T, script ...[]string) *fixture {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bh.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	layout, err := home.Resolve(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, layout: layout}
	f.launch = &scripted{
		Process: &vm.Process{Bin: os.Args[0], Prefix: []string{standin.Arg}, Dir: layout.VMDir(), StandIn: true, Control: true},
		script:  script,
	}
	f.opts = Options{Layout: layout, Launcher: f.launch, Timing: quick(), Host: &config.Host{CPUs: 10, MemMiB: 32768}}
	return f
}

func (f *fixture) start() *fixture {
	f.t.Helper()
	d, err := Start(f.opts)
	if err != nil {
		f.t.Fatal(err)
	}
	f.d = d
	f.t.Cleanup(d.Close)
	return f
}

func started(t *testing.T, script ...[]string) *fixture {
	t.Helper()
	return newFixture(t, script...).start()
}

func (f *fixture) conn() net.Conn {
	f.t.Helper()
	conn, err := net.Dial("unix", f.layout.Socket())
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { conn.Close() })
	return conn
}

func (f *fixture) client() *api.Client {
	f.t.Helper()
	c, _, err := api.NewClient(bg, f.conn(), "test")
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) raw() *jsonrpc.Conn {
	f.t.Helper()
	c := jsonrpc.NewConn(f.conn(), jsonrpc.HandlerFunc(func(context.Context, *jsonrpc.Conn, string, json.RawMessage) (any, error) {
		return nil, nil
	}))
	if err := c.Call(bg, api.MethodHello, api.HelloParams{APIVersion: api.APIVersion, Build: version.Build, Client: "raw"}, nil); err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) until(what string, cond func(api.Status) bool) api.Status {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(bg, 15*time.Second)
	defer cancel()
	st, err := f.d.hub.Wait(ctx, cond)
	if err != nil {
		f.t.Fatalf("waiting for %s: the status is %+v", what, f.d.hub.Status())
	}
	return st
}

func (f *fixture) running() api.Status {
	f.t.Helper()
	return f.until("running", func(st api.Status) bool { return st.State == api.StateRunning })
}

func (f *fixture) stopped() api.Status {
	f.t.Helper()
	return f.until("stopped", func(st api.Status) bool { return st.State == api.StateStopped })
}

type history struct {
	mu       sync.Mutex
	snapshot api.Status
	seen     []api.Status
	gaps     int
	configs  []api.ConfigChange
}

func (f *fixture) watch(c *api.Client) *history {
	f.t.Helper()
	h := &history{}
	st, _, err := c.Subscribe(bg, api.Events{
		State:  func(st api.Status) { h.mu.Lock(); h.seen = append(h.seen, st); h.mu.Unlock() },
		Gap:    func(uint64) { h.mu.Lock(); h.gaps++; h.mu.Unlock() },
		Config: func(ch api.ConfigChange) { h.mu.Lock(); h.configs = append(h.configs, ch); h.mu.Unlock() },
	})
	if err != nil {
		f.t.Fatal(err)
	}
	h.snapshot = *st
	return h
}

func (h *history) states(t *testing.T, upTo uint64) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.seen)
		done := n > 0 && h.seen[n-1].Seq >= upTo
		h.mu.Unlock()
		if done || upTo <= h.snapshot.Seq {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the subscriber never saw seq %d", upTo)
		}
		time.Sleep(time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	want := h.snapshot.Seq
	for _, st := range h.seen {
		want++
		if st.Seq != want {
			t.Fatalf("after seq %d came seq %d", want-1, st.Seq)
		}
		s := st.State
		if st.Phase != "" {
			s += "/" + st.Phase
		}
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	if h.gaps != 0 {
		t.Errorf("%d gaps were reported", h.gaps)
	}
	return out
}

func code(err error) int {
	var e *jsonrpc.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func (f *fixture) vmmPID() int {
	f.t.Helper()
	info, err := f.layout.ReadInfo()
	if err != nil {
		f.t.Fatal(err)
	}
	return info.VMMPID
}

func (f *fixture) logFile(name string) string {
	data, _ := os.ReadFile(filepath.Join(f.layout.LogDir(), name))
	return string(data)
}

func (f *fixture) traces() []api.Trace {
	f.t.Helper()
	file, err := os.Open(f.layout.BootTraceLog())
	if err != nil {
		return nil
	}
	defer file.Close()
	var out []api.Trace
	sc := bufio.NewScanner(file)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if err := api.Conforms(sc.Bytes(), api.Trace{}); err != nil {
			f.t.Errorf("a line of the boot trace log does not fit the schema: %v", err)
		}
		var tr api.Trace
		json.Unmarshal(sc.Bytes(), &tr)
		out = append(out, tr)
	}
	return out
}

func readyPipe(t *testing.T) (io.WriteCloser, func() launch.Ready) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	return w, func() launch.Ready {
		defer r.Close()
		var ready launch.Ready
		r.SetReadDeadline(time.Now().Add(20 * time.Second))
		line, _ := bufio.NewReader(r).ReadBytes('\n')
		if err := json.Unmarshal(line, &ready); err != nil {
			t.Fatalf("ready line %q: %v", line, err)
		}
		return ready
	}
}

func contains(list []string, s string) bool {
	for _, l := range list {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

func killPID(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
