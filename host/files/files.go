package files

import (
	"context"
	"encoding/json"
	"log/slog"
	"path"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"fev/host/daemon"
	"fev/host/daemon/vm"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/fsevents"
)

type Options struct {
	Share *vm.Share
}

type Module struct {
	opts Options
	host daemon.Host
	log  *slog.Logger

	mu         sync.Mutex
	boot       string
	vm         daemon.VM
	status     api.FilesStatus
	binds      map[string][]string
	asked      []string
	sending    bool
	paused     bool
	pauses     uint64
	owed       bool
	watchCall  func(ctx context.Context, vm daemon.VM, paths []string) (*agent.WatchResult, error)
	infoCall   func(ctx context.Context, vm daemon.VM) (share, events bool, unbound []string)
	followCall func(vm daemon.VM, deliver func(agent.FileShare))
	bindCall   func(ctx context.Context, vm daemon.VM) (*agent.ShareBound, error)

	told        bool
	engineReady bool
	unbound     []string
	stuck       map[string]bool
	wait        *waiter
	binding     bool
	bindOwed    bool
}

func New(opts Options) *Module {
	return &Module{
		opts: opts, binds: map[string][]string{},
		status: api.FilesStatus{Watch: api.FilesWatch{Paths: []string{}}},
		watchCall: func(ctx context.Context, vm daemon.VM, paths []string) (*agent.WatchResult, error) {
			return vm.Agent().FileShareWatch(ctx, paths)
		},
		infoCall: func(ctx context.Context, vm daemon.VM) (bool, bool, []string) {
			info, err := vm.Agent().Info(ctx)
			if err != nil || info.FileShare == nil {
				return false, false, nil
			}
			return true, hears(*info.FileShare), info.FileShare.Unbound
		},
		followCall: func(vm daemon.VM, deliver func(agent.FileShare)) { vm.Agent().OnFileShareState(deliver) },
		bindCall: func(ctx context.Context, vm daemon.VM) (*agent.ShareBound, error) {
			return vm.Agent().FileShareBind(ctx)
		},
	}
}

func hears(share agent.FileShare) bool {
	return share.Device && share.Feed == agent.FeedConnected
}

func (m *Module) Name() string    { return "files" }
func (m *Module) Marks() []string { return nil }

func (m *Module) Start(h daemon.Host) error {
	m.host, m.log = h, h.Log()
	h.Handle(api.MethodFilesStatus, func(context.Context, json.RawMessage) (any, error) { return m.Status(), nil })
	m.warnShare()
	return nil
}

func (m *Module) Status() api.FilesStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Module) change(change func(*api.FilesStatus)) {
	next := m.status
	change(&next)
	if next.Watch.Paths == nil {
		next.Watch.Paths = []string{}
	}
	if reflect.DeepEqual(next, m.status) {
		return
	}
	m.status = next
	m.host.PublishFiles(next)
}

var samePaths = []string{"/Users", "/Volumes", "/private", "/Applications", "/Library", "/System", "/opt/homebrew", "/usr/local"}

func MacPath(root, enginePath string) (string, bool) {
	p := path.Clean(enginePath)
	if !path.IsAbs(p) {
		return "", false
	}
	under := func(prefix string) (string, bool) {
		if p == prefix {
			return "", true
		}
		return strings.CutPrefix(p, prefix+"/")
	}
	mac := ""
	switch {
	case p == vm.ShareMount || strings.HasPrefix(p, vm.ShareMount+"/"):
		mac = strings.TrimPrefix(p, vm.ShareMount)
	default:
		found := false
		for _, same := range samePaths {
			if _, ok := under(same); ok {
				mac, found = p, true
				break
			}
		}
		if rest, ok := under("/tmp"); ok {
			mac, found = path.Join("/private/tmp", rest), true
		}
		if rest, ok := under("/var/folders"); ok {
			mac, found = path.Join("/private/var/folders", rest), true
		}
		if !found {
			return "", false
		}
	}
	mac = path.Join(root, mac)
	if fsevents.ValidPath(mac) != nil {
		return "", false
	}
	return mac, true
}

