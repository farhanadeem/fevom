package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"fev/proto/agent"
	"fev/proto/version"
)

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "agentctl: "+format+"\n", args...)
	os.Exit(1)
}

func emit(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		fail("%v", err)
	}
	fmt.Println(string(data))
}

func dial(path string, wait time.Duration) (*agent.Client, *agent.HelloResult) {
	deadline := time.Now().Add(wait)
	for {
		conn, err := net.Dial("unix", path)
		if err == nil {
			ctx, cancel := context.WithDeadline(context.Background(), deadline.Add(time.Second))
			client, hello, herr := agent.NewClient(ctx, conn, "agentctl")
			cancel()
			if herr == nil {
				return client, hello
			}
			var mismatch *version.Mismatch
			if errors.As(herr, &mismatch) {
				fail("%v", herr)
			}
			err = herr
		}
		if time.Now().After(deadline) {
			fail("%s: %v", path, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func main() {
	socket := flag.String("socket", "", "unix socket for the agent's vsock port (DIR/vsock_1024.sock)")
	wait := flag.Duration("wait", 0, "keep trying to connect for this long")
	showVersion := flag.Bool("version", false, "print the build identifier and exit")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: agentctl -socket PATH [-wait D] hello | info | boot-trace | logs | fetch URL | ping ADDRESS | engine ... | domains set FILE | fileshare watch ...|bind | stats [-interval MS] [-count N] | storage info|trim|sync|budget ... | memory reclaim | shutdown")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Build)
		return
	}
	if *socket == "" || flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	command, args := flag.Arg(0), flag.Args()[1:]
	client, hello := dial(*socket, *wait)
	defer client.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	check := func(v any, err error) {
		if err != nil {
			fail("%s: %v", command, err)
		}
		emit(v)
	}
	switch command {
	case "hello":
		emit(hello)
	case "info":
		check(client.Info(ctx))
	case "boot-trace":
		fs := flag.NewFlagSet("boot-trace", flag.ExitOnError)
		waitReady := fs.Bool("wait", false, "block until init is ready")
		fs.Parse(args)
		check(client.BootTrace(ctx, *waitReady))
	case "logs":
		fs := flag.NewFlagSet("logs", flag.ExitOnError)
		backlog := fs.Bool("backlog", false, "include what was logged before now")
		dur := fs.Duration("for", 0, "stop after this long")
		source := fs.String("source", "", "kernel or init (default both)")
		fs.Parse(args)
		p := agent.LogSubscribeParams{Backlog: *backlog}
		if *source != "" {
			p.Sources = []string{*source}
		}
		cancel, err := client.SubscribeLogs(ctx, p, func(e agent.LogEntry) { emit(e) })
		if err != nil {
			fail("logs: %v", err)
		}
		var timeout <-chan time.Time
		if *dur > 0 {
			timeout = time.After(*dur)
		}
		select {
		case <-timeout:
		case <-ctx.Done():
		case <-client.Done():
		}
		cancel()
	case "fetch":
		if len(args) != 1 {
			fail("fetch needs a URL")
		}
		check(client.Fetch(ctx, agent.FetchParams{URL: args[0]}))
	case "ping":
		fs := flag.NewFlagSet("ping", flag.ExitOnError)
		count := fs.Int("count", 3, "echo requests to send")
		ttl := fs.Int("ttl", 0, "hop limit (0: the system default)")
		fs.Parse(args)
		if fs.NArg() != 1 {
			fail("ping needs an address")
		}
		check(client.PingHost(ctx, agent.PingParams{Address: fs.Arg(0), Count: *count, TTL: *ttl, TimeoutMS: 2000 * *count}))
	case "engine":
		if len(args) == 0 {
			fail("engine needs one of: status, ports, start, stop, restart, watch, configure")
		}
		if args[0] == "configure" {
			if len(args) != 2 {
				fail("engine configure needs a file, - for standard input, or {} for none")
			}
			var config []byte
			var err error
			switch args[1] {
			case "-":
				config, err = io.ReadAll(os.Stdin)
			case "{}":
				config = []byte("{}")
			default:
				config, err = os.ReadFile(args[1])
			}
			if err != nil {
				fail("engine configure: %v", err)
			}
			check(client.EngineConfigure(ctx, config))
			return
		}
		fs := flag.NewFlagSet("engine", flag.ExitOnError)
		grace := fs.Duration("grace", 0, "how long containers get to stop (default: the agent's)")
		dur := fs.Duration("for", 0, "watch: stop after this long")
		fs.Parse(args[1:])
		stop := agent.EngineStopParams{GraceMS: int(grace.Milliseconds())}
		switch args[0] {
		case "status":
			check(client.EngineStatus(ctx))
		case "ports":
			check(client.EnginePorts(ctx))
		case "start":
			check(client.EngineStart(ctx))
		case "stop":
			check(client.EngineStop(ctx, stop))
		case "restart":
			check(client.EngineRestart(ctx, stop))
		case "watch":
			cancel, err := client.SubscribeEngine(ctx, func(s agent.EngineStatus) { emit(s) })
			if err != nil {
				fail("engine watch: %v", err)
			}
			var timeout <-chan time.Time
			if *dur > 0 {
				timeout = time.After(*dur)
			}
			select {
			case <-timeout:
			case <-ctx.Done():
			case <-client.Done():
			}
			cancel()
		default:
			fail("unknown engine command %q", args[0])
		}
	case "mac-addresses":
		if len(args) < 1 || args[0] != "set" {
			fail("mac-addresses set takes the addresses, none for an empty set")
		}
		addrs := append([]string{}, args[1:]...)
		if err := client.MacAddressesSet(ctx, addrs); err != nil {
			fail("mac-addresses set: %v", err)
		}
		emit(agent.Empty{})
	case "domains":
		if len(args) != 2 || args[0] != "set" {
			fail("domains set needs a file of domains.set's parameters, or - for standard input")
		}
		var data []byte
		var err error
		if args[1] == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(args[1])
		}
		if err != nil {
			fail("domains set: %v", err)
		}
		var p agent.DomainsParams
		if err := json.Unmarshal(data, &p); err != nil {
			fail("domains set: %v", err)
		}
		check(client.DomainsSet(ctx, p))
	case "emulation":
		switch {
		case len(args) == 1 && args[0] == "status":
			check(client.EmulationStatus(ctx))
		case len(args) == 2 && args[0] == "set" && (args[1] == "on" || args[1] == "off"):
			check(client.EmulationSet(ctx, args[1] == "on"))
		default:
			fail("emulation status | set on|off")
		}
	case "k8s":
		follow := func(what string, start func(func(agent.OperationProgress)) (*agent.Operation, error)) {
			ended := make(chan struct{})
			var once sync.Once
			op, err := start(func(p agent.OperationProgress) {
				emit(p)
				if p.State != agent.OperationRunning {
					once.Do(func() { close(ended) })
				}
			})
			if err != nil {
				fail("%s: %v", what, err)
			}
			emit(op)
			select {
			case <-ended:
			case <-client.Done():
				fail("%s: the agent's connection ended", what)
			case <-ctx.Done():
				fail("%s: no end within the time limit", what)
			}
		}
		switch {
		case len(args) >= 1 && args[0] == "start":
			p := agent.K8sStartParams{}
			if len(args) > 1 {
				p.ArchiveURL = args[1]
			}
			follow("k8s start", func(pr func(agent.OperationProgress)) (*agent.Operation, error) { return client.K8sStart(ctx, p, pr) })
		case len(args) == 1 && args[0] == "stop":
			check(client.K8sStop(ctx))
		case len(args) >= 1 && args[0] == "reset":
			all := len(args) > 1 && args[1] == "-all"
			follow("k8s reset", func(pr func(agent.OperationProgress)) (*agent.Operation, error) {
				return client.K8sReset(ctx, agent.K8sResetParams{All: all}, pr)
			})
		case len(args) == 1 && args[0] == "status":
			check(client.K8sStatus(ctx))
		case len(args) == 2 && args[0] == "pull":
			check(client.K8sImagePull(ctx, args[1]))
		default:
			fail("k8s start [ARCHIVE_URL] | stop | reset [-all] | status | pull REF")
		}
	case "view":
		switch {
		case len(args) >= 3 && args[0] == "start":
			var n [3]uint64
			var isolated []string
			var name, group string
			nums := 0
			for i := 1; i < len(args); i++ {
				if args[i] == "-isolated" && i+1 < len(args) {
					isolated = append(isolated, args[i+1])
					i++
					continue
				}
				if (args[i] == "-user" || args[i] == "-group") && i+1 < len(args) {
					if args[i] == "-user" {
						name = args[i+1]
					} else {
						group = args[i+1]
					}
					i++
					continue
				}
				v, err := strconv.ParseUint(args[i], 10, 32)
				if err != nil || nums == 3 {
					fail("view start UID GID [THREADS] [-user NAME] [-group NAME] [-isolated NAME]...: %q", args[i])
				}
				n[nums] = v
				nums++
			}
			check(client.ViewStart(ctx, agent.ViewStartParams{UID: uint32(n[0]), GID: uint32(n[1]), User: name, Group: group, Threads: int(n[2]), Isolated: isolated}))
		case len(args) == 1 && args[0] == "stop":
			check(client.ViewStop(ctx))
		case len(args) == 1 && args[0] == "status":
			check(client.ViewStatus(ctx))
		case len(args) == 1 && args[0] == "objects":
			check(client.ViewObjects(ctx))
		default:
			fail("view start UID GID [THREADS] [-isolated NAME]... | stop | status | objects")
		}
	case "fileshare":
		switch {
		case len(args) > 0 && args[0] == "bind":
			var targets []agent.BindTarget
			for _, a := range args[1:] {
				if name, ok := strings.CutPrefix(a, "machine="); ok {
					targets = append(targets, agent.BindTarget{Context: agent.ExecMachine, Machine: name})
				} else if a == "engine" {
					targets = append(targets, agent.BindTarget{Context: agent.ExecEngine})
				} else {
					fail("fileshare bind takes engine and machine=NAME, not %q", a)
				}
			}
			check(client.FileShareBindIn(ctx, targets))
		case len(args) > 0 && args[0] == "watch":
			check(client.FileShareWatch(ctx, args[1:]))
		default:
			fail("fileshare needs: watch [PATH...] | bind [engine] [machine=NAME...]")
		}
	case "stats":
		fs := flag.NewFlagSet("stats", flag.ExitOnError)
		interval := fs.Int("interval", 0, "milliseconds between samples (default: the agent's, 2000)")
		count := fs.Int("count", 0, "stop after this many samples")
		dur := fs.Duration("for", 0, "stop after this long")
		fs.Parse(args)
		seen, enough := 0, make(chan struct{})
		end, err := client.SubscribeStats(ctx, *interval, func(s agent.StatsSample) {
			if *count == 0 || seen < *count {
				emit(s)
			}
			if seen++; seen == *count {
				close(enough)
			}
		})
		if err != nil {
			fail("stats: %v", err)
		}
		var timeout <-chan time.Time
		if *dur > 0 {
			timeout = time.After(*dur)
		}
		select {
		case <-enough:
		case <-timeout:
		case <-ctx.Done():
		case <-client.Done():
		}
		end()
	case "storage":
		if len(args) > 1 && args[0] == "budget" {
			p := agent.BudgetParams{Unlimited: len(args) == 2 && args[1] == "none"}
			if !p.Unlimited {
				if len(args) != 4 {
					fail("storage budget needs: AVAILABLE MARGIN IMAGE in bytes, or: none")
				}
				for i, to := range []*int64{&p.AvailableBytes, &p.MarginBytes, &p.ImageBytes} {
					n, err := strconv.ParseInt(args[1+i], 10, 64)
					if err != nil {
						fail("storage budget: %q is not a number of bytes", args[1+i])
					}
					*to = n
				}
			}
			check(client.StorageBudget(ctx, p))
			break
		}
		if len(args) > 0 && args[0] == "check" {
			fs := flag.NewFlagSet("storage check", flag.ExitOnError)
			wait := fs.Bool("wait", false, "stay for the check's end")
			follow := fs.Bool("follow", false, "start none: follow this boot's last check")
			fs.Parse(args[1:])
			ended := make(chan struct{})
			var once sync.Once
			start := client.StorageCheck
			if *follow {
				start = client.StorageCheckFollow
			}
			var pmu sync.Mutex
			var held []agent.OperationProgress
			printed := false
			op, err := start(ctx, func(p agent.OperationProgress) {
				pmu.Lock()
				if printed {
					emit(p)
				} else {
					held = append(held, p)
				}
				pmu.Unlock()
				if p.State != agent.OperationRunning {
					once.Do(func() { close(ended) })
				}
			})
			if err != nil {
				fail("storage check: %v", err)
			}
			pmu.Lock()
			emit(op)
			for _, p := range held {
				emit(p)
			}
			printed = true
			pmu.Unlock()
			if *wait {
				select {
				case <-ended:
				case <-client.Done():
					fail("storage check: the agent's connection ended")
				case <-ctx.Done():
					fail("storage check: no end within the time limit")
				}
			} else {
				select {
				case <-ended:
				case <-time.After(200 * time.Millisecond):
				}
			}
			break
		}
		if len(args) > 0 && args[0] == "usage" {
			fs := flag.NewFlagSet("storage usage", flag.ExitOnError)
			engine := fs.Bool("engine", false, "the engine's own account of its part as well")
			fs.Parse(args[1:])
			check(client.StorageUsage(ctx, agent.UsageParams{Engine: *engine}))
			break
		}
		if len(args) > 1 && args[0] == "salvage" {
			p := agent.SalvageParams{Dest: args[1]}
			for _, a := range args[2:] {
				switch k, v, _ := strings.Cut(a, "="); k {
				case "volume":
					p.Volumes = append(p.Volumes, v)
				case "machine":
					p.Machines = append(p.Machines, v)
				case "docker":
					p.Docker = true
				default:
					fail("storage salvage: %q is not volume=NAME, machine=NAME or docker", a)
				}
			}
			ended := make(chan agent.OperationProgress, 1)
			op, err := client.StorageSalvage(ctx, p, func(p agent.OperationProgress) {
				if p.State != agent.OperationRunning {
					ended <- p
					return
				}
				emit(p)
			})
			if err != nil {
				fail("storage salvage: %v", err)
			}
			emit(op)
			select {
			case p := <-ended:
				emit(p)
				if p.State != agent.OperationDone {
					os.Exit(1)
				}
			case <-client.Done():
				fail("storage salvage: the agent's connection ended")
			case <-ctx.Done():
				fail("storage salvage: interrupted; the salvage ends with this connection")
			}
			break
		}
		if len(args) != 1 {
			fail("storage needs one of: info, trim, sync, usage [-engine], check [-wait|-follow], salvage DEST ..., budget AVAILABLE MARGIN IMAGE|none")
		}
		switch args[0] {
		case "info":
			check(client.StorageInfo(ctx))
		case "sync":
			check(client.StorageSync(ctx))
		case "trim":
			check(client.StorageTrim(ctx, agent.TrimParams{}))
		default:
			fail("unknown storage command %q", args[0])
		}
	case "memory":
		if len(args) == 0 || args[0] != "reclaim" {
			fail("memory needs: reclaim [-floor MIB] [-no-compact]")
		}
		fs := flag.NewFlagSet("memory reclaim", flag.ExitOnError)
		floor := fs.Int("floor", 0, "file cache to leave, in MiB (default: the agent's, 128)")
		noCompact := fs.Bool("no-compact", false, "leave out the compaction that follows")
		fs.Parse(args[1:])
		check(client.MemoryReclaim(ctx, agent.ReclaimParams{FloorMiB: *floor, NoCompact: *noCompact}))
	case "shutdown":
		fs := flag.NewFlagSet("shutdown", flag.ExitOnError)
		reboot := fs.Bool("reboot", false, "reset instead of powering off")
		deadline := fs.Duration("deadline", 0, "abandon what is still running after this long (default: the agent's, 15s)")
		fs.Parse(args)
		p := agent.ShutdownParams{Reboot: *reboot, DeadlineMS: int(deadline.Milliseconds())}
		if err := client.Shutdown(ctx, p, func(ev agent.ShutdownProgress) { emit(ev) }); err != nil {
			fail("shutdown: %v", err)
		}
		select {
		case <-client.Done():
		case <-ctx.Done():
		case <-time.After(time.Duration(p.DeadlineMS)*time.Millisecond + 20*time.Second):
		}
	case "exec":
		os.Exit(runExec(client, *socket, args))
	default:
		fail("unknown command %q", command)
	}
}
