package standin

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"fev/host/daemon/boottrace"
	"fev/host/docker/fakeguest"
	api "fev/proto/agent"
	"fev/proto/fsevents"
	"fev/proto/version"
	"fev/proto/vmmctl"
)

const Arg = "standin-vmm"

const Usage = `stand-in VMM instructions (KEY=VALUE):
  vmm-ms=N           time from start to guest entry (default 2)
  kernel-ms=N        time from guest entry to init's main (default 5)
  init-ms=N          time init takes to be ready (default 2)
  engine-ms=N        time the engine takes to be ready (default 20)
  engine-error=TEXT  the engine fails with this instead of becoming ready
  engine=flap        the engine is never ready: dockerd dies of a bus error engine-ms after each start
                     and init starts it again, without end (as after a mount from an older tree root,
                     with each run slow enough that init does not give up)
  stop-ms=N          time a shutdown takes (default 5)
  vmm-build=ID       build identifier on the first trace line
  agent-build=ID     build identifier the agent reports
  kernel=RELEASE     the kernel release the agent reports (default 7.2.0-fev-standin)
  exit-early=CODE    exit with CODE before the first trace line
  fail-file=PATH     while PATH exists, fail right after starting, status 1
  exit-after-ms=N    exit N ms after init is ready, as a guest fault would
  exit-code=CODE     the status of that exit (default 1)
  no-agent=1         never connect to host port 1025
  drop-agent-ms=N    close the agent's connections N ms after init is ready
  ignore-shutdown=1  answer system.shutdown and keep running
  ignore-lifeline=1  keep running when the lifeline closes
  chatty=N           print N extra console lines and log N init lines at boot
  feed=STATE         the change feed's state with a share: connected (default), none, lost, mismatch
  no-device=1        the guest kernel has no change device
  denied=PATH        once init is ready, say the Mac refused the share this folder (repeatable with denied2=, denied3=)
  feed-lost-ms=N     N ms after init is ready the change feed is lost, and the guest says so
  granted-ms=N       N ms after init is ready the folders said to be denied are opened after all
  sync-ms=N          time a storage.sync takes (default 0)
  cache-mib=N        the guest starts with N MiB of file cache that nothing running owns, which
                     memory.reclaim gives back down to its floor (default 0)
  stats-first-ms=N   a stats stream's first sample comes N ms after its reply (default 0: at once)
  mount-rung=N       storage.info says the data disk mounted on rung N of the ladder (2 to 4 a recovery, 5 read-only)
  repaired=N         the kernel logs N metadata blocks of the data disk repaired from their second copy, as it mounts
  check-uncorrectable=N  the check the guest runs by itself after a mount on rungs 2 to 4 finds N sectors no copy repairs
  check-ms=N         that check runs N ms from the guest's entry (default 0: done at once), and storage.check is answered while it runs
  mount-file=PATH    mount-rung, repaired and check-uncorrectable apply only to a boot that finds PATH
  no-data=1          there is no data filesystem: every storage call fails as in a guest whose disk was refused
  emulation=none     the system image has no x86 translator (emulation.status says so); fev.x86=0 on the
                     command line turns the x86 entries off for the boot, as in a guest
  emulation-shadow-file=PATH  while PATH exists, the table also has another tool's x86-64 entry,
                     qemu-x86_64, registered after fev's, and emulation.status names it as the guest does
  pause-ms=N         time a pause takes on the control socket (default 0)
  pause-never=1      never answer pause
  resume-ms=N        time a resume takes on the control socket (default 0)
  pause-error=TEXT   pause fails with this
  resume-never=1     never answer resume
  hang-file=PATH     while PATH exists, answer nothing on the control socket: a VMM that has stopped running
  clock-behind-ms=N  the guest's wall clock starts this far behind the Mac's
  images=none        the guest keeps no image build (by default it keeps fev's default release
                     of each distribution, both variants, so a create needs no image server)
  create-ms=N        time a machine's first boot takes in machine.create (default 0)
  create-fail=TEXT   every machine.create fails with this at the first boot
  starting-ms=N      a new machine says it is starting this long before it runs, its create
                     not yet ended (default 0)
  k8s=on             the Kubernetes cluster is wanted from the boot (as fev.k8s=1 says); its fake
                     cluster (k8scluster.go) is served at <vsock-dir>/k8s.sock (host/k8s's wire)
  k8s-cached=1       the cluster's archive is cached (no fetch at the first k8s.start)
  k8s-offline=1      a first k8s.start fails as offline, nothing changed
  k8s-short=1        a first k8s.start fails short of the data budget, nothing changed
  k8s-start-ms=N     time a k8s.start takes after its checks, and a boot's start with the
                     cluster wanted (default 0)
  k8s-fetch-ms=N     time each step of a first k8s.start's download takes (default 0)
  k8s-error=TEXT     the cluster fails with this once started
  k8s-kubeconfig-ms=N  k3s's kubeconfig written N ms after its start; as init, the cluster is
                     starting until then and running with it
  k8s-count-asks=1   each k8s.status asked is counted in <vsock-dir>/standin-k8s-asks

Its Linux machines (machine.*) are a table kept in the socket directory, as
a guest keeps them on its data disk; a machine runs nothing. Exec sessions,
in a machine or elsewhere, run a fake process from a fixed table (true,
false, exit N, echo, seq N, bytes N, stderr, cat, sleep MS, size, whoami,
pwd, env; anything else is 127), never a program of the Mac's.

It takes --share as fevvm does and shares nothing: the fake agent
reports the share as mounted, answers fileshare.watch from the Mac's own
view of the paths, and keeps the engine configuration it is given in the
socket directory, as a guest keeps it on its data disk. It takes
--control-fd as fevvm does (fev/proto/vmmctl): while paused its agent
answers nothing, its engine takes no connection and its guest clock stands
still, and after the resume its wall clock is behind by the pause until
time.set.`

