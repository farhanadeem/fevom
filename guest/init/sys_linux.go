package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/agent"
	"fev/guest/internal/binfmt"
	"fev/guest/internal/hostlo"
	"fev/guest/internal/logbuf"
	"fev/guest/internal/netcfg"
	"fev/guest/internal/reaper"
	"fev/guest/internal/session"
	"fev/guest/internal/stage"
	"fev/guest/internal/stats"
	"fev/guest/internal/streams"
	"fev/guest/internal/thp"
	"fev/guest/machines"
	machnet "fev/guest/machines/net"
	"fev/guest/nfsview"
	"fev/guest/storage"
	"fev/proto/fsevents"
	"fev/proto/netplan"
	"fev/proto/version"
)

type system struct {
	cmdline string
	log     *logbuf.Buffer
	plan    netplan.Plan
	planErr error
	plan6   netplan.Plan6
	ok6     bool

	agentMu  sync.Mutex
	agentMac string
	agentSet bool

	mu     sync.Mutex
	trace  api.BootTrace
	mounts []api.Mount

	ready chan struct{}
	down  atomic.Bool

	rcu     *rcuHolds
	rcuBoot func()

	reaper                *reaper.Reaper
	vol                   *storage.Volume
	volErr                error
	volDone               chan struct{}
	dataDevice, dataTools string
	lastBudget            int64
	budgetSet             bool
	dirtyBytes            int64

	k8s      *k8sGuest
	engine   *engine
	sessions *session.Manager
	debug    *session.Debug
	debugSrv *debugServer
	machines *machines.Runtime
	view     *nfsview.View
	viewFeed atomic.Pointer[viewFeed]
	net      *machnet.Network
	direct   *direct
	hostlo   *hostlo.Programs
	engPorts *enginePorts
	web      *webFront
	emu      *emulation

	share    *fileShare
	statsMu  sync.Mutex
	stats    *stats.Reader
	requests *streams.Requests
	host     atomic.Pointer[agent.HostLink]
	server   atomic.Pointer[agent.Server]
}

func monotonicMS() float64 {
	var ts unix.Timespec
	unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return float64(ts.Sec)*1000 + float64(ts.Nsec)/1e6
}

func readCmdline() string {
	os.MkdirAll("/proc", 0o555)
	unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")
	data, _ := os.ReadFile("/proc/cmdline")
	return strings.TrimSpace(string(data))
}

func mount(source, target, fstype string, flags uintptr, data string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	err := unix.Mount(source, target, fstype, flags, data)
	if err != nil && !errors.Is(err, unix.EBUSY) {
		return fmt.Errorf("mount %s on %s: %w", fstype, target, err)
	}
	return nil
}

func (s *system) mountPseudo() error {
	const base = unix.MS_NOSUID | unix.MS_NOEXEC
	steps := []struct {
		source, target, fstype string
		flags                  uintptr
		data                   string
	}{
		{"proc", "/proc", "proc", base | unix.MS_NODEV, ""},
		{"sysfs", "/sys", "sysfs", base | unix.MS_NODEV, ""},
		{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755"},
		{"devpts", "/dev/pts", "devpts", base, "gid=5,mode=0620,ptmxmode=0666"},
		{"tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755"},
		{"tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
	}
	for _, m := range steps {
		if err := mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil {
			return err
		}
	}
	if err := os.MkdirAll("/etc", 0o755); err != nil {
		return err
	}
	for _, key := range []string{"fs/protected_hardlinks", "fs/protected_symlinks"} {
		if err := os.WriteFile("/proc/sys/"+key, []byte("1"), 0); err != nil {
			s.log.Logf(logbuf.Warning, "init: cannot set %s: %v", key, err)
		}
	}
	for _, v := range idleSysctls {
		if err := os.WriteFile("/proc/sys/"+v.key, []byte(v.value), 0); err != nil {
			s.log.Logf(logbuf.Warning, "init: cannot set %s: %v", v.key, err)
		}
	}
	if err := thp.SmallFolios(); err != nil {
		s.log.Logf(logbuf.Warning, "init: workloads' 16 KiB folios stay off: %v", err)
	}
	s.capDirtyData()
	s.rcuBoot = s.rcu.hold()
	return unix.Sethostname([]byte("fev"))
}

var idleSysctls = []struct{ key, value string }{
	{"vm/compaction_proactiveness", "0"},
	{"vm/stat_interval", "30"},
}

type rcuHolds struct {
	mu   sync.Mutex
	n    int
	logf func(format string, args ...any)
	path string
}

const rcuExpedited = "/sys/kernel/rcu_expedited"

func (r *rcuHolds) hold() (release func()) {
	r.mu.Lock()
	r.n++
	if r.n == 1 {
		r.write("1")
	}
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			r.n--
			if r.n == 0 {
				r.write("0")
			}
			r.mu.Unlock()
		})
	}
}

