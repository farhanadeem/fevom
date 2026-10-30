package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"fev/host/daemon"
	"fev/host/daemon/config"
	"fev/host/daemon/home"
	"fev/host/daemon/vm"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/fsevents"
	"fev/proto/vmmctl"
)

func TestMacPathReadsTheLayoutBackwards(t *testing.T) {
	for engine, mac := range map[string]string{
		"/Users/me/src":                    "/Users/me/src",
		"/Users":                           "/Users",
		"/Users/me/src/../other/":          "/Users/me/other",
		"/Volumes/Disk/x":                  "/Volumes/Disk/x",
		"/private/tmp/x":                   "/private/tmp/x",
		"/Applications/X.app":              "/Applications/X.app",
		"/Library/Fonts":                   "/Library/Fonts",
		"/System/Library":                  "/System/Library",
		"/opt/homebrew/bin":                "/opt/homebrew/bin",
		"/usr/local/share":                 "/usr/local/share",
		"/tmp/tmp.Xq3f":                    "/private/tmp/tmp.Xq3f",
		"/tmp":                             "/private/tmp",
		"/var/folders/ab/cd/T/x":           "/private/var/folders/ab/cd/T/x",
		"/mnt/mac/nix/store":               "/nix/store",
		"/mnt/mac":                         "/",
		"/mnt/mac/Users/me":                "/Users/me",
		"/var/lib/docker/volumes/v/_data":  "",
		"/var/run/docker.sock":             "",
		"/run/host-services/ssh-auth.sock": "",
		"/etc/hosts":                       "",
		"/usr/bin":                         "",
		"/opt/other":                       "",
		"/Usersx/me":                       "",
		"/tmpfoo":                          "",
		"/":                                "",
		"relative/path":                    "",
		"":                                 "",
		"/Users/me/\xff":                   "",
	} {
		got, ok := MacPath("/", engine)
		if got != mac || ok != (mac != "") {
			t.Errorf("MacPath(/, %q) = %q, %v; want %q", engine, got, ok, mac)
		}
	}
	for engine, mac := range map[string]string{
		"/Users/me/src": "/tmp/scratch/Users/me/src", "/tmp/x": "/tmp/scratch/private/tmp/x", "/mnt/mac": "/tmp/scratch",
	} {
		if got, ok := MacPath("/tmp/scratch", engine); got != mac || !ok {
			t.Errorf("MacPath(/tmp/scratch, %q) = %q, %v; want %q", engine, got, ok, mac)
		}
	}
}

type host struct {
	layout home.Layout
	cfg    *config.Store

	mu        sync.Mutex
	vm        daemon.VM
	published []api.FilesStatus
	wake      chan struct{}
	warnings  map[string]string
}

