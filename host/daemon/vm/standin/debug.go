package standin

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
)

type debugState struct {
	made bool
}

const (
	standinDebugBytes    int64 = 320864256
	standinDebugPaths          = 104
	standinDebugPackages       = 24
	standinDefaultSet          = "defaultset-standin"
)

func (a *Agent) resolveDebug(t *api.DebugTarget) (*api.DebugStarted, error) {
	if t == nil || t.Target == "" {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "a debug session needs a target")
	}
	name := t.Target
	lower := strings.ToLower(name)
	id := fakeTargetID(name)
	a.mu.Lock()
	a.debug.made = true
	a.mu.Unlock()
	d := &api.DebugStarted{TargetID: id, Name: name}
	switch {
	case strings.HasPrefix(lower, "missing") || strings.HasPrefix(lower, "nosuch"):
		return nil, jsonrpc.Errorf(api.CodeFailed, "no container or image %q here (docker pull %s first)", name, name)
	case t.Kind == api.DebugRequestImage || strings.HasPrefix(lower, "image") || strings.HasPrefix(lower, "img"):
		d.Kind = api.DebugKindImage
		d.ReadOnly = true
		d.Notes = []string{"tools in /nix; galley install NAME adds a package"}
	case strings.HasPrefix(lower, "stopped"):
		d.Kind = api.DebugKindStopped
		d.Image = "busybox:1.37"
		d.ReadOnly = t.ReadOnly
		if t.ReadOnly {
			d.Notes = []string{name + " is stopped; its root is read-only (--read-only)"}
		} else {
			d.Notes = []string{name + " is stopped; changes are committed into it at the session's end while it stays stopped"}
		}
	default:
		d.Kind = api.DebugKindRunning
		d.Image = "nginx:1.29"
		d.ReadOnly = t.ReadOnly
		d.Notes = []string{"tools in /nix; galley install NAME adds a package"}
	}
	return d, nil
}

func fakeTargetID(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])[:12]
}

func (a *Agent) debugSessions() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, s := range a.sessions {
		if s.params.Context == api.ExecDebug {
			n++
		}
	}
	return n
}

func (a *Agent) debugStatus() (any, error) {
	a.mu.Lock()
	made := a.debug.made
	a.mu.Unlock()
	if !made {
		return api.DebugStatus{Exists: false, Sessions: a.debugSessions()}, nil
	}
	return api.DebugStatus{
		Exists: true, Bytes: standinDebugBytes, Paths: standinDebugPaths,
		Packages: []api.DebugPackage{
			{Name: "vim-9.2", StorePath: "/nix/store/1a2b3c4d5e6f7g8h9i0j1k2l3m4n5o6p-vim-9.2", Commands: []string{"vim", "vimdiff"}, Default: true},
		},
		DefaultSet: standinDefaultSet, DefaultWhole: true,
		Sessions: a.debugSessions(),
	}, nil
}

func (a *Agent) debugReset(p api.DebugResetParams) (any, error) {
	if n := a.debugSessions(); n > 0 && !p.Force {
		return nil, jsonrpc.Errorf(api.CodeBusy, "%d debug session(s) are open; close them or reset --force", n)
	}
	a.mu.Lock()
	freed := int64(0)
	if a.debug.made {
		freed = standinDebugBytes
	}
	a.debug.made = false
	a.mu.Unlock()
	return api.DebugReset{FreedBytes: freed}, nil
}
