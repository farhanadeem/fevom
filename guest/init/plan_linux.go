package main

import (
	"os"
	"strings"

	"fev/guest/internal/engineconf"
	"fev/guest/internal/logbuf"
	"fev/guest/internal/netcfg"
	"fev/proto/netplan"
)

func readPlan(cmdline string, log *logbuf.Buffer) (p netplan.Plan, p6 netplan.Plan6, ok6 bool, err error) {
	p, p6, ok6, err = netplan.FromCmdline(cmdline)
	if err != nil {
		log.Logf(logbuf.Error, "init: %v; the default address plan is used", err)
		return netplan.Default(), netplan.Plan6{}, false, err
	}
	return p, p6, ok6, nil
}

func (s *system) engineBase() engineconf.Base {
	b := engineconf.Base{HostGateway: s.plan.Host().String(), Bip: s.plan.Docker0().String()}
	for _, p := range s.plan.Pools() {
		b.Pools = append(b.Pools, engineconf.Pool{Base: p.Base.String(), Size: p.Size})
	}
	if s.ok6 {
		p := s.plan6.Pool6()
		b.Pool6 = engineconf.Pool{Base: p.Base.String(), Size: p.Size}
	}
	if st, err := os.Stat(engineImage + runtimeWrapper); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
		b.Runtime = runtimeWrapper
	}
	b.Containerd = containerdSocket
	return b
}

const runtimeWrapper = "/usr/bin/fev-runc"

func (s *system) engineEnvironment() []string {
	env := append([]string{}, engineEnv...)
	ep := "http://" + s.plan.ProxyEndpoint().String()
	return append(env, "HTTP_PROXY="+ep, "HTTPS_PROXY="+ep, "NO_PROXY="+noProxy(s.plan, s.plan6, s.ok6))
}

func noProxy(p netplan.Plan, p6 netplan.Plan6, ok6 bool) string {
	list := []string{"localhost", "127.0.0.1", "127.0.0.0/8", "::1",
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7", p.Base().String()}
	if ok6 {
		list = append(list, p6.Prefix().String())
	}
	list = append(list, "*.local", ".local", "*.fev.internal", "host.docker.internal", "host.fev.internal")
	return strings.Join(list, ",")
}

func (s *system) uplink() netcfg.Config {
	if cfg, err := netcfg.FromCmdline(s.cmdline); err == nil {
		return cfg
	}
	return netcfg.FromPlan(s.plan)
}