func (r *rcuHolds) write(value string) {
	if err := os.WriteFile(r.path, []byte(value), 0); err != nil {
		r.logf("init: cannot set rcu_expedited=%s: %v", value, err)
	}
}

const cgroupRoot = "/sys/fs/cgroup"

func (s *system) setupCgroups() error {
	if err := mount("cgroup2", cgroupRoot, "cgroup2", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "nsdelegate,memory_recursiveprot"); err != nil {
		return err
	}
	for _, group := range []string{"fev.init", "fev.system", "fev.machines"} {
		if err := os.MkdirAll(filepath.Join(cgroupRoot, group), 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(cgroupRoot, "fev.init", "cgroup.procs"), []byte("1"), 0); err != nil {
		return fmt.Errorf("move init into its cgroup: %w", err)
	}
	available, err := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.controllers"))
	if err != nil {
		return err
	}
	controllers := strings.Fields(string(available))
	control := filepath.Join(cgroupRoot, "cgroup.subtree_control")
	if os.WriteFile(control, []byte("+"+strings.Join(controllers, " +")), 0) == nil {
		return nil
	}
	var failed []string
	for _, controller := range controllers {
		if err := os.WriteFile(control, []byte("+"+controller), 0); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", controller, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not delegate controllers: %s", strings.Join(failed, ", "))
	}
	return nil
}

const (
	dataDiskParam   = "fev.data"
	dataCowParam    = "fev.data_cow"
	systemDiskParam = "fev.system"
	systemMount     = "/system"
)

func (s *system) mountSystemImage() error {
	device := netcfg.Arg(s.cmdline, systemDiskParam)
	named := device != ""
	if !named {
		device = "/dev/vdb"
		if p, err := storage.ProbePath(device); err != nil || p.What != "erofs" {
			return nil
		}
	}
	if err := mount(device, systemMount, "erofs", unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
		return fmt.Errorf("system image %s: %w", device, errors.Unwrap(err))
	}
	s.mu.Lock()
	s.mounts = append(s.mounts, api.Mount{Source: device, Target: systemMount, FSType: "erofs"})
	s.mu.Unlock()
	return nil
}

func (s *system) mountDataDisk() error {
	defer close(s.volDone)
	go s.startSwap()
	if err := s.mountSystemImage(); err != nil {
		s.log.Logf(logbuf.Error, "init: %v", err)
	}
	device := netcfg.Arg(s.cmdline, dataDiskParam)
	if device == "" {
		device = "/dev/vda"
		if _, err := os.Stat(device); err != nil {
			return s.noDataDisk(stage.Skip("no data disk attached"))
		}
		if p, err := storage.ProbePath(device); err == nil && p.What == "erofs" {
			return s.noDataDisk(stage.Skip("no data disk attached"))
		}
	}
	tools := ""
	if _, err := os.Stat(systemMount + "/bin"); err == nil {
		tools = systemMount + "/bin"
	}
	s.mu.Lock()
	s.dataDevice, s.dataTools = device, tools
	s.mu.Unlock()
	vol, err := storage.Open(storage.Config{
		Device: device, Target: "/data", ToolDir: tools,
		Run:       s.reaper.Run,
		Logf:      func(format string, args ...any) { s.log.Logf(logbuf.Info, format, args...) },
		NowMS:     monotonicMS,
		Build:     version.Build,
		DataCow:   netcfg.Arg(s.cmdline, dataCowParam) == "1",
		Allocated: storage.ParseAllocated(netcfg.Arg(s.cmdline, storage.AllocatedParam)),
		Step:      migrationStep(s.cmdline),
	})
	if err != nil {
		return s.noDataDisk(err)
	}
	s.mu.Lock()
	s.vol = vol
	s.mounts = append(s.mounts, api.Mount{Source: device, Target: "/data", FSType: vol.FSType()})
	s.mu.Unlock()
	return nil
}