func (h *host) Layout() home.Layout   { return h.layout }
func (h *host) Log() *slog.Logger     { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func (h *host) Config() *config.Store { return h.cfg }
func (h *host) Fake() bool            { return true }
func (h *host) Status() api.Status    { return api.Status{} }
func (h *host) VM() (daemon.VM, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.vm, h.vm != nil
}
func (h *host) Mark(string)                        {}
func (h *host) Handle(string, daemon.MethodFunc)   {}
func (h *host) PublishForward(api.ForwardsChanged) {}
func (h *host) PublishDocker(api.DockerEvent)      {}
func (h *host) StopVM(context.Context) error       { return nil }
func (h *host) StartVM() error                     { return nil }
func (h *host) CheckAlive()                        {}
func (h *host) Rouse(bool) bool                    { return false }
func (h *host) StartOnDemand() error               { return nil }

func (h *host) StartAsked() error { return nil }
func (h *host) WaitStatus(context.Context, func(api.Status) bool) (api.Status, error) {
	return api.Status{}, errors.New("the fake host has no status to wait for")
}
func (h *host) PublishMachine(api.MachinesChanged) {}
func (h *host) PublishOperation(api.Operation)     {}
func (h *host) Warn(key, text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.warnings == nil {
		h.warnings = map[string]string{}
	}
	h.warnings[key] = text
}
func (h *host) warning(key string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.warnings[key]
}
func (h *host) PublishFiles(status api.FilesStatus) {
	h.mu.Lock()
	h.published = append(h.published, status)
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}
func (h *host) WorkEnded() {}

type fakeVM struct{ boot string }

func (v fakeVM) VsockPath(uint32) string { return "" }
func (v fakeVM) Agent() *agent.Client    { return nil }
func (v fakeVM) BootID() string          { return v.boot }
func (v fakeVM) Control() *vmmctl.Client { return nil }

type guest struct {
	mu      sync.Mutex
	sets    [][]string
	fail    error
	missing map[string]bool
	hold    chan struct{}
}

func (g *guest) watch(ctx context.Context, _ daemon.VM, paths []string) (*agent.WatchResult, error) {
	g.mu.Lock()
	g.sets = append(g.sets, append([]string{}, paths...))
	fail, hold := g.fail, g.hold
	g.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if fail != nil {
		return nil, fail
	}
	r := &agent.WatchResult{}
	for _, p := range paths {
		if g.missing[p] {
			r.Rejected = append(r.Rejected, agent.WatchRejected{Path: p, Reason: agent.WatchMissing})
		} else {
			r.Watching++
		}
	}
	return r, nil
}

func (g *guest) asked() [][]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([][]string{}, g.sets...)
}

type world struct {
	t *testing.T
	*host
	m *Module
	g *guest
}

func newWorld(t *testing.T, share *vm.Share) *world {
	t.Helper()
	dir := t.TempDir()
	layout := home.Layout{Home: dir, DataDir: filepath.Join(dir, "data")}
	if err := os.MkdirAll(filepath.Dir(layout.ConfigFile()), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(layout.ConfigFile(), config.Keys(config.ThisMac()), func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, host: &host{layout: layout, cfg: cfg, wake: make(chan struct{}, 1)}, g: &guest{missing: map[string]bool{}}}
	w.m = New(Options{Share: share})
	w.m.watchCall = w.g.watch
	w.m.infoCall = func(context.Context, daemon.VM) (bool, bool, []string) { return true, true, nil }
	w.m.followCall = func(daemon.VM, func(agent.FileShare)) {}
	if err := w.m.Start(w.host); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) boot(id string) {
	w.cfg.VMStarted()
	w.mu.Lock()
	w.vm = fakeVM{id}
	w.mu.Unlock()
	w.m.StateChanged(api.Status{State: api.StateRunning, Engine: api.EngineReady, BootID: id})
}

func (w *world) stop() {
	w.cfg.VMStopped()
	w.mu.Lock()
	w.vm = nil
	w.mu.Unlock()
	w.m.StateChanged(api.Status{State: api.StateStopped, Engine: api.EngineStopped})
}

func (w *world) until(what string, ok func(api.FilesStatus) bool) api.FilesStatus {
	w.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if st := w.m.Status(); ok(st) {
			w.mu.Lock()
			last := w.published[len(w.published)-1]
			w.mu.Unlock()
			if !reflect.DeepEqual(last, st) {
				w.t.Fatalf("%s: files.status is %+v and the last files.changed %+v", what, st, last)
			}
			return st
		}
		select {
		case <-w.wake:
		case <-deadline:
			w.t.Fatalf("%s: the status is %+v", what, w.m.Status())
		}
	}
}

var share = &vm.Share{Root: "/", Deny: []string{"/Users/me/.fev/data", "/Users/me/.fev/run"}}