type options struct {
	traceFD, lifelineFD int
	controlFD           int
	vsockDir            string
	ports               []uint32
	cpus, mem           int
	set                 map[string]string
	shares              []string
	cmdline             string
}

func (o *options) ms(key string, def int) time.Duration {
	if v, err := strconv.Atoi(o.set[key]); err == nil && v >= 0 {
		return time.Duration(v) * time.Millisecond
	}
	return time.Duration(def) * time.Millisecond
}

func parse(args []string) (*options, error) {
	o := &options{traceFD: -1, lifelineFD: -1, controlFD: -1, set: map[string]string{}}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		switch flag {
		case "--stamp", "--no-reboot", "--verbose":
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("%s needs a value", flag)
		}
		i++
		value := args[i]
		var err error
		switch flag {
		case "--kernel", "--initrd", "--disk", "--disk-sync", "--net", "--console":
		case "--cmdline":
			o.cmdline = value
		case "--share":
			o.shares = append(o.shares, value)
		case "--cpus":
			o.cpus, err = strconv.Atoi(value)
		case "--mem":
			o.mem, err = strconv.Atoi(value)
		case "--vsock-dir":
			o.vsockDir = value
		case "--vsock-port":
			var port uint64
			port, err = strconv.ParseUint(value, 10, 32)
			o.ports = append(o.ports, uint32(port))
		case "--trace-fd":
			o.traceFD, err = strconv.Atoi(value)
		case "--lifeline-fd":
			o.lifelineFD, err = strconv.Atoi(value)
		case vmmctl.Flag:
			o.controlFD, err = strconv.Atoi(value)
		case "--standin":
			k, v, ok := strings.Cut(value, "=")
			if !ok {
				return nil, fmt.Errorf("--standin %q: want KEY=VALUE", value)
			}
			o.set[k] = v
		default:
			return nil, fmt.Errorf("unknown flag %s", flag)
		}
		if err != nil {
			return nil, fmt.Errorf("%s %q: %v", flag, value, err)
		}
	}
	return o, nil
}