func (s *system) noDataDisk(why error) error {
	s.mu.Lock()
	s.volErr = why
	s.mu.Unlock()
	return why
}

func (s *system) volume() (*storage.Volume, error) {
	select {
	case <-s.volDone:
	case <-s.ready:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.vol != nil:
		return s.vol, nil
	case s.volErr != nil:
		return nil, fmt.Errorf(api.NoDataFilesystem+": %w", s.volErr)
	}
	return nil, errors.New(api.NoDataFilesystem + ": the data-disk stage did not run")
}

func (s *system) StorageInfo() (*api.StorageInfo, error) {
	vol, err := s.volume()
	if err != nil {
		return nil, err
	}
	return vol.Info()
}

func (s *system) StorageTrim(minLen uint64) (*api.TrimResult, error) {
	vol, err := s.volume()
	if err != nil {
		return nil, err
	}
	r, err := vol.Trim(minLen)
	if err == nil {
		s.log.Logf(logbuf.Debug, "storage: trim discarded %d bytes of free space in %.1f ms", r.TrimmedBytes, r.DurationMS)
	}
	return r, err
}

func (s *system) StorageBudget(p api.BudgetParams) (*api.Budget, error) {
	vol, err := s.volume()
	if err != nil {
		return nil, err
	}
	r, err := vol.SetBudget(p)
	if err != nil {
		return nil, err
	}
	if r.Deferred {
		s.log.Logf(logbuf.Info, "storage: no budget set: the image holds %d bytes more than the filesystem uses and has reserved, to be trimmed first (image %d, used %d, reserved %d)",
			api.ReclaimableBytes(p.ImageBytes, r.UsedBytes, r.ReservedBytes), p.ImageBytes, r.UsedBytes, r.ReservedBytes)
		return r, nil
	}
	s.mu.Lock()
	moved := r.BudgetBytes - s.lastBudget
	first := !s.budgetSet
	if first || moved >= 64<<20 || moved <= -(64<<20) || !r.Limited {
		s.lastBudget, s.budgetSet = r.BudgetBytes, true
	}
	s.mu.Unlock()
	level := logbuf.Debug
	if first || moved >= 64<<20 || moved <= -(64<<20) {
		level = logbuf.Info
	}
	if !r.Limited {
		s.log.Logf(level, "storage: the quota group's limit was removed (referenced %d, used %d)", r.ReferencedBytes, r.UsedBytes)
	} else {
		s.log.Logf(level, "storage: budget %d bytes: limit %d = referenced %d + budget (Mac available %d, margin %d, image %d, used %d, reserved %d) in %.2f ms",
			r.BudgetBytes, r.LimitBytes, r.ReferencedBytes, p.AvailableBytes, p.MarginBytes, p.ImageBytes, r.UsedBytes, r.ReservedBytes, r.DurationMS)
	}
	return r, nil
}

