package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"fev/proto/bridge"
	api "fev/proto/daemon"
)

type SSHAgent struct {
	Initial   string
	GuestPath func(p string) string
	Pinned    bool

	host Host
	log  *slog.Logger

	mu     sync.Mutex
	path   string
	given  string
	sendMu sync.Mutex
}

const sshAgentCallBound = 10 * time.Second

func (a *SSHAgent) Name() string    { return "ssh-agent" }
func (a *SSHAgent) Marks() []string { return nil }

func (a *SSHAgent) Start(h Host) error {
	a.host, a.log = h, h.Log()
	a.mu.Lock()
	if a.path == "" {
		a.path = a.Initial
	}
	a.mu.Unlock()
	return nil
}

func (a *SSHAgent) StateChanged(st api.Status) {
	if st.State == api.StateRunning {
		go a.sync()
	}
}

func (a *SSHAgent) ConfigChanged(api.ConfigChange) {}
func (a *SSHAgent) Close()                         {}

func (a *SSHAgent) Path() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.path
}

func (a *SSHAgent) Hello(p api.HelloParams) { a.Offer(p.SSHAuthSock) }

func (a *SSHAgent) Offer(p string) {
	if a == nil || !filepath.IsAbs(p) {
		return
	}
	p = filepath.Clean(p)
	a.mu.Lock()
	same := p == a.path
	a.mu.Unlock()
	if same {
		return
	}
	if st, err := os.Stat(p); err != nil || st.Mode()&os.ModeSocket == 0 {
		return
	}
	a.mu.Lock()
	old := a.path
	a.path = p
	a.mu.Unlock()
	if a.log != nil {
		a.log.Info("ssh-agent: the Mac's agent is now a client's SSH_AUTH_SOCK", "path", p, "was", old)
	}
	go a.sync()
}

func (a *SSHAgent) guest(p string) string {
	if p == "" || a.GuestPath == nil {
		return ""
	}
	return a.GuestPath(p)
}

func (a *SSHAgent) BootWords() []string {
	if a == nil || a.Pinned {
		return nil
	}
	a.mu.Lock()
	g := a.guest(a.path)
	a.given = g
	a.mu.Unlock()
	if w := bridge.AgentPathWord(g); w != "" {
		return []string{w}
	}
	return nil
}

func (a *SSHAgent) sync() {
	if a.Pinned || a.host == nil {
		return
	}
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	a.mu.Lock()
	want, given := a.guest(a.path), a.given
	a.mu.Unlock()
	if want == given {
		return
	}
	vm, ok := a.host.VM()
	if !ok || vm.Agent() == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sshAgentCallBound)
	defer cancel()
	r, err := vm.Agent().SSHAgentSet(ctx, want)
	if err != nil {
		a.log.Warn("ssh-agent: the guest did not take the agent's new path; the next boot will", "path", want, "err", err)
		return
	}
	a.mu.Lock()
	a.given = want
	a.mu.Unlock()
	a.log.Info("ssh-agent: the guest binds the agent at its new path", "path", want, "engine", r.Engine, "machines", r.Machines)
}
