package main

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	_ "golang.org/x/crypto/x509roots/fallback"

	api "fev/proto/agent"
	"fev/proto/relay"
	"fev/proto/version"

	"fev/guest/agent"
	"fev/guest/internal/logbuf"
	"fev/guest/internal/netcfg"
	"fev/guest/internal/reaper"
	"fev/guest/internal/session"
	"fev/guest/internal/stage"
	"fev/guest/internal/streams"
	"fev/guest/machines"
)

func init() { runtime.LockOSThread() }

func main() {
	if machines.IsStage(os.Args) {
		machines.StageMain()
	}
	if session.IsEntry(os.Args) {
		session.EntryMain()
	}
	if isEngineStage(os.Args) {
		engineStageMain()
	}
	if isContainerdName(os.Args) && os.Getpid() != 1 {
		err := execContainerd(os.Args)
		fmt.Fprintf(os.Stderr, "fev-init: start %s: %v\n", containerdReal, err)
		os.Exit(127)
	}
	if os.Getpid() == 1 {
		hugePagesOff()
	}
	start := monotonicMS()
	sys := &system{
		cmdline: readCmdline(), ready: make(chan struct{}),
		reaper: reaper.New(), volDone: make(chan struct{}),
	}
	log := logbuf.New(512, monotonicMS, os.Stdout)
	sys.log = log
	sys.plan, sys.plan6, sys.ok6, sys.planErr = readPlan(sys.cmdline, log)
	sys.share = newFileShare(log)
	sys.requests = &streams.Requests{Ask: sys.askHost, Logf: func(format string, args ...any) { log.Logf(logbuf.Debug, format, args...) }}
	raiseFileLimit(log)
	sys.rcu = &rcuHolds{path: rcuExpedited, logf: func(format string, args ...any) { log.Logf(logbuf.Warning, format, args...) }}
	sys.engine = newEngine(sys)
	sys.direct = newDirect(sys)
	sys.engPorts = newEnginePorts(log)
	go sys.engPorts.run(func(p api.EnginePorts) {
		if link := sys.host.Load(); link != nil {
			link.Conn.Notify(api.NotifyEnginePorts, p)
		}
	})
	sys.web = newWebFront(sys)
	sys.emu = newEmulation(log, sys.cmdline)
	sys.emu.reaper = sys.reaper
	sys.machines = newMachines(sys)
	sys.view = newView(sys)
	go sys.serveViewControl()
	sys.debug = sys.newDebug()
	sys.sessions = session.NewManager(sys.reaper, map[string]session.Context{
		api.ExecInit:    session.Groups{Dir: initSessions},
		api.ExecEngine:  sys.engine,
		api.ExecMachine: sys.machines.Sessions(),
		api.ExecDebug:   sys.debug,
	}, func(format string, args ...any) { log.Logf(logbuf.Debug, format, args...) })

	defer func() {
		if r := recover(); r != nil {
			log.Logf(logbuf.Error, "init: internal error: %v", r)
		}
		select {}
	}()

	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "fev-init must run as PID 1")
		os.Exit(1)
	}
	go superviseSignals(sys, log)

	results, err := stage.Run(sys.stages(log), monotonicMS)
	if err != nil {
		log.Logf(logbuf.Error, "init: %v", err)
	}
	trace := api.BootTrace{InitStartMS: start, ReadyMS: monotonicMS()}
	for _, r := range results {
		s := api.Stage{Name: r.Name, StartMS: r.StartMS, EndMS: r.EndMS, Skipped: r.Skipped}
		switch {
		case r.NotRun:
			s.Error = "not run: a stage it needs failed"
			if r.Name == "engine" {
				why := "the guest could not prepare what the engine needs; see the boot trace"
				sys.mu.Lock()
				if sys.volErr != nil {
					why = "the data disk cannot be used: " + sys.volErr.Error()
				}
				sys.mu.Unlock()
				sys.engine.sup.Absent(why)
			}
			if r.Name == "machines" {
				why := errors.New("the guest could not prepare what machines need; see the boot trace")
				sys.mu.Lock()
				if sys.volErr != nil {
					why = fmt.Errorf("the data disk cannot be used: %w", sys.volErr)
				}
				sys.mu.Unlock()
				sys.machines.Unavailable(why)
			}
		case r.Err != nil:
			s.Error = r.Err.Error()
		}
		trace.Stages = append(trace.Stages, s)
		switch {
		case s.Error != "":
			log.Logf(logbuf.Error, "init: stage %s failed after %.1f ms: %s", s.Name, s.EndMS-s.StartMS, s.Error)
		case s.Skipped != "":
			log.Logf(logbuf.Info, "init: stage %s skipped: %s", s.Name, s.Skipped)
		default:
			log.Logf(logbuf.Debug, "init: stage %s took %.1f ms", s.Name, s.EndMS-s.StartMS)
		}
	}
	sys.setTrace(trace)
	if sys.rcuBoot != nil {
		sys.rcuBoot()
	}
	log.Logf(logbuf.Info, "init: ready at %.1f ms, %.1f ms after reaching main", trace.ReadyMS, trace.ReadyMS-start)

	select {}
}