func (s *system) StorageSync() error {
	if _, err := s.volume(); err != nil {
		return err
	}
	fd, err := unix.Open("/data", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.Syncfs(fd)
}

func (s *system) StatsSample() (api.StatsSample, error) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if s.stats == nil {
		s.stats = &stats.Reader{
			Proc: "/proc", Sys: "/sys", Root: cgroupRoot, Engine: engineCgroup,
			Own: engineTree.Own, Containers: engineTree.Containers, Data: engineData, Now: monotonicMS,
			Machines: func() []stats.Machine {
				if s.machines == nil {
					return nil
				}
				var out []stats.Machine
				for _, m := range s.machines.Running() {
					out = append(out, stats.Machine{Name: m.Name, UUID: m.UUID, Group: m.Group, PID: m.PID})
				}
				return out
			},
		}
	}
	return s.stats.Sample()
}

func (s *system) networkBase() error {
	for _, key := range []string{"net/ipv4/ip_forward", "net/ipv6/conf/all/forwarding"} {
		if err := os.WriteFile("/proc/sys/"+key, []byte("1"), 0); err != nil {
			return fmt.Errorf("enable %s: %w", key, err)
		}
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_local_port_range", []byte(localPortRange), 0); err != nil {
		return fmt.Errorf("set the ephemeral port range: %w", err)
	}
	return nil
}

const localPortRange = "32768 49151"

func (s *system) configureNetwork(cfg netcfg.Config) error {
	if lo, err := netlink.LinkByName("lo"); err == nil {
		if err := netlink.LinkSetUp(lo); err != nil {
			return fmt.Errorf("bring up lo: %w", err)
		}
	}

	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	var uplink netlink.Link
	for _, l := range links {
		if l.Type() == "device" && l.Attrs().EncapType == "ether" {
			uplink = l
			break
		}
	}
	if uplink == nil {
		return stage.Skip("no network device attached")
	}

	addr, err := netlink.ParseAddr(cfg.Address.String())
	if err != nil {
		return err
	}
	if err := netlink.AddrReplace(uplink, addr); err != nil {
		return fmt.Errorf("set %s on %s: %w", cfg.Address, uplink.Attrs().Name, err)
	}
	if s.ok6 {
		if err := ipv6Quiet(uplink.Attrs().Name); err != nil {
			return err
		}
	}
	if err := netlink.LinkSetUp(uplink); err != nil {
		return fmt.Errorf("bring up %s: %w", uplink.Attrs().Name, err)
	}
	route := &netlink.Route{LinkIndex: uplink.Attrs().Index, Gw: net.IP(cfg.Gateway.AsSlice())}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("default route via %s: %w", cfg.Gateway, err)
	}
	if s.ok6 {
		if err := s.uplink6(uplink); err != nil {
			return err
		}
	}
	if err := s.direct.uplinkUp(uplink.Attrs().Name); err != nil {
		return err
	}
	resolv := fmt.Sprintf("# written by fev-init\nnameserver %s\n", cfg.DNS)
	return os.WriteFile("/etc/resolv.conf", []byte(resolv), 0o644)
}

func ipv6Quiet(dev string) error {
	for _, kv := range [][2]string{{"accept_ra", "0"}, {"router_solicitations", "0"}, {"dad_transmits", "0"}} {
		if err := os.WriteFile("/proc/sys/net/ipv6/conf/"+dev+"/"+kv[0], []byte(kv[1]), 0); err != nil {
			return fmt.Errorf("set %s's %s: %w", dev, kv[0], err)
		}
	}
	return nil
}

func (s *system) uplink6(uplink netlink.Link) error {
	g := s.plan6.Guest6()
	a := &netlink.Addr{IPNet: &net.IPNet{IP: g.Addr().AsSlice(), Mask: net.CIDRMask(g.Bits(), 128)}, Flags: unix.IFA_F_NODAD}
	if err := netlink.AddrReplace(uplink, a); err != nil {
		return fmt.Errorf("set %s on %s: %w", g, uplink.Attrs().Name, err)
	}
	gw := s.plan6.UplinkGateway6()
	route := &netlink.Route{LinkIndex: uplink.Attrs().Index, Gw: net.IP(gw.AsSlice()),
		Dst: &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("IPv6 default route via %s: %w", gw, err)
	}
	return nil
}