type standin struct {
	o     *options
	start time.Time

	ctl   *control
	agent *Agent

	mu    sync.Mutex
	trace *os.File
	conns []net.Conn
}

func (s *standin) mark(name string, extra map[string]string) {
	if s.trace == nil {
		return
	}
	line := map[string]any{"mark": name, "ns": boottrace.Now()}
	for k, v := range extra {
		line[k] = v
	}
	data, _ := json.Marshal(line)
	s.mu.Lock()
	s.trace.Write(append(data, '\n'))
	s.mu.Unlock()
}

func (s *standin) console(text string) {
	fmt.Fprintf(os.Stdout, "[%9.1fms] %s\n", float64(time.Since(s.start).Microseconds())/1000, text)
}

func (s *standin) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fevvm: stand-in: "+format+"\n", args...)
}

func (s *standin) exit(outcome string, code int) {
	s.mark("vmm.exit", map[string]string{"outcome": outcome})
	os.Exit(code)
}

func Main(args []string) int {
	s := &standin{start: time.Now()}
	s.ctl = &control{s: s}
	o, err := parse(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fevvm: stand-in: %v\n%s\n", err, Usage)
		return 2
	}
	s.o = o
	if code, err := strconv.Atoi(o.set["exit-early"]); err == nil {
		s.logf("told to exit with status %d before the first trace line", code)
		return code
	}
	if o.traceFD >= 3 {
		s.trace = os.NewFile(uintptr(o.traceFD), "trace")
	}
	build := version.Build
	if b, ok := o.set["vmm-build"]; ok {
		build = b
	}
	s.mark("vmm.main", map[string]string{"build": build})
	s.logf("%d vCPUs, %d MiB, no guest: every answer comes from a fake agent", o.cpus, o.mem)
	if o.cmdline != "" {
		s.logf("kernel command line: %s", o.cmdline)
	}
	if path := o.set["fail-file"]; path != "" {
		if _, err := os.Stat(path); err == nil {
			s.logf("%s exists: failing as told", path)
			s.exit("fault", 1)
		}
	}

	if o.lifelineFD >= 3 {
		lifeline := os.NewFile(uintptr(o.lifelineFD), "lifeline")
		go func() {
			lifeline.Read(make([]byte, 1))
			if o.set["ignore-lifeline"] != "" {
				s.logf("the lifeline closed; ignoring it as told")
				return
			}
			s.logf("the lifeline closed; stopping")
			s.exit("stopped", 0)
		}()
	}

	time.Sleep(o.ms("vmm-ms", 2))
	for _, name := range []string{"vmm.memory_mapped", "vmm.devices_ready", "vmm.kernel_loaded", "vmm.guest_entry"} {
		s.mark(name, nil)
	}
	entry := time.Now()
	clock := func() float64 { return float64((time.Since(entry) - s.ctl.parked()).Microseconds()) / 1000 }

	agentBuild := version.Build
	if b, ok := o.set["agent-build"]; ok {
		agentBuild = b
	}
	agent := NewAgent(agentBuild, clock)
	if k, ok := o.set["kernel"]; ok {
		agent.Kernel = k
	}
	agent.Gate = s.ctl.wait
	agent.FallBehind(o.ms("clock-behind-ms", 0))
	agent.SetEmulation(o.cmdline, o.set["emulation"] == "none")
	agent.SetEmulationShadow(o.set["emulation-shadow-file"])
	s.agent = agent
	if o.controlFD >= 3 {
		s.ctl.serve(os.NewFile(uintptr(o.controlFD), "control"))
	}
	if v, ok := o.set["budget-fail"]; ok {
		fmt.Sscanf(v, "%d-%d", &agent.BudgetFail[0], &agent.BudgetFail[1])
	}
	damaged := true
	if path := o.set["mount-file"]; path != "" {
		_, err := os.Stat(path)
		damaged = err == nil
	}
	if damaged {
		agent.MountRung, _ = strconv.Atoi(o.set["mount-rung"])
		agent.CheckUncorrectable, _ = strconv.ParseUint(o.set["check-uncorrectable"], 10, 64)
	}
	agent.CheckTime = o.ms("check-ms", 0)
	if o.set["no-data"] != "" {
		agent.NoData = "/dev/vda holds XFS, not a fev data filesystem; it was not touched"
	}
	agent.SyncDelay = o.ms("sync-ms", 0)
	if mib, err := strconv.ParseUint(o.set["cache-mib"], 10, 32); err == nil {
		agent.LeaveCache(mib << 20)
	}
	agent.StatsFirst = o.ms("stats-first-ms", 0)
	agent.EngineDelay = o.ms("engine-ms", 20)
	agent.EngineError = o.set["engine-error"]
	agent.EngineFlap = o.set["engine"] == "flap"
	agent.EngineOff = slices.Contains(strings.Fields(o.cmdline), api.EngineCmdlineParam+"=0")
	if agent.NoData != "" && agent.EngineError == "" {
		agent.EngineError = "the data disk cannot be used: " + agent.NoData
	}
	for _, spec := range o.shares {
		if share, ok := parseShare(spec); ok {
			agent.Share = share
			agent.Feed = o.set["feed"]
			agent.NoDevice = o.set["no-device"] != ""
		}
	}
	if o.vsockDir != "" {
		agent.ConfigFile = filepath.Join(o.vsockDir, "standin-engine.json")
	}
	mo := MachinesOptions{CreateTime: o.ms("create-ms", 0), CreateFail: o.set["create-fail"], StartingTime: o.ms("starting-ms", 0), NoBuilds: o.set["images"] == "none"}
	if o.vsockDir != "" {
		mo.File = filepath.Join(o.vsockDir, "standin-machines.json")
	}
	agent.SetMachines(mo)
	if err := agent.SetK8s(k8sOptions(o.set, o.cmdline, o.vsockDir)); err != nil {
		s.logf("the fake cluster's socket: %v", err)
		return 1
	}
	agent.Console = func(text string) { s.console("fev-init: " + text) }
	agent.OnShutdown = func(reboot bool, _ time.Duration) {
		if o.set["ignore-shutdown"] != "" {
			return
		}
		time.Sleep(o.ms("stop-ms", 5))
		if reboot {
			s.ctl.reset()
			s.exit("reset", 0)
		}
		s.exit("power_off", 0)
	}

	time.Sleep(o.ms("kernel-ms", 5))
	s.mark("guest.console_first", nil)
	time.Sleep(time.Millisecond)
	initStart := clock()
	chatty, _ := strconv.Atoi(o.set["chatty"])
	repaired, _ := strconv.Atoi(o.set["repaired"])
	for i := 0; damaged && i < repaired; i++ {
		agent.Logf(api.LogKernel, 6, "%s", RepairLine)
	}
	for i := 0; i < chatty; i++ {
		s.console(fmt.Sprintf("chatty console line %d", i))
		agent.Logf(api.LogInit, 7, "init: chatty line %d", i)
	}

	guest := fakeguest.New()
	guest.Engine.HostStream = agent.HostStream
	agent.Containers = func() []StatsName {
		ids, names := guest.Engine.Running()
		out := make([]StatsName, len(ids))
		for i := range ids {
			out[i] = StatsName{ID: ids[i], Name: names[i]}
		}
		return out
	}
	agent.Volumes = guest.Engine
	guest.Relay.Stream = agent.AttachStream
	guest.Relay.Exec = agent.AttachExec
	guest.Relay.MTCP = agent.DialMachine
	if o.vsockDir != "" {
		for _, port := range o.ports {
			l, err := net.Listen("unix", filepath.Join(o.vsockDir, fmt.Sprintf("vsock_%d.sock", port)))
			if err != nil {
				s.logf("%v", err)
				return 1
			}
			var gl net.Listener = gated{l, s.ctl.wait}
			if port == fakeguest.PortRelay {
				gl = &viewRelay{Listener: gl, agent: agent, nfs: o.set["view-nfs"], wait: s.ctl.wait}
			}
			if guest.Take(port, gl) {
				continue
			}
			go func(port uint32) {
				for {
					conn, err := l.Accept()
					if err != nil {
						return
					}
					if port == api.Port {
						agent.ServeConn(conn)
					} else {
						conn.Close()
					}
				}
			}(port)
		}
	}
	if o.set["no-agent"] == "" && o.vsockDir != "" {
		conn, err := net.Dial("unix", filepath.Join(o.vsockDir, fmt.Sprintf("host_%d.sock", api.HostPort)))
		if err != nil {
			agent.Logf(api.LogInit, 7, "agent: nothing on host port %d: %v", api.HostPort, err)
		} else {
			s.mu.Lock()
			s.conns = append(s.conns, conn)
			s.mu.Unlock()
			agent.SetHost(agent.ServeConn(conn))
		}
	}
	time.Sleep(o.ms("init-ms", 2))
	agent.Boot(initStart)
	for _, key := range []string{"denied", "denied2", "denied3"} {
		if folder := o.set[key]; folder != "" {
			s.mark(fsevents.MarkDenied, map[string]string{"path": folder})
		}
	}

	if _, ok := o.set["feed-lost-ms"]; ok {
		time.AfterFunc(o.ms("feed-lost-ms", 0), func() { agent.SetFeed(api.FeedLost) })
	}
	if _, ok := o.set["granted-ms"]; ok {
		time.AfterFunc(o.ms("granted-ms", 0), func() {
			for _, key := range []string{"denied", "denied2", "denied3"} {
				if folder := o.set[key]; folder != "" {
					s.mark(fsevents.MarkGranted, map[string]string{"path": folder})
				}
			}
		})
	}

	if _, ok := o.set["space-ms"]; ok {
		num := func(key string, def uint64) uint64 {
			if v, err := strconv.ParseUint(o.set[key], 10, 64); err == nil {
				return v
			}
			return def
		}
		time.AfterFunc(o.ms("space-ms", 0), func() {
			s.ctl.space(num("space-available", 1<<30), num("space-image", 4<<30), num("space-held", 0))
		})
	}

	if v, ok := o.set["drop-agent-ms"]; ok {
		n, _ := strconv.Atoi(v)
		time.AfterFunc(time.Duration(n)*time.Millisecond, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, c := range s.conns {
				c.Close()
			}
		})
	}
	if v, ok := o.set["exit-after-ms"]; ok {
		n, _ := strconv.Atoi(v)
		code := 1
		if c, err := strconv.Atoi(o.set["exit-code"]); err == nil {
			code = c
		}
		time.Sleep(time.Duration(n) * time.Millisecond)
		s.logf("exiting with status %d as told", code)
		if code == 0 {
			s.console("reboot: Power down")
			s.exit("power_off", 0)
		}
		s.console("Kernel panic - not syncing: the stand-in was told to fail")
		s.exit("fault", code)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	<-sig
	return 0
}

type gated struct {
	net.Listener
	wait func()
}

func (g gated) Accept() (net.Conn, error) {
	conn, err := g.Listener.Accept()
	if err == nil {
		g.wait()
	}
	return conn, err
}

func parseShare(spec string) (*Share, bool) {
	share := &Share{}
	tag := ""
	for _, part := range strings.Split(spec, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "tag":
			tag = v
		case "path":
			share.Root = v
		case "deny":
			share.Deny = append(share.Deny, v)
		}
	}
	return share, tag == "mac" && share.Root != ""
}
