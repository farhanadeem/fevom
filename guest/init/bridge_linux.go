package main

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"fev/guest/internal/logbuf"
	"fev/guest/internal/netcfg"
	"fev/guest/machines"
	machnet "fev/guest/machines/net"
	api "fev/proto/agent"
	"fev/proto/bridge"
)

const (
	macHelpers = systemMount + bridge.HelperDir
	macLinks   = "/data/@system/mac-bin"
)

func (s *system) startBridge() bool {
	if st, err := os.Stat(filepath.Join(macHelpers, "bin")); err != nil || !st.IsDir() {
		s.log.Logf(logbuf.Warning, "init: the system image has no %s; machines have no mac command", bridge.BinDir)
		return false
	}
	if err := machnet.EnsureLinks(macLinks); err != nil {
		s.log.Logf(logbuf.Warning, "init: the linked commands in %s: %v; machines have only the helper", macLinks, err)
		return true
	}
	target := filepath.Join(macHelpers, "bin")
	if err := unix.Mount(macLinks, target, "", unix.MS_BIND, ""); err != nil {
		s.log.Logf(logbuf.Warning, "init: bind %s at %s: %v; machines have no linked commands", macLinks, target, err)
		return true
	}
	if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
		s.log.Logf(logbuf.Warning, "init: make %s read-only: %v", target, err)
	}
	return true
}

func macAgentPath(cmdline string) string {
	p, ok := bridge.AgentPath(netcfg.Arg(cmdline, bridge.AgentPathParam))
	if !ok {
		return ""
	}
	return p
}

func bindMacAgent(root, socket, mac string) error {
	return machines.BindAgentAt(root, socket, mac)
}

const engineAgentSock = "/run/host-services/ssh-auth.sock"

func (s *system) agentPath() string {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if !s.agentSet {
		s.agentMac, s.agentSet = macAgentPath(s.cmdline), true
	}
	return s.agentMac
}

func (s *system) SSHAgentSet(p string) (api.SSHAgentBound, error) {
	s.agentMu.Lock()
	old := s.agentMac
	if !s.agentSet {
		old = macAgentPath(s.cmdline)
	}
	s.agentMac, s.agentSet = p, true
	s.agentMu.Unlock()
	r := api.SSHAgentBound{Machines: []string{}}
	if s.engine != nil {
		r.Engine = s.engine.followAgent(nil)
	}
	if s.machines != nil {
		r.Machines = s.machines.SetAgentPath(p)
	}
	s.log.Logf(logbuf.Info, "init: the Mac's SSH agent moved from %q to %q: in the engine %v, in machines %v", old, p, r.Engine, r.Machines)
	return r, nil
}

func (e *engine) followAgent(run *dockerdRun) bool {
	if run == nil {
		e.mu.Lock()
		run = e.cur
		e.mu.Unlock()
	}
	if run == nil {
		return false
	}
	want := e.sys.agentPath()
	run.agentMu.Lock()
	defer run.agentMu.Unlock()
	if want == run.agentAt && run.agentKnown {
		return run.agentBound
	}
	run.ns.RLock()
	defer run.ns.RUnlock()
	if run.mnt == nil {
		return false
	}
	var at bool
	err := machines.InMountNS(int(run.mnt.Fd()), func() error {
		var err error
		at, err = machines.MoveAgent(engineAgentSock, run.agentAt, want)
		return err
	})
	run.agentAt, run.agentKnown, run.agentBound = want, true, at
	if err != nil {
		e.sys.log.Logf(logbuf.Info, "engine: the SSH agent is not at the Mac's SSH_AUTH_SOCK path: %v", err)
	}
	return at
}
