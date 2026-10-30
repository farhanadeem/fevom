package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	api "fev/proto/agent"

	"fev/guest/agent"
	policy "fev/guest/internal/emulation"
	"fev/guest/internal/logbuf"
	"fev/guest/internal/netcfg"
	"fev/guest/machines"
	machnet "fev/guest/machines/net"
	"fev/proto/netplan"
)

const (
	machinesData   = "/data/@machines"
	machinesCgroup = cgroupRoot + "/fev.machines"
	machinesRun    = "/run/fev/machines"
	machinesView   = "/run/fev/mnt-machines"
)

const maxMapCount = "1048576"

func newMachines(s *system) *machines.Runtime {
	return machines.New(machines.Options{
		Emulation: func() policy.Config {
			if s.emu != nil {
				return s.emu.config()
			}
			return policy.Config{Mode: policy.FEX}
		},
		DataDir: machinesData, CgroupDir: machinesCgroup, RunDir: machinesRun,
		Self: initPath(), Dev: "/dev", SysBlock: "/sys/class/block",
		Reaper: s.reaper, Log: s.log, Network: newMachineNetwork(s), View: machinesView,
		Helpers:   macHelpers,
		Resolver:  s.plan.MachinesGateway().String(),
		AgentPath: s.agentPath(),
		Notify: func(st api.MachineState) {
			if link := s.host.Load(); link != nil {
				link.Notify(api.NotifyMachineState, st)
			}
			s.viewMachinesChanged()
		},
	})
}

const raRefreshParam = "fev.ra_refresh_s"

func newMachineNetwork(s *system) *machnet.Network {
	var prefix6 netip.Prefix
	if s.ok6 {
		prefix6 = s.plan6.Subnet(netplan.Subnet6Machines)
	}
	var refresh time.Duration
	if v, err := strconv.Atoi(netcfg.Arg(s.cmdline, raRefreshParam)); err == nil && v > 0 {
		refresh = time.Duration(v) * time.Second
	}
	n := machnet.New(machnet.Options{
		Prefix:       s.plan.Machines(),
		Prefix6:      prefix6,
		RefreshAfter: refresh,
		Upstream:     netip.AddrPortFrom(s.uplink().DNS, 53), FirewallRoot: engineImage,
		ForwardLocal: true,
		Reaper:       s.reaper, Log: s.log,
		Requests: s.requests, Links: macLinks,
		DialEngine: func() (net.Conn, error) {
			if s.engine.sup.Status().State != api.EngineReady {
				return nil, errors.New("the engine is not ready")
			}
			return net.Dial("unix", engineSocket)
		},
		Members: func(gone netip.Addr) { s.web.machines(gone) },
		Uplink:  s.direct.uplinkName, Base: s.plan.Base(),
	})
	n.FollowPorts(func(p api.MachinePorts) {
		if link := s.host.Load(); link != nil {
			link.Conn.Notify(api.NotifyMachinePorts, p)
		}
	})
	s.net = n
	var mu sync.Mutex
	was := ""
	s.engine.sup.Subscribe(func(st api.EngineStatus) {
		mu.Lock()
		became := st.State == api.EngineReady && was != api.EngineReady
		was = st.State
		mu.Unlock()
		if became {
			n.EngineReady()
		}
	})
	return n
}

func (s *system) startMachines() error {
	if err := os.WriteFile("/proc/sys/vm/max_map_count", []byte(maxMapCount), 0); err != nil {
		s.log.Logf(logbuf.Warning, "init: cannot set vm.max_map_count: %v", err)
	}
	s.mu.Lock()
	vol, volErr := s.vol, s.volErr
	s.mu.Unlock()
	if vol == nil {
		why := errors.New("there is no data disk")
		if volErr != nil {
			why = fmt.Errorf("the data disk cannot be used: %w", volErr)
		}
		s.machines.Unavailable(why)
		return nil
	}
	s.startBridge()
	if err := machines.EnsureView(machinesView, engineVolumes()); err != nil {
		s.log.Logf(logbuf.Warning, "init: %v; machines have no /mnt/machines", err)
	}
	st, err := vol.Machines()
	if err != nil {
		s.machines.Unavailable(err)
		return nil
	}
	return s.machines.Open(st)
}