func TestTheWatchSetFollowsTheContainers(t *testing.T) {
	w := newWorld(t, share)
	if st := w.m.Status(); st.Share || st.Events || st.Watch.Seq != 0 || st.Watch.Paths == nil {
		t.Fatalf("before any VM: %+v", st)
	}
	w.boot("b1")
	st := w.until("the share", func(st api.FilesStatus) bool { return st.Share && st.Events })
	if st.Root != "/" || st.Mount != "/mnt/mac" || !reflect.DeepEqual(st.Hidden, share.Deny) || st.Watch.Seq != 0 || len(st.Watch.Paths) != 0 {
		t.Fatalf("a VM with the share and no container: %+v", st)
	}
	w.m.Set("c0", []string{"/var/lib/docker/volumes/v/_data", "/var/run/docker.sock"})
	w.m.Set("c0", nil)
	w.m.Synced(map[string]bool{})
	if sets := w.g.asked(); len(sets) != 0 {
		t.Fatalf("sets sent with no bind mount of a Mac path: %v", sets)
	}

	w.m.Set("c1", []string{"/Users/me/src/app", "/tmp/tmp.Xq3f", "/var/lib/docker/volumes/v/_data"})
	st = w.until("the first set", func(st api.FilesStatus) bool { return st.Watch.Seq == 1 })
	if !reflect.DeepEqual(st.Watch.Paths, []string{"/Users/me/src/app", "/private/tmp/tmp.Xq3f"}) || len(st.Watch.Rejected) != 0 {
		t.Fatalf("the first set: %+v", st.Watch)
	}
	w.m.Set("c2", []string{"/Users/me/src/app"})
	w.g.missing["/Users/me/gone"] = true
	w.m.Set("c3", []string{"/Users/me/gone"})
	st = w.until("the second set", func(st api.FilesStatus) bool { return st.Watch.Seq == 2 })
	if !reflect.DeepEqual(st.Watch.Paths, []string{"/Users/me/src/app", "/private/tmp/tmp.Xq3f"}) ||
		!reflect.DeepEqual(st.Watch.Rejected, []api.FilesRejected{{Path: "/Users/me/gone", Reason: "missing"}}) {
		t.Fatalf("the second set: %+v", st.Watch)
	}
	w.m.Set("c1", nil)
	st = w.until("the third set", func(st api.FilesStatus) bool { return st.Watch.Seq == 3 })
	if !reflect.DeepEqual(st.Watch.Paths, []string{"/Users/me/src/app"}) {
		t.Fatalf("the third set: %+v", st.Watch)
	}
	w.m.Clear()
	st = w.until("the empty set", func(st api.FilesStatus) bool { return st.Watch.Seq == 4 })
	if len(st.Watch.Paths) != 0 || len(st.Watch.Rejected) != 0 {
		t.Fatalf("the empty set: %+v", st.Watch)
	}
	want := [][]string{
		{"/Users/me/src/app", "/private/tmp/tmp.Xq3f"},
		{"/Users/me/gone", "/Users/me/src/app", "/private/tmp/tmp.Xq3f"},
		{"/Users/me/gone", "/Users/me/src/app"},
		{},
	}
	if sets := w.g.asked(); !reflect.DeepEqual(sets, want) {
		t.Fatalf("sets sent:\n%v\nwant\n%v", sets, want)
	}
	w.stop()
	st = w.until("stopped", func(st api.FilesStatus) bool { return !st.Share })
	if st.Events || st.Root != "" || st.Watch.Seq != 0 || len(st.Hidden) != 0 {
		t.Fatalf("after the stop: %+v", st)
	}
	w.boot("b2")
	w.m.Set("c9", []string{"/Users/me/src/app"})
	st = w.until("the next VM's first set", func(st api.FilesStatus) bool { return st.Share && st.Watch.Seq == 1 })
	if !reflect.DeepEqual(st.Watch.Paths, []string{"/Users/me/src/app"}) {
		t.Fatalf("the next VM: %+v", st.Watch)
	}
}