func (s *system) Kernel() string {
	var u unix.Utsname
	unix.Uname(&u)
	return unix.ByteSliceToString(u.Release[:])
}

func (s *system) Info() (*api.Info, error) {
	info := &api.Info{
		Kernel:     s.Kernel(),
		UptimeMS:   int64(monotonicMS()),
		CPUs:       runtime.NumCPU(),
		Interfaces: []api.Interface{},
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	for sc := bufio.NewScanner(f); sc.Scan(); {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			info.MemTotalKB = kb
		case "MemAvailable:":
			info.MemAvailableKB = kb
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, i := range ifaces {
		entry := api.Interface{Name: i.Name, Up: i.Flags&net.FlagUp != 0, Addresses: []string{}}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			entry.Addresses = append(entry.Addresses, a.String())
		}
		info.Interfaces = append(info.Interfaces, entry)
	}
	s.mu.Lock()
	info.Mounts = append([]api.Mount{}, s.mounts...)
	s.mu.Unlock()
	info.DirtyBytes = s.dirtyBytes
	if info.FileShare = s.share.info(); info.FileShare != nil {
		info.Mounts = append(info.Mounts, api.Mount{Source: shareTag, Target: shareMount, FSType: "virtiofs"})
	}
	delegated, _ := os.ReadFile(filepath.Join(cgroupRoot, "fev.system", "cgroup.controllers"))
	info.CgroupControllers = append([]string{}, strings.Fields(string(delegated))...)
	return info, nil
}

func (s *system) WallClock() (int64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_REALTIME, &ts); err != nil {
		return 0, err
	}
	return ts.Nano(), nil
}

func (s *system) SetWallClock(unixNS int64) error {
	ts := unix.NsecToTimespec(unixNS)
	return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
}

func (s *system) setTrace(t api.BootTrace) {
	s.mu.Lock()
	s.trace = t
	s.mu.Unlock()
	close(s.ready)
}

func (s *system) Ready() <-chan struct{} { return s.ready }

func (s *system) BootTrace() api.BootTrace {
	s.mu.Lock()
	t := s.trace
	s.mu.Unlock()
	if t.Stages == nil {
		t.Stages = []api.Stage{}
	}
	if status := s.engine.sup.Status(); status.Starts > 0 {
		t.EngineStartMS, t.EngineReadyMS = status.StartedMS, status.ReadyMS
	}
	t.NowMS = monotonicMS()
	return t
}

func (s *system) started() error {
	select {
	case <-s.ready:
		return nil
	default:
		return errors.New("the guest is still starting")
	}
}

func (s *system) EngineStatus() (api.EngineStatus, error) { return s.engine.sup.Status(), nil }

func (s *system) EngineStart() (api.EngineStatus, error) {
	if err := s.started(); err != nil {
		return s.engine.sup.Status(), err
	}
	return s.engine.sup.Start()
}

func (s *system) EngineStop(grace time.Duration) (api.EngineStatus, error) {
	if err := s.started(); err != nil {
		return s.engine.sup.Status(), err
	}
	return s.engine.sup.Stop(grace)
}

func (s *system) EngineRestart(grace time.Duration) (api.EngineStatus, error) {
	if err := s.started(); err != nil {
		return s.engine.sup.Status(), err
	}
	return s.engine.sup.Restart(grace)
}

func (s *system) EngineSubscribe(emit func(api.EngineStatus)) (func(), error) {
	return s.engine.sup.Subscribe(emit), nil
}

func (s *system) EngineConfigure(config map[string]json.RawMessage) (api.EngineConfigured, error) {
	if err := s.started(); err != nil {
		return api.EngineConfigured{}, err
	}
	return s.engine.configure(config)
}

func (s *system) FileShareWatch(ctx context.Context, paths []string) ([]fsevents.Status, error) {
	return s.share.watch(ctx, paths)
}

func (s *system) StreamRefuse(token, reason string) { s.requests.Refuse(token, reason) }

func (s *system) FileShareBind() (bound, unbound []string, err error) {
	return s.share.bindLate(func(fn func() error) error {
		return s.engine.In(func(int) error { return fn() })
	})
}

