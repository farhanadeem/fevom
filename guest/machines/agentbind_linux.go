package machines

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"fev/guest/internal/logbuf"
)

const AgentSock = SSHAuthSock

func BindAgentAt(root, socket, mac string) error {
	if mac == "" {
		return nil
	}
	if root == "" {
		root = "/"
	}
	cur := root
	parts := strings.Split(strings.TrimPrefix(mac, "/"), "/")
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if err != nil {
			return fmt.Errorf("the tree has no %s: %w", mac, err)
		}
		last := i == len(parts)-1
		shown := filepath.Join("/", strings.TrimPrefix(cur, strings.TrimSuffix(root, "/")))
		switch {
		case st.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("%s is a link", shown)
		case !last && !st.IsDir():
			return fmt.Errorf("%s is not a directory", shown)
		case last && st.IsDir():
			return fmt.Errorf("%s is a directory", mac)
		}
	}
	if err := unix.Mount(socket, cur, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind the agent's socket at %s: %w", mac, err)
	}
	return nil
}

func MoveAgent(socket, old, mac string) (bool, error) {
	if old != "" && old != mac && sameFile(old, socket) {
		if err := unix.Unmount(old, unix.MNT_DETACH); err != nil {
			return false, fmt.Errorf("unbind the agent at %s: %w", old, err)
		}
	}
	if mac == "" {
		return false, nil
	}
	if sameFile(mac, socket) {
		return true, nil
	}
	if _, err := os.Stat(socket); err != nil {
		return false, fmt.Errorf("the agent's socket: %w", err)
	}
	if err := BindAgentAt("", socket, mac); err != nil {
		return false, err
	}
	return true, nil
}

func InMountNS(mnt int, fn func() error) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			done <- err
			return
		}
		if err := unix.Setns(mnt, unix.CLONE_NEWNS); err != nil {
			done <- fmt.Errorf("enter the mount namespace: %w", err)
			return
		}
		if err := unix.Chdir("/"); err != nil {
			done <- err
			return
		}
		done <- fn()
	}()
	return <-done
}

func (rt *Runtime) AgentPath() string {
	rt.agentMu.Lock()
	defer rt.agentMu.Unlock()
	return rt.agentPath
}

func (rt *Runtime) SetAgentPath(p string) []string {
	rt.agentMu.Lock()
	rt.agentPath = p
	rt.agentMu.Unlock()
	rt.mu.Lock()
	all := make([]*machine, 0, len(rt.byName))
	for _, m := range rt.byName {
		all = append(all, m)
	}
	rt.mu.Unlock()
	var runs []*run
	for _, m := range all {
		m.mu.Lock()
		if r := m.cur; r != nil {
			runs = append(runs, r)
		}
		m.mu.Unlock()
	}
	bound := []string{}
	for _, r := range runs {
		if b := r.boot(); b != nil && r.followAgent(b) {
			bound = append(bound, r.m.Name())
		}
	}
	sort.Strings(bound)
	return bound
}

func (r *run) followAgent(b *boot) bool {
	if r.cfg.Isolated {
		return false
	}
	want := r.m.rt.AgentPath()
	if !agentPathInMachine(want) {
		want = ""
	}
	b.agentMu.Lock()
	defer b.agentMu.Unlock()
	if want == b.agentAt && b.agentKnown {
		return b.agentBound
	}
	b.nsMu.RLock()
	defer b.nsMu.RUnlock()
	if b.ns == nil {
		return false
	}
	var at bool
	err := InMountNS(int(b.ns["mnt"].Fd()), func() error {
		var err error
		at, err = MoveAgent(AgentSock, b.agentAt, want)
		return err
	})
	b.agentAt, b.agentKnown, b.agentBound = want, true, at
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		r.m.runtimeLogf(logbuf.Info, "the Mac's SSH agent is not at %s in this machine: %v", want, err)
	default:
		r.m.runtimeLogf(logbuf.Warning, "the Mac's SSH agent at %s: %v", want, err)
	}
	return at
}