func TestSetsAreSentOneAtATimeAndTheNewestWins(t *testing.T) {
	w := newWorld(t, share)
	w.g.hold = make(chan struct{})
	w.boot("b1")
	w.m.Set("c1", []string{"/Users/me/a"})
	for len(w.g.asked()) == 0 {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		w.m.Set(fmt.Sprintf("x%d", i), []string{fmt.Sprintf("/Users/me/x%d", i)})
	}
	w.m.Set("c2", []string{"/Users/me/b"})
	for i := 0; i < 20; i++ {
		w.m.Set(fmt.Sprintf("x%d", i), nil)
	}
	close(w.g.hold)
	st := w.until("the second set", func(st api.FilesStatus) bool { return st.Watch.Seq == 2 })
	if !reflect.DeepEqual(st.Watch.Paths, []string{"/Users/me/a", "/Users/me/b"}) {
		t.Fatalf("watching %v", st.Watch.Paths)
	}
	if sets := w.g.asked(); len(sets) != 2 {
		t.Fatalf("%d sets were sent for one change while another was on its way", len(sets))
	}
}

func TestAGuestWithNoFeedIsNotAskedAgainUntilTheSetChanges(t *testing.T) {
	w := newWorld(t, share)
	w.g.fail = errors.New("the VMM does not answer the change feed")
	w.boot("b1")
	w.until("events", func(st api.FilesStatus) bool { return st.Events })
	w.m.Set("c1", []string{"/Users/me/a"})
	st := w.until("no events", func(st api.FilesStatus) bool { return !st.Events })
	if !st.Share || st.Watch.Seq != 0 || len(st.Watch.Paths) != 0 {
		t.Fatalf("after the failure: %+v", st)
	}
	w.m.Set("c1", []string{"/Users/me/a"})
	w.m.Synced(map[string]bool{"c1": true})
	if sets := w.g.asked(); len(sets) != 1 {
		t.Fatalf("the same set was sent %d times", len(sets))
	}
	w.g.mu.Lock()
	w.g.fail = nil
	w.g.mu.Unlock()
	w.m.Set("c2", []string{"/Users/me/b"})
	st = w.until("the feed is back", func(st api.FilesStatus) bool { return st.Events && st.Watch.Seq == 1 })
	if !reflect.DeepEqual(st.Watch.Paths, []string{"/Users/me/a", "/Users/me/b"}) {
		t.Fatalf("watching %v", st.Watch.Paths)
	}
}

func TestWithNoShareNothingIsWatched(t *testing.T) {
	w := newWorld(t, share)
	if _, err := w.cfg.Set(map[string]any{api.KeyFilesShare: false}); err != nil {
		t.Fatal(err)
	}
	w.boot("b1")
	w.m.Set("c1", []string{"/Users/me/a"})
	if st := w.m.Status(); st.Share || st.Events || st.Root != "" || len(st.Watch.Paths) != 0 {
		t.Fatalf("with the setting off: %+v", st)
	}
	warning := func(h *host) string {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.warnings[WarnShareOff]
	}
	if got := warning(w.host); !strings.Contains(got, "file sharing is off") || !strings.Contains(got, "cannot bind-mount Mac folders") || !strings.Contains(got, api.KeyFilesShare) {
		t.Errorf("the status warning with the setting off: %q", got)
	}
	w.stop()
	if got := warning(w.host); !strings.Contains(got, "file sharing is off") {
		t.Errorf("with the VM stopped and the setting off: %q", got)
	}
	changed, err := w.cfg.Set(map[string]any{api.KeyFilesShare: true})
	if err != nil || len(changed) != 1 {
		t.Fatal(changed, err)
	}
	w.m.ConfigChanged(changed[0])
	if got := warning(w.host); got != "" {
		t.Errorf("with the setting on again: %q", got)
	}
	on := newWorld(t, share)
	on.boot("b1")
	if got := warning(on.host); got != "" {
		t.Errorf("with the share on: %q", got)
	}
	none := newWorld(t, nil)
	none.boot("b1")
	none.m.Set("c1", []string{"/Users/me/a"})
	if st := none.m.Status(); st.Share {
		t.Fatalf("a daemon with nothing to share: %+v", st)
	}
	if got := warning(none.host); got != "" {
		t.Errorf("a daemon with nothing to share: %q", got)
	}
	if len(w.g.asked())+len(none.g.asked()) != 0 {
		t.Fatal("a set was sent to a VM with no share")
	}
}