func (s *system) FileShareFollow(emit func(api.FileShare)) func() { return s.share.follow(emit) }

func (s *system) askHost(req api.StreamRequest) bool {
	link := s.host.Load()
	return link != nil && link.Notify(api.NotifyStreamRequest, req)
}

func (s *system) ExecStart(p api.ExecStartParams, exited func(api.ExecExit)) (*api.ExecStarted, error) {
	if p.Context == api.ExecEngine && s.engine.sup.Status().State != api.EngineReady {
		return nil, errors.New("the engine is not ready")
	}
	if p.Context == api.ExecMachine {
		i, err := s.machines.Info(p.Machine)
		if err != nil {
			return nil, machineErr(err)
		}
		if i.State != api.MachineRunning {
			return nil, fmt.Errorf("%s is not running (%s)", p.Machine, i.State)
		}
		if err := s.machines.SessionUser(p.Machine, p.User); err != nil {
			return nil, err
		}
	}
	if p.Context == api.ExecDebug {
		if err := s.debug.Prepare(p); err != nil {
			return nil, err
		}
		ds, err := s.debug.Resolve(p)
		if err != nil {
			return nil, err
		}
		started, err := s.sessions.Start(p, exited)
		if err != nil {
			return nil, err
		}
		started.Debug = ds
		return started, nil
	}
	return s.sessions.Start(p, exited)
}

func (s *system) ExecResize(id uint64, rows, cols int) error {
	return s.sessions.Resize(id, rows, cols)
}
func (s *system) ExecSignal(id uint64, signal int) error { return s.sessions.Signal(id, signal) }
func (s *system) ExecClose(id uint64) error              { return s.sessions.Close(id) }

func (s *system) Shutdown(reboot bool, deadline time.Duration, progress func(api.ShutdownProgress)) error {
	if !s.down.CompareAndSwap(false, true) {
		return errors.New("shutdown is already in progress")
	}
	defer s.down.Store(false)
	return s.shutdown(reboot, deadline, progress)
}

const (
	shutdownReserve = 2 * time.Second
	sessionsWait    = time.Second
)

func (s *system) shutdown(reboot bool, deadline time.Duration, progress func(api.ShutdownProgress)) error {
	began := time.Now()
	cmd := unix.LINUX_REBOOT_CMD_POWER_OFF
	if reboot {
		cmd = unix.LINUX_REBOOT_CMD_RESTART
	}
	readOnly := func() {
		s.mu.Lock()
		mounts := append([]api.Mount{}, s.mounts...)
		s.mu.Unlock()
		for i := len(mounts) - 1; i >= 0; i-- {
			unix.Mount("", mounts[i].Target, "", unix.MS_REMOUNT|unix.MS_RDONLY, "")
		}
	}

	hard := time.AfterFunc(deadline, func() {
		s.log.Logf(logbuf.Warning, "init: shutdown did not finish in %v; forcing it", deadline)
		progress(api.ShutdownProgress{Phase: api.ShutdownDeadline})
		readOnly()
		unix.Sync()
		unix.Reboot(cmd)
	})
	defer hard.Stop()
	s.rcu.hold()

	progress(api.ShutdownProgress{Phase: api.ShutdownBegin})
	var problem string
	note := func(format string, args ...any) {
		problem = fmt.Sprintf(format, args...)
		s.log.Logf(logbuf.Warning, "init: shutdown: %s", problem)
	}
	next := func(phase string) {
		progress(api.ShutdownProgress{Phase: phase, Error: problem})
		problem = ""
	}

	next(api.ShutdownEngine)
	machinesDown := make(chan struct{})
	go func() {
		defer close(machinesDown)
		s.machines.Shutdown(max(deadline-shutdownReserve, 0))
	}()
	s.engine.sup.Close(max(deadline-shutdownReserve, 0))
	<-machinesDown

	next(api.ShutdownSessions)
	if left := s.sessions.Shutdown(sessionsWait); left > 0 {
		note("%d sessions did not end", left)
	}

	stallAt(s.cmdline, "shutdown")
	next(api.ShutdownSync)
	unix.Sync()

	next(api.ShutdownUnmount)
	if err := s.share.unmount(); err != nil {
		note("%v", err)
	}
	s.mu.Lock()
	mounts := append([]api.Mount{}, s.mounts...)
	vol := s.vol
	s.mu.Unlock()
	if vol != nil {
		vol.Close()
	}
	if mi, err := os.ReadFile("/proc/self/mountinfo"); err != nil {
		note("read the mount table: %v", err)
	} else {
		for _, at := range mountsHolding(string(mi), mounts) {
			if err := unix.Unmount(at, 0); err != nil && !errors.Is(err, unix.EINVAL) {
				note("unmount %s: %v", at, err)
			}
		}
	}
	if err := unix.Unmount(binfmt.Dir, 0); err != nil && !errors.Is(err, unix.EINVAL) {
		note("unmount %s: %v", binfmt.Dir, err)
	}
	if err := unix.Unmount(api.EmulationRosettaMount, 0); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
		note("unmount %s: %v", api.EmulationRosettaMount, err)
	}
	for i := len(mounts) - 1; i >= 0; i-- {
		if err := unix.Unmount(mounts[i].Target, 0); err != nil && !errors.Is(err, unix.EINVAL) {
			note("unmount %s: %v; remounted read-only", mounts[i].Target, err)
			unix.Mount("", mounts[i].Target, "", unix.MS_REMOUNT|unix.MS_RDONLY, "")
		}
	}
	next(api.ShutdownOff)
	s.log.Logf(logbuf.Info, "init: shutdown took %d ms", time.Since(began).Milliseconds())
	unix.Sync()
	return unix.Reboot(cmd)
}