func (s *system) stages(log *logbuf.Buffer) []stage.Stage {
	return []stage.Stage{
		{Name: "mounts", Required: true, Run: s.mountPseudo},
		{Name: "agent", After: []string{"mounts"}, Run: func() error {
			l, err := vsock.Listen(api.Port, nil)
			if err != nil {
				return err
			}
			srv := agent.NewServer(s, log, version.Build)
			srv.EngineLog = s.engine.out
			s.server.Store(srv)
			go func() {
				log.Logf(logbuf.Error, "agent: listener stopped: %v", srv.Serve(agent.HostOnly(l, fromHost)))
			}()
			go dialHost(s, srv, log)
			s.startHooks()
			return nil
		}},
		{Name: "cgroups", After: []string{"mounts"}, Required: true, Run: s.setupCgroups},
		{Name: "data-disk", After: []string{"mounts"}, Required: true, Run: s.mountDataDisk},
		{Name: "network", After: []string{"mounts"}, Run: func() error {
			if s.planErr != nil {
				return s.planErr
			}
			cfg, err := netcfg.FromCmdline(s.cmdline)
			if err != nil {
				return err
			}
			return s.configureNetwork(cfg)
		}},
		{Name: "net-base", After: []string{"network"}, Required: true, Run: s.networkBase},
		{Name: "streams", After: []string{"mounts"}, Run: s.serveStreams},
		{Name: "fileshare", After: []string{"mounts"}, Run: s.share.start},
		{Name: "listen", After: []string{"cgroups"}, Run: s.startListen},
		{Name: "hostnet", After: []string{"cgroups", "data-disk", "network"}, Run: s.startHostNet},
		{Name: "emulation", After: []string{"cgroups", "data-disk"}, Run: s.emu.start},
		{Name: "macexec", After: []string{"data-disk", "emulation"}, Run: s.registerMacPrograms},
		{Name: "machines", After: []string{"cgroups", "data-disk", "listen", "emulation", "macexec"}, Run: s.startMachines},
		{Name: "engine", After: []string{"cgroups", "data-disk", "net-base", "fileshare", "hostnet", "emulation"}, Run: s.startEngine},
	}
}