func TestDeniedFoldersAreReported(t *testing.T) {
	w := newWorld(t, share)
	w.m.Denied("/Users/me/Documents")
	w.boot("b1")
	w.m.Denied("/Users/me/Documents")
	w.m.Denied("/Users/me/Desktop")
	st := w.until("two folders", func(st api.FilesStatus) bool { return len(st.Denied) == 2 && st.Share })
	if !reflect.DeepEqual(st.Denied, []string{"/Users/me/Documents", "/Users/me/Desktop"}) {
		t.Fatalf("denied: %v", st.Denied)
	}
	w.stop()
	if st := w.m.Status(); len(st.Denied) != 0 {
		t.Fatalf("after the stop: %v", st.Denied)
	}
}

func TestTheSetIsBounded(t *testing.T) {
	w := newWorld(t, share)
	w.boot("b1")
	var sources []string
	for i := 0; i < fsevents.MaxPaths+50; i++ {
		sources = append(sources, fmt.Sprintf("/Users/me/p%04d", i))
	}
	w.m.Set("c1", sources)
	st := w.until("the set", func(st api.FilesStatus) bool { return st.Watch.Seq == 1 })
	if len(st.Watch.Paths) != fsevents.MaxPaths || st.Watch.Paths[0] != "/Users/me/p0000" {
		t.Fatalf("%d paths watched, the first %q", len(st.Watch.Paths), st.Watch.Paths[0])
	}
	if _, err := fsevents.Append(nil, fsevents.Watch{Seq: 1, Paths: w.g.asked()[0]}); err != nil {
		t.Fatalf("the set sent is not one the feed carries: %v", err)
	}
}