func engineVolumes() string {
	if st, err := os.Stat(engineData); err != nil || !st.IsDir() {
		return ""
	}
	return filepath.Join(engineData, "volumes")
}

func machineErr(err error) error {
	if errors.Is(err, machines.ErrNoMachine) {
		return fmt.Errorf("%w: %v", agent.ErrNoMachine, err)
	}
	return err
}

func (s *system) TrustSet(ders [][]byte) (api.Trusted, error) { return s.machines.TrustSet(ders) }

func (s *system) MachineList() (*api.MachineList, error) {
	l, err := s.machines.List()
	if err != nil {
		return nil, machineErr(err)
	}
	return &l, nil
}

func (s *system) MachineInfo(name string) (*api.MachineInfo, error) {
	i, err := s.machines.Info(name)
	if err != nil {
		return nil, machineErr(err)
	}
	return &i, nil
}

func (s *system) MachineStart(name string, cfg api.MachineConfig) (*api.MachineInfo, error) {
	s.viewIsolated(name, cfg.Isolated)
	i, err := s.machines.Start(name, cfg)
	if err != nil {
		return nil, machineErr(err)
	}
	return &i, nil
}

func (s *system) MachineStop(name string, grace time.Duration, force bool) (*api.MachineInfo, error) {
	i, err := s.machines.Stop(name, grace, force)
	if err != nil {
		return nil, machineErr(err)
	}
	return &i, nil
}

func (s *system) MachineRestart(name string, grace time.Duration, cfg api.MachineConfig) (*api.MachineInfo, error) {
	s.viewIsolated(name, cfg.Isolated)
	i, err := s.machines.Restart(name, grace, cfg)
	if err != nil {
		return nil, machineErr(err)
	}
	return &i, nil
}

func (s *system) MachineCreate(p api.MachineCreateParams) (agent.Making, error) {
	s.viewIsolated(p.Name, p.Config.Isolated)
	c, err := s.machines.BeginCreate(p)
	if err != nil {
		return nil, machineErr(err)
	}
	return making{c: c, create: true}, nil
}

func (s *system) MachineImport(name string, cfg api.MachineConfig) (agent.Making, error) {
	s.viewIsolated(name, cfg.Isolated)
	c, err := s.machines.BeginImport(name, cfg)
	if err != nil {
		return nil, machineErr(err)
	}
	return making{c: c}, nil
}

func (s *system) MachineExport(ctx context.Context, name string, w io.Writer, progress agent.Progress) (int64, error) {
	n, err := s.machines.Export(ctx, name, w, machines.Progress(progress))
	return n, machineErr(err)
}

func (s *system) MachineDelete(name string, grace time.Duration, force bool) error {
	return machineErr(s.machines.Delete(name, grace, force))
}

func (s *system) MachineClone(name, newName string) (*api.MachineInfo, error) {
	if o := s.view.Objects(); o != nil {
		o.SetIsolated(newName, o.Isolated(name))
	}
	i, err := s.machines.Clone(name, newName)
	if err != nil {
		return nil, machineErr(err)
	}
	return &i, nil
}

func (s *system) MachineRename(name, newName string) (*api.MachineInfo, error) {
	i, err := s.machines.Rename(name, newName)
	if err != nil {
		return nil, machineErr(err)
	}
	return &i, nil
}

type making struct {
	c      *machines.Creation
	create bool
}

func (m making) NeedStream() bool { return m.c.NeedImage }
func (m making) Abandon()         { m.c.Abandon() }

func (m making) Run(ctx context.Context, stream io.Reader, progress agent.Progress) (any, error) {
	if m.create {
		r, err := m.c.Create(ctx, stream, machines.Progress(progress))
		if err != nil && r.Machine.Name == "" {
			return nil, machineErr(err)
		}
		return r, machineErr(err)
	}
	i, err := m.c.Import(ctx, stream, machines.Progress(progress))
	if err != nil {
		return nil, machineErr(err)
	}
	return i, nil
}

func (s *system) MacAddressesSet(addresses []string) error {
	addrs := make([]netip.Addr, 0, len(addresses))
	for _, a := range addresses {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			return err
		}
		addrs = append(addrs, ip.WithZone(""))
	}
	if s.net == nil {
		return errors.New("the machines' network is not up")
	}
	return s.net.SetMacAddresses(addrs)
}