func (s *system) serveStreams() error {
	docker, err := vsock.Listen(api.DockerPort, nil)
	if err != nil {
		return err
	}
	relayed, err := vsock.Listen(relay.Port, nil)
	if err != nil {
		docker.Close()
		return err
	}
	go func() {
		err := streams.ServeSplice(agent.HostOnly(docker, fromHost), func() (net.Conn, error) {
			if s.engine.sup.Status().State != api.EngineReady {
				return nil, errors.New("the engine is not ready")
			}
			return net.Dial("unix", engineSocket)
		})
		s.log.Logf(logbuf.Error, "streams: Docker port stopped: %v", err)
	}()
	go func() {
		r := &streams.Relay{
			Local: func(a netip.Addr) bool {
				return streams.LocalAddress(a) || s.net.MachineAddress(a) || s.cluster().relayTarget(a)
			},
			Exec: s.sessions.Attach,
			Stream: func(token string, conn net.Conn) {
				if srv := s.server.Load(); srv != nil && srv.AttachStream(token, conn) {
					return
				}
				s.requests.Attach(token, conn)
			},
			Machine:  s.machines.Dial,
			View:     s.dialView,
			ViewHops: viewHopsFile(s.cmdline),
			Logf:     func(format string, args ...any) { s.log.Logf(logbuf.Debug, format, args...) },
		}
		r.Max = relayStreams
		var lim unix.Rlimit
		if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err == nil {
			r.Max = min(r.Max, streams.StreamsFor(lim.Cur))
			s.log.Logf(logbuf.Debug, "streams: the relay serves at most %d streams at once (%d descriptors)", r.Max, lim.Cur)
		}
		s.log.Logf(logbuf.Error, "streams: relay stopped: %v", r.Serve(agent.HostOnly(relayed, fromHost)))
	}()
	return nil
}

func (s *system) startEngine() error {
	if err := s.engine.listenNotify(); err != nil {
		s.engine.sup.Absent(err.Error())
		return err
	}
	if err := s.engine.available(); err != nil {
		s.engine.sup.Absent(err.Error())
		return stage.Skip("%v", err)
	}
	if engineOff(s.cmdline) {
		return stage.Skip("the Docker engine is off (%s=0)", api.EngineCmdlineParam)
	}
	_, err := s.engine.sup.Start()
	return err
}

func engineOff(cmdline string) bool { return netcfg.Arg(cmdline, api.EngineCmdlineParam) == "0" }

func engineNodeName(cmdline string) string {
	if n := netcfg.Arg(cmdline, api.EngineNodeNameParam); n != "" && api.CheckNodeName(n) == nil {
		return n
	}
	return api.DefaultNodeName
}

func fromHost(a net.Addr) bool {
	v, ok := a.(*vsock.Addr)
	return ok && v.ContextID == vsock.Host
}

func dialHost(s *system, srv *agent.Server, log *logbuf.Buffer) {
	conn, err := vsock.Dial(vsock.Host, api.HostPort, nil)
	if err != nil {
		log.Logf(logbuf.Debug, "agent: nothing on host port %d: %v", api.HostPort, err)
		return
	}
	log.Logf(logbuf.Debug, "agent: connected to host port %d", api.HostPort)
	link := srv.ServeHost(conn)
	s.host.Store(link)
	<-link.Conn.Done()
	s.host.Store(nil)
	log.Logf(logbuf.Info, "agent: connection to host port %d ended", api.HostPort)
}

const relayStreams = 4096 - 256

const fileLimit = 524288

func raiseFileLimit(log *logbuf.Buffer) {
	lim := syscall.Rlimit{Cur: fileLimit, Max: fileLimit}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		log.Logf(logbuf.Warning, "init: cannot raise the descriptor limit to %d: %v", fileLimit, err)
	}
}

const signalShutdownDeadline = api.DefaultShutdownDeadlineMS * time.Millisecond

func superviseSignals(sys *system, log *logbuf.Buffer) {
	sig := make(chan os.Signal, 16)
	signal.Notify(sig, unix.SIGCHLD, unix.SIGTERM, unix.SIGINT, unix.SIGPWR)
	var once sync.Once
	for s := range sig {
		switch s {
		case unix.SIGCHLD:
			sys.reaper.Reap()
		default:
			once.Do(func() {
				log.Logf(logbuf.Info, "init: %v received, shutting down", s)
				go func() {
					if err := sys.Shutdown(s == unix.SIGINT, signalShutdownDeadline, func(api.ShutdownProgress) {}); err != nil {
						log.Logf(logbuf.Error, "init: shutdown failed: %v", err)
					}
				}()
			})
		}
	}
}