func TestAWatchSetInFlightAcrossAPauseIsSentAgainAfterTheWake(t *testing.T) {
	old := watchTimeout
	watchTimeout = 100 * time.Millisecond
	defer func() { watchTimeout = old }()
	w := newWorld(t, share)
	parked := make(chan struct{})
	calls := make(chan struct{}, 8)
	w.m.watchCall = func(ctx context.Context, vm daemon.VM, paths []string) (*agent.WatchResult, error) {
		calls <- struct{}{}
		select {
		case <-parked:
			return w.g.watch(ctx, vm, paths)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	w.boot("b1")
	w.until("events", func(st api.FilesStatus) bool { return st.Events })
	w.m.Set("c1", []string{"/Users/me/a"})
	<-calls
	w.m.StateChanged(api.Status{State: api.StatePaused, PauseReason: api.PauseSleep, Engine: api.EngineReady, BootID: "b1"})
	time.Sleep(3 * watchTimeout)
	if st := w.m.Status(); !st.Events {
		t.Fatalf("the feed was reported gone because a parked guest did not answer: %+v", st)
	}
	select {
	case <-calls:
		t.Fatal("the set was sent again while the VM was paused")
	default:
	}
	close(parked)
	w.m.StateChanged(api.Status{State: api.StateRunning, Engine: api.EngineReady, BootID: "b1"})
	st := w.until("the set after the wake", func(st api.FilesStatus) bool { return st.Watch.Seq == 1 })
	if !st.Events || !reflect.DeepEqual(st.Watch.Paths, []string{"/Users/me/a"}) {
		t.Fatalf("after the wake: %+v", st)
	}
}

func TestTheFeedsStateIsWhatTheGuestLastSaid(t *testing.T) {
	w := newWorld(t, share)
	var mu sync.Mutex
	var tell func(agent.FileShare)
	atHello := agent.FeedConnected
	w.m.followCall = func(_ daemon.VM, deliver func(agent.FileShare)) {
		mu.Lock()
		first := atHello
		mu.Unlock()
		deliver(agent.FileShare{Tag: "mac", Target: "/mnt/mac", Device: true, Feed: first})
		mu.Lock()
		tell = deliver
		mu.Unlock()
	}
	answer := make(chan struct{})
	w.m.infoCall = func(context.Context, daemon.VM) (bool, bool, []string) {
		<-answer
		return true, true, nil
	}
	w.boot("b1")
	w.until("the state sent at the hello", func(st api.FilesStatus) bool { return st.Events })
	say := func(s agent.FileShare) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			deliver := tell
			mu.Unlock()
			if deliver != nil {
				deliver(s)
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the module never asked to be told of the share's state")
			}
			time.Sleep(time.Millisecond)
		}
	}
	say(agent.FileShare{Tag: "mac", Target: "/mnt/mac", Device: true, Feed: agent.FeedLost, FeedError: "the VMM closed the change feed"})
	if st := w.until("the feed lost", func(st api.FilesStatus) bool { return !st.Events }); st.Events {
		t.Fatalf("a feed the guest calls lost: %+v", st)
	}
	close(answer)
	time.Sleep(50 * time.Millisecond)
	if st := w.m.Status(); st.Events {
		t.Fatalf("an old answer overruled the guest's word: %+v", st)
	}
	say(agent.FileShare{Tag: "mac", Target: "/mnt/mac", Device: true, Feed: agent.FeedConnected})
	w.until("the feed back", func(st api.FilesStatus) bool { return st.Events })
	say(agent.FileShare{Tag: "mac", Target: "/mnt/mac", Device: false, Feed: agent.FeedConnected})
	w.until("no device", func(st api.FilesStatus) bool { return !st.Events })
	say(agent.FileShare{Tag: "mac", Target: "/mnt/mac", Device: true, Feed: agent.FeedMismatch, FeedError: "another build"})
	if st := w.m.Status(); st.Events {
		t.Fatalf("a feed from another build: %+v", st)
	}
	mu.Lock()
	old := tell
	atHello = agent.FeedNone
	mu.Unlock()
	w.stop()
	w.boot("b2")
	old(agent.FileShare{Device: true, Feed: agent.FeedConnected})
	if st := w.m.Status(); st.Events {
		t.Fatalf("a state from the boot before: %+v", st)
	}
}

func TestAGrantedFolderIsWithdrawn(t *testing.T) {
	w := newWorld(t, share)
	w.boot("b1")
	w.m.Denied("/Users/me/Documents")
	w.m.Denied("/Users/me/Desktop")
	w.until("two folders", func(st api.FilesStatus) bool { return len(st.Denied) == 2 })
	w.m.Granted("/Users/me/Documents")
	w.m.Granted("/Users/me/Music")
	st := w.until("one withdrawn", func(st api.FilesStatus) bool { return len(st.Denied) == 1 })
	if !reflect.DeepEqual(st.Denied, []string{"/Users/me/Desktop"}) {
		t.Fatalf("denied: %v", st.Denied)
	}
	w.m.Granted("/Users/me/Desktop")
	if st := w.until("both withdrawn", func(st api.FilesStatus) bool { return len(st.Denied) == 0 }); st.Denied != nil {
		t.Fatalf("an empty list is left out: %#v", st.Denied)
	}
}

type binder struct {
	mu    sync.Mutex
	root  string
	bound map[string]bool
	calls int
	fail  error
}

func (b *binder) unbound() []string {
	var out []string
	for _, sp := range agent.SharePaths {
		if !b.bound[sp.Target] {
			out = append(out, sp.Target)
		}
	}
	return out
}

func (b *binder) bind(context.Context, daemon.VM) (*agent.ShareBound, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.fail != nil {
		return nil, b.fail
	}
	r := &agent.ShareBound{Bound: []string{}}
	for _, sp := range agent.SharePaths {
		if b.bound[sp.Target] || sp.Target == "/tmp" {
			continue
		}
		if st, err := os.Lstat(filepath.Join(b.root, sp.Source)); err == nil && st.IsDir() {
			b.bound[sp.Target] = true
			r.Bound = append(r.Bound, sp.Target)
		}
	}
	r.Unbound = b.unbound()
	return r, nil
}