func (m *Module) StateChanged(st api.Status) {
	current, haveVM := m.host.VM()
	up := haveVM && st.Engine == api.EngineReady && (st.State == api.StateRunning || st.State == api.StateStarting)
	ready := haveVM && st.Engine == api.EngineReady
	m.mu.Lock()
	if paused := st.State == api.StatePaused; paused != m.paused {
		m.paused = paused
		if paused {
			m.pauses++
		} else {
			if m.owed {
				m.owed = false
				m.sendLocked()
			}
			if m.bindOwed && m.vm != nil {
				m.bindOwed = false
				m.lookLocked(m.vm)
			}
		}
	}
	gone, follow := false, false
	switch {
	case up && current.BootID() != m.boot:
		m.boot, m.vm, m.asked = current.BootID(), current, nil
		m.told, m.engineReady = false, false
		share, _ := m.host.Config().InEffect(api.KeyFilesShare)
		on := share == true && m.opts.Share != nil
		m.change(func(s *api.FilesStatus) {
			*s = api.FilesStatus{Share: on, Watch: api.FilesWatch{Paths: []string{}}, Denied: s.Denied}
			if on {
				s.Root, s.Mount = m.opts.Share.Root, vm.ShareMount
				s.Hidden = append([]string{}, m.opts.Share.Deny...)
			}
		})
		follow = on
	case !haveVM && m.boot != "":
		m.boot, m.vm, m.asked = "", nil, nil
		m.binds = map[string][]string{}
		m.unbound, m.stuck, m.bindOwed = nil, nil, false
		m.wait.stop()
		m.wait = nil
		gone = true
		m.change(func(s *api.FilesStatus) {
			*s = api.FilesStatus{Watch: api.FilesWatch{Paths: []string{}}}
		})
	}
	settle := m.vm != nil && m.vm == current && m.status.Share && ready && !m.engineReady
	m.engineReady = ready && m.vm != nil
	if settle {
		go m.settle(current)
	}
	m.mu.Unlock()
	if follow {
		m.followCall(current, func(share agent.FileShare) { m.shareState(current, share) })
	}
	if gone {
		m.host.Warn(WarnBind, "")
	}
	m.warnShare()
}

func (m *Module) warnShare() {
	off := ""
	share, running := m.host.Config().InEffect(api.KeyFilesShare)
	if !running {
		share = m.host.Config().Bool(api.KeyFilesShare)
	}
	if share != true && m.opts.Share != nil {
		off = "file sharing is off (" + api.KeyFilesShare + " is false): containers cannot bind-mount Mac folders, and a bind mount of one fails in the engine with its own error; " +
			"\"fev config set " + api.KeyFilesShare + " true\" and \"fev restart\" turn it on"
	}
	m.host.Warn(WarnShareOff, off)
}

const WarnShareOff = "files.share_off"

const WarnBind = "files.unbound"

func (m *Module) settle(current daemon.VM) {
	share, events, unbound := m.infoCall(context.Background(), current)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vm != current {
		return
	}
	if !share {
		m.log.Warn("files: the VM was started with the Mac share and the guest has not mounted it; bind mounts of Mac folders will fail")
	}
	if !m.told {
		m.change(func(s *api.FilesStatus) { s.Events = events })
	}
	m.sendLocked()
	m.unbound, m.stuck = unbound, nil
	m.lookLocked(current)
}

func (m *Module) shareState(current daemon.VM, share agent.FileShare) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vm != current {
		return
	}
	m.told = true
	events := hears(share)
	if was := m.status.Events; was && !events {
		m.log.Warn("files: the guest no longer hears of changes made on the Mac; containers' watchers get none until the feed is back",
			"device", share.Device, "feed", clip(share.Feed, 20), "error", clip(share.FeedError, 200))
	} else if !was && events {
		m.log.Info("files: the guest hears of changes made on the Mac")
	}
	m.change(func(s *api.FilesStatus) { s.Events = events })
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (m *Module) macOf(target string) string {
	for _, sp := range agent.SharePaths {
		if sp.Target == target {
			return path.Join(m.opts.Share.Root, sp.Source)
		}
	}
	return ""
}

func (m *Module) lookLocked(current daemon.VM) {
	m.wait.stop()
	m.wait = nil
	var present bool
	var missing []string
	for _, target := range m.unbound {
		mac := m.macOf(target)
		if mac == "" || target == "/tmp" || m.stuck[target] {
			continue
		}
		if isDir(mac) {
			present = true
		} else {
			missing = append(missing, mac)
		}
	}
	if present {
		if m.paused {
			m.bindOwed = true
		} else if !m.binding {
			m.binding = true
			go m.bind(current, m.pauses)
		}
		return
	}
	if len(missing) == 0 {
		return
	}
	m.wait = waitFor(m.opts.Share.Root, missing, func(w *waiter) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.wait == w && m.vm == current {
			m.lookLocked(current)
		}
	})
}

func (m *Module) waiting() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wait == nil {
		return nil
	}
	return m.wait.dirs
}

