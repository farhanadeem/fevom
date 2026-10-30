package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"fev/host/daemon/boottrace"
	"fev/host/daemon/home"
	"fev/host/daemon/launch"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/relay"
	"fev/proto/version"
)

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fevctl-dev: "+format+"\n", args...)
	os.Exit(1)
}

func emit(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		fail("%v", err)
	}
	fmt.Println(string(data))
}

func failCall(what string, err error) {
	var e *jsonrpc.Error
	if errors.As(err, &e) {
		data, _ := json.Marshal(e)
		fmt.Fprintf(os.Stderr, "fevctl-dev: %s: %s\n", what, data)
		os.Exit(1)
	}
	fail("%s: %v", what, err)
}

type sources []string

func (s *sources) String() string     { return strings.Join(*s, ",") }
func (s *sources) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	originNS := boottrace.Now()
	homeFlag := flag.String("home", "", "the daemon's home (default $FEV_HOME, else ~/.fev)")
	noStart := flag.Bool("no-start", false, "fail if no daemon is running instead of starting one")
	fakeVM := flag.Bool("fake-vm", false, "if a daemon is started, start it with --fake-vm")
	showVersion := flag.Bool("version", false, "print the build identifier and exit")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: fevctl-dev [--home DIR] [--no-start] [--fake-vm] call METHOD [JSON] | watch | logs | stats [-interval MS] [-count N] | hello | power sleep|wake|poweron|notsleep | hold")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Build)
		return
	}
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	layout, err := home.Resolve(*homeFlag, "")
	if err != nil {
		fail("%v", err)
	}
	opts := launch.Options{Layout: layout, Client: "fevctl-dev", OriginNS: originNS, SSHAuthSock: os.Getenv("SSH_AUTH_SOCK"),
		Terminfo: os.Getenv("TERMINFO"), TerminfoDirs: os.Getenv("TERMINFO_DIRS")}
	if *homeFlag != "" {
		opts.Args = append(opts.Args, "--home", layout.Home)
	}
	if *fakeVM {
		opts.Args = append(opts.Args, "--fake-vm")
	}
	opts.Args = append(opts.Args, strings.Fields(os.Getenv("FEV_DAEMON_ARGS"))...)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	connect := func() (net.Conn, error) { return net.Dial("unix", layout.Socket()) }
	dial := func() (*api.Client, *api.HelloResult) {
		if *noStart {
			conn, err := connect()
			if err != nil {
				fail("no daemon is running in %s: %v", layout.Home, err)
			}
			client, hello, err := api.NewClient(ctx, conn, opts.Client, opts.HelloOptions()...)
			if err != nil {
				failCall("hello", err)
			}
			return client, hello
		}
		client, hello, err := launch.Dial(ctx, opts)
		if err != nil {
			failCall("connect", err)
		}
		return client, hello
	}

	command, args := flag.Arg(0), flag.Args()[1:]
	switch command {
	case "hello":
		_, hello := dial()
		emit(hello)

	case "call":
		if len(args) < 1 || len(args) > 2 {
			fail("usage: call METHOD [JSON]")
		}
		method := args[0]
		if !strings.Contains(method, ".") {
			method = "daemon." + method
		}
		var params json.RawMessage
		if len(args) == 2 {
			params = json.RawMessage(args[1])
			if !json.Valid(params) {
				fail("the parameters are not JSON: %s", args[1])
			}
		}
		call := func() (json.RawMessage, error) {
			conn, err := connect()
			if err != nil && !*noStart {
				if err = launch.Ensure(opts); err == nil {
					conn, err = connect()
				}
			}
			if err != nil {
				return nil, err
			}
			c := jsonrpc.NewConn(conn, nil)
			defer c.Close()
			if err := c.Call(ctx, api.MethodHello, api.HelloParams{APIVersion: api.APIVersion, Build: version.Build, Client: opts.Client, SSHAuthSock: opts.SSHAuthSock,
				Terminfo: opts.Terminfo, TerminfoDirs: opts.TerminfoDirs}, nil); err != nil {
				return nil, err
			}
			var result json.RawMessage
			err = c.Call(ctx, method, params, &result)
			return result, err
		}
		result, err := call()
		if api.IsShuttingDown(err) && !*noStart && method != api.MethodShutdown {
			if _, serr := launch.Spawn(opts); serr != nil {
				fail("%v", serr)
			}
			result, err = call()
		}
		if err != nil {
			failCall(method, err)
		}
		fmt.Println(string(result))

	case "op":
		if len(args) != 2 || !json.Valid([]byte(args[1])) {
			fail("usage: op METHOD JSON")
		}
		client, _ := dial()
		defer client.Close()
		ops := make(chan api.Operation, 4096)
		_, end, err := client.Subscribe(ctx, api.Events{Operation: func(op api.Operation) { ops <- op }})
		if err != nil {
			failCall("events.subscribe", err)
		}
		defer end()
		var op *api.Operation
		raw := []byte(args[1])
		switch args[0] {
		case api.MethodMachinesCreate:
			var p api.MachinesCreateParams
			json.Unmarshal(raw, &p)
			op, err = client.CreateMachine(ctx, p)
		case api.MethodMachinesExport:
			var p api.MachinesExportParams
			json.Unmarshal(raw, &p)
			op, err = client.ExportMachine(ctx, p)
		case api.MethodMachinesImport:
			var p api.MachinesImportParams
			json.Unmarshal(raw, &p)
			op, err = client.ImportMachine(ctx, p)
		case api.MethodK8sEnable:
			op, err = client.K8sEnable(ctx)
		case api.MethodK8sDisable:
			op, err = client.K8sDisable(ctx)
		case api.MethodK8sReset:
			var p api.K8sResetParams
			json.Unmarshal(raw, &p)
			op, err = client.K8sReset(ctx, p)
		default:
			fail("op: %s is not an operation this knows", args[0])
		}
		if err != nil {
			failCall(args[0], err)
		}
		started := *op
		emit(started)
		for op := range ops {
			if op.ID != started.ID {
				continue
			}
			emit(op)
			if op.State != api.OperationRunning {
				if op.State != api.OperationDone {
					os.Exit(1)
				}
				return
			}
		}

	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		machine := fs.String("m", "", "the machine (default: the default one)")
		user := fs.String("u", "", "the user (default: the machine's)")
		fs.Parse(args)
		client, _ := dial()
		defer client.Close()
		exits := make(chan api.SessionExit, 1)
		started, err := client.SessionStart(ctx, api.SessionStartParams{Machine: *machine, User: *user, Argv: fs.Args(), Login: fs.NArg() == 0},
			func(e api.SessionExit) { exits <- e })
		if err != nil {
			failCall("session.start", err)
		}
		stream, err := api.AttachSession(ctx, layout.Home, started.Token)
		if err != nil {
			fail("attaching the session: %v", err)
		}
		go func() {
			buf := make([]byte, 32<<10)
			for {
				n, err := os.Stdin.Read(buf)
				if n > 0 {
					relay.WriteFrame(stream, relay.Stdin, buf[:n])
				}
				if err != nil {
					relay.WriteFrame(stream, relay.Stdin, nil)
					return
				}
			}
		}()
		buf := make([]byte, relay.MaxFrame)
		for {
			kind, n, err := relay.ReadFrame(stream, buf)
			if err != nil {
				break
			}
			if kind == relay.Stderr {
				os.Stderr.Write(buf[:n])
			} else {
				os.Stdout.Write(buf[:n])
			}
		}
		e := <-exits
		switch {
		case e.Error != "":
			fail("%s", e.Error)
		case e.Signal != 0:
			os.Exit(128 + e.Signal)
		}
		os.Exit(e.ExitCode)

	case "k8s-watch":
		fs := flag.NewFlagSet("k8s-watch", flag.ExitOnError)
		forD := fs.Duration("for", 5*time.Second, "how long to follow")
		ns := fs.String("ns", "", "the namespace (default: all)")
		fs.Parse(args)
		client, _ := dial()
		defer client.Close()
		w, stopW, err := client.K8sWatch(ctx, api.K8sWatchParams{Kinds: fs.Args(), Namespace: *ns}, func(ch api.K8sChanged) { emit(ch) })
		if err != nil {
			failCall("k8s.watch", err)
		}
		emit(w)
		select {
		case <-time.After(*forD):
		case <-ctx.Done():
		}
		stopW()
		return
	case "k8s-logs":
		fs := flag.NewFlagSet("k8s-logs", flag.ExitOnError)
		follow := fs.Bool("f", false, "follow")
		tail := fs.Int("tail", 0, "lines from the end")
		container := fs.String("c", "", "the container")
		previous := fs.Bool("previous", false, "the last terminated instance's")
		fs.Parse(args)
		if fs.NArg() != 2 {
			fail("k8s-logs NS POD")
		}
		client, _ := dial()
		defer client.Close()
		ended := make(chan error, 1)
		if _, err := client.K8sLogs(ctx, api.K8sLogsParams{Namespace: fs.Arg(0), Pod: fs.Arg(1), Container: *container, Follow: *follow, TailLines: *tail, Previous: *previous},
			func(s string) { os.Stdout.WriteString(s) }, func(err error) { ended <- err }); err != nil {
			failCall("k8s.logs", err)
		}
		select {
		case err := <-ended:
			if err != nil {
				fail("k8s.logs: %v", err)
			}
		case <-ctx.Done():
		}
		return
	case "k8s-exec":
		fs := flag.NewFlagSet("k8s-exec", flag.ExitOnError)
		tty := fs.Bool("t", false, "a terminal")
		rows := fs.Int("rows", 0, "the terminal's rows")
		cols := fs.Int("cols", 0, "the terminal's columns")
		container := fs.String("c", "", "the container")
		fs.Parse(args)
		if fs.NArg() < 2 {
			fail("k8s-exec NS POD [ARGV]")
		}
		client, _ := dial()
		defer client.Close()
		exits := make(chan api.SessionExit, 1)
		started, err := client.K8sExec(ctx, api.K8sExecParams{Namespace: fs.Arg(0), Pod: fs.Arg(1), Container: *container, Argv: fs.Args()[2:], TTY: *tty, Rows: *rows, Cols: *cols},
			func(e api.SessionExit) { exits <- e })
		if err != nil {
			failCall("k8s.exec", err)
		}
		stream, err := api.AttachSession(ctx, layout.Home, started.Token)
		if err != nil {
			fail("attaching the session: %v", err)
		}
		go func() {
			buf := make([]byte, 32<<10)
			for {
				n, err := os.Stdin.Read(buf)
				if n > 0 {
					stream.Write(buf[:n])
				}
				if err != nil {
					stream.CloseWrite()
					return
				}
			}
		}()
		buf := make([]byte, 32<<10)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				os.Stdout.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		e := <-exits
		if e.Error != "" {
			fail("%s", e.Error)
		}
		os.Exit(e.ExitCode)
	case "watch":
		fs := flag.NewFlagSet("watch", flag.ExitOnError)
		dur := fs.Duration("for", 0, "stop after this long")
		until := fs.String("until", "", "stop when the VM's state is this (stopped, starting, running, stopping)")
		fs.Parse(args)
		client, _ := dial()
		defer client.Close()
		events := make(chan any, 4096)
		queue := func(v any) {
			select {
			case events <- v:
			default:
			}
		}
		st, _, err := client.Subscribe(ctx, api.Events{
			State:   func(st api.Status) { queue(st) },
			Gap:     func(missed uint64) { queue(map[string]any{"gap": missed}) },
			Config:  func(ch api.ConfigChange) { queue(map[string]any{"config": ch}) },
			Forward: func(f api.Forward, removed bool) { queue(map[string]any{"forward": f, "removed": removed}) },
			Docker:  func(e api.DockerEvent) { queue(map[string]any{"docker": e}) },
			Files:   func(f api.FilesStatus) { queue(map[string]any{"files": f}) },
			Sleep:   func(n api.PowerSleep) { queue(map[string]any{"power.sleep": n}) },
			Wake:    func(n api.PowerWake) { queue(map[string]any{"power.wake": n}) },
		})
		if err != nil {
			failCall("events.subscribe", err)
		}
		var timeout <-chan time.Time
		if *dur > 0 {
			timeout = time.After(*dur)
		}
		emit(st)
		for st.State != *until {
			select {
			case v := <-events:
				emit(v)
				if next, ok := v.(api.Status); ok {
					st = &next
				}
			case <-timeout:
				return
			case <-client.Done():
				return
			case <-ctx.Done():
				return
			}
		}

	case "power":
		if len(args) != 1 || !map[string]bool{"sleep": true, "wake": true, "poweron": true, "notsleep": true}[args[0]] {
			fail("usage: power sleep|wake|poweron|notsleep")
		}
		conn, err := net.Dial("unix", filepath.Join(layout.RunDir(), "power.sock"))
		if err != nil {
			fail("no daemon in %s takes injected power notifications (start fevd with --power-inject): %v", layout.Home, err)
		}
		defer conn.Close()
		fmt.Fprintln(conn, args[0])
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			fail("the daemon went away before it had dealt with the notification: %v", err)
		}
		fmt.Print(line)

	case "hold":
		fs := flag.NewFlagSet("hold", flag.ExitOnError)
		display := fs.Bool("display", false, "keep the display awake too")
		dur := fs.Duration("for", 0, "release after this long (default: until interrupted)")
		fs.Parse(args)
		client, _ := dial()
		defer client.Close()
		held, err := client.CaffeinateHold(ctx, api.CaffeinateParams{Reason: strings.Join(fs.Args(), " "), Display: *display})
		if err != nil {
			failCall("caffeinate.hold", err)
		}
		emit(held)
		wait(ctx, client, *dur)

	case "stats":
		fs := flag.NewFlagSet("stats", flag.ExitOnError)
		interval := fs.Int("interval", 0, "milliseconds between samples (default: the daemon's, 2000)")
		count := fs.Int("count", 0, "stop after this many samples")
		dur := fs.Duration("for", 0, "stop after this long")
		fs.Parse(args)
		client, _ := dial()
		defer client.Close()
		seen := 0
		enough, cancel := context.WithCancel(ctx)
		defer cancel()
		end, err := client.SubscribeStats(ctx, *interval, func(s api.StatsSample) {
			if *count == 0 || seen < *count {
				emit(s)
			}
			if seen++; seen == *count {
				cancel()
			}
		})
		if err != nil {
			failCall("stats.subscribe", err)
		}
		wait(enough, client, *dur)
		end()

	case "logs":
		fs := flag.NewFlagSet("logs", flag.ExitOnError)
		backlog := fs.Int("backlog", 0, "also show up to this many earlier lines of each source")
		dur := fs.Duration("for", 0, "stop after this long")
		var only sources
		fs.Var(&only, "source", "daemon, vmm, console, kernel, init or engine; repeatable (default all)")
		fs.Parse(args)
		client, _ := dial()
		defer client.Close()
		_, err := client.SubscribeLogs(ctx, api.LogSubscribeParams{Sources: only, Backlog: *backlog}, func(e api.LogEntry) { emit(e) })
		if err != nil {
			failCall("log.subscribe", err)
		}
		wait(ctx, client, *dur)

	default:
		flag.Usage()
		os.Exit(2)
	}
}

func wait(ctx context.Context, client *api.Client, dur time.Duration) {
	var timeout <-chan time.Time
	if dur > 0 {
		timeout = time.After(dur)
	}
	select {
	case <-timeout:
	case <-client.Done():
	case <-ctx.Done():
	}
}