func (b *binder) called() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *binder) has(target string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bound[target]
}

func TestADirectoryThatAppearsOnTheMacIsBoundInTheRunningEngine(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"Users", "private/tmp", "private/var/folders", "Volumes", "Applications", "Library", "System", "usr/local"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w := newWorld(t, &vm.Share{Root: root})
	b := &binder{root: root, bound: map[string]bool{}}
	for _, sp := range agent.SharePaths {
		b.bound[sp.Target] = sp.Target != "/opt/homebrew"
	}
	w.m.bindCall = b.bind
	w.m.infoCall = func(context.Context, daemon.VM) (bool, bool, []string) { return true, true, b.unbound() }
	w.boot("b1")
	until := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d bind calls, watching %v", what, b.called(), w.m.waiting())
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	until("watching the root", func() bool { return reflect.DeepEqual(w.m.waiting(), []string{root}) })
	if err := os.Mkdir(filepath.Join(root, "cores"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "opt"), 0o755); err != nil {
		t.Fatal(err)
	}
	until("watching /opt", func() bool { return reflect.DeepEqual(w.m.waiting(), []string{filepath.Join(root, "opt")}) })
	if n := b.called(); n != 0 {
		t.Fatalf("the guest was asked %d times with nothing to bind", n)
	}
	if err := os.Mkdir(filepath.Join(root, "opt", "homebrew"), 0o755); err != nil {
		t.Fatal(err)
	}
	until("bound", func() bool { return b.has("/opt/homebrew") })
	until("nothing left to watch", func() bool { return len(w.m.waiting()) == 0 })
	if n := b.called(); n != 1 {
		t.Errorf("the guest was asked %d times for one directory", n)
	}
	if text := w.warning(WarnBind); text != "" {
		t.Errorf("a warning with everything bound: %q", text)
	}
}

func TestABindTheGuestCannotMakeIsAWarningUntilItCan(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"Users", "opt/homebrew"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w := newWorld(t, &vm.Share{Root: root})
	b := &binder{root: root, bound: map[string]bool{"/Users": true}, fail: errors.New("the engine is not ready")}
	w.m.bindCall = b.bind
	w.m.infoCall = func(context.Context, daemon.VM) (bool, bool, []string) { return true, true, b.unbound() }
	w.boot("b1")
	deadline := time.Now().Add(5 * time.Second)
	for w.warning(WarnBind) == "" {
		if time.Now().After(deadline) {
			t.Fatalf("no warning after %d bind calls", b.called())
		}
		time.Sleep(2 * time.Millisecond)
	}
	if text := w.warning(WarnBind); !strings.Contains(text, "/opt/homebrew") || !strings.Contains(text, "fev restart docker") {
		t.Errorf("the warning: %q", text)
	}
	b.mu.Lock()
	b.fail = nil
	b.mu.Unlock()
	w.m.StateChanged(api.Status{State: api.StateRunning, Engine: api.EngineStarting, BootID: "b1"})
	w.m.StateChanged(api.Status{State: api.StateRunning, Engine: api.EngineReady, BootID: "b1"})
	for w.warning(WarnBind) != "" || !b.has("/opt/homebrew") {
		if time.Now().After(deadline) {
			t.Fatalf("the warning stays: %q after %d bind calls", w.warning(WarnBind), b.called())
		}
		time.Sleep(2 * time.Millisecond)
	}
	for len(w.m.waiting()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("nothing is watched for the directories the root lacks")
		}
		time.Sleep(2 * time.Millisecond)
	}
	w.stop()
	if left := w.m.waiting(); len(left) != 0 {
		t.Fatalf("still watching %v with no VM", left)
	}
}