func (s *system) KernelLog(ctx context.Context, backlog bool, emit func(timeMS float64, level int, text string), caughtUp func()) error {
	fd, err := unix.Open("/dev/kmsg", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	if !backlog {
		if _, err := unix.Seek(fd, 0, unix.SEEK_END); err != nil {
			unix.Close(fd)
			return err
		}
	} else {
		buf := make([]byte, 8192)
		for ctx.Err() == nil {
			n, err := unix.Read(fd, buf)
			if errors.Is(err, unix.EPIPE) || errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				if !errors.Is(err, unix.EAGAIN) {
					unix.Close(fd)
					return err
				}
				break
			}
			if timeMS, level, text, ok := parseKmsg(string(buf[:n])); ok {
				emit(timeMS, level, text)
			}
		}
	}
	caughtUp()
	f := os.NewFile(uintptr(fd), "/dev/kmsg")
	defer f.Close()
	stop := context.AfterFunc(ctx, func() { f.Close() })
	defer stop()

	buf := make([]byte, 8192)
	for {
		n, err := f.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, unix.EPIPE) {
				continue
			}
			return err
		}
		if timeMS, level, text, ok := parseKmsg(string(buf[:n])); ok {
			emit(timeMS, level, text)
		}
	}
}

func mountsHolding(mountinfo string, mounts []api.Mount) []string {
	type entry struct{ dev, at string }
	var all []entry
	for _, line := range strings.Split(mountinfo, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		all = append(all, entry{dev: f[2], at: unescapeMountinfo(f[4])})
	}
	own := map[string]bool{}
	devs := map[string]bool{}
	for _, m := range mounts {
		own[m.Target] = true
	}
	for _, e := range all {
		if own[e.at] {
			devs[e.dev] = true
		}
	}
	var held []string
	for i := len(all) - 1; i >= 0; i-- {
		e := all[i]
		if own[e.at] {
			continue
		}
		below := false
		for _, m := range mounts {
			if strings.HasPrefix(e.at, strings.TrimSuffix(m.Target, "/")+"/") {
				below = true
			}
		}
		if below || devs[e.dev] {
			held = append(held, e.at)
		}
	}
	return held
}

func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