func (m *Module) bind(current daemon.VM, pauses uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), watchTimeout)
	result, err := m.bindCall(ctx, current)
	cancel()
	m.mu.Lock()
	m.binding = false
	if m.vm != current {
		m.mu.Unlock()
		return
	}
	if err != nil && (m.paused || m.pauses != pauses) {
		if m.paused {
			m.bindOwed = true
		} else {
			m.lookLocked(current)
		}
		m.mu.Unlock()
		return
	}
	if err == nil {
		m.unbound = result.Unbound
		if len(result.Bound) > 0 {
			m.log.Info("files: directories that appeared on the Mac are bound in the running engine", "paths", strings.Join(result.Bound, " "))
		}
	}
	var left []string
	for _, target := range m.unbound {
		if mac := m.macOf(target); mac != "" && target != "/tmp" && isDir(mac) {
			if m.stuck == nil {
				m.stuck = map[string]bool{}
			}
			m.stuck[target] = true
			left = append(left, target)
		}
	}
	warning := ""
	if len(left) > 0 {
		m.log.Warn("files: the guest could not bind a directory that appeared on the Mac", "paths", strings.Join(left, " "), "err", err)
		warning = "the Mac's " + strings.Join(left, ", ") + " appeared after the Docker engine started and could not be bound in it: " +
			"a container that bind-mounts a path there does not get the Mac's files; \"fev restart docker\" binds it"
	}
	m.lookLocked(current)
	m.mu.Unlock()
	m.host.Warn(WarnBind, warning)
}

func (m *Module) ConfigChanged(ch api.ConfigChange) {
	if ch.Key == api.KeyFilesShare {
		m.warnShare()
	}
}
func (m *Module) Close() {}

func (m *Module) Granted(folder string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keep []string
	for _, have := range m.status.Denied {
		if have != folder {
			keep = append(keep, have)
		}
	}
	m.change(func(s *api.FilesStatus) { s.Denied = keep })
}

func (m *Module) Denied(folder string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, have := range m.status.Denied {
		if have == folder {
			return
		}
	}
	m.change(func(s *api.FilesStatus) { s.Denied = append(append([]string{}, s.Denied...), folder) })
}

func (m *Module) Set(owner string, sources []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	root := "/"
	if m.opts.Share != nil {
		root = m.opts.Share.Root
	}
	var mac []string
	for _, src := range sources {
		if p, ok := MacPath(root, src); ok {
			mac = append(mac, p)
		}
	}
	if len(mac) == 0 {
		if _, had := m.binds[owner]; !had {
			return
		}
		delete(m.binds, owner)
	} else {
		m.binds[owner] = mac
	}
	m.sendLocked()
}

func (m *Module) Synced(running map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for owner := range m.binds {
		if !running[owner] {
			delete(m.binds, owner)
		}
	}
	m.sendLocked()
}

func (m *Module) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.binds = map[string][]string{}
	m.sendLocked()
}

func (m *Module) want() []string {
	seen := map[string]bool{}
	set := []string{}
	for _, paths := range m.binds {
		for _, p := range paths {
			if !seen[p] {
				seen[p] = true
				set = append(set, p)
			}
		}
	}
	sort.Strings(set)
	if len(set) > fsevents.MaxPaths {
		m.log.Warn("files: more bind-mount sources than the change feed carries; changes under the rest raise no events in containers",
			"sources", len(set), "watched", fsevents.MaxPaths)
		set = set[:fsevents.MaxPaths]
	}
	return set
}

func (m *Module) sendLocked() {
	if m.vm == nil || !m.status.Share || m.sending {
		return
	}
	set := m.want()
	if reflect.DeepEqual(set, m.asked) || (m.asked == nil && len(set) == 0) {
		return
	}
	m.sending, m.asked = true, set
	go m.send(m.vm, set, m.pauses)
}

var watchTimeout = 30 * time.Second

func (m *Module) send(current daemon.VM, set []string, pauses uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), watchTimeout)
	result, err := m.watchCall(ctx, current, set)
	cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sending = false
	if m.vm != current {
		return
	}
	if err != nil && (m.paused || m.pauses != pauses) {
		m.log.Info("files: the watch set was not taken before the VM was paused; it is sent again at the wake", "paths", len(set), "err", err)
		m.asked = nil
		if m.paused {
			m.owed = true
		} else {
			m.sendLocked()
		}
		return
	}
	if err != nil {
		m.log.Warn("files: the guest did not take the watch set; changes made on the Mac raise no events in containers", "paths", len(set), "err", err)
		m.change(func(s *api.FilesStatus) { s.Events = false })
	} else {
		rejected := map[string]bool{}
		var list []api.FilesRejected
		for _, r := range result.Rejected {
			if len(list) < fsevents.MaxPaths {
				rejected[r.Path] = true
				reason := r.Reason
				switch reason {
				case agent.WatchOutside, agent.WatchMissing, agent.WatchFailed:
				default:
					reason = agent.WatchFailed
				}
				list = append(list, api.FilesRejected{Path: r.Path, Reason: reason})
			}
		}
		paths := []string{}
		for _, p := range set {
			if !rejected[p] {
				paths = append(paths, p)
			}
		}
		m.change(func(s *api.FilesStatus) {
			s.Events = true
			s.Watch = api.FilesWatch{Seq: s.Watch.Seq + 1, Paths: paths, Rejected: list}
		})
	}
	m.sendLocked()
}
