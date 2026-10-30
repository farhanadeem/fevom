package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"fev/host/daemon/logs"
	api "fev/proto/daemon"
)

func exposure(f api.Forward) string {
	for _, addr := range f.Mac {
		if strings.HasPrefix(addr, "0.0.0.0:") || strings.HasPrefix(addr, "[::]:") {
			return "LAN"
		}
	}
	return "this Mac"
}

func owner(f api.Forward) string {
	if f.Source == api.ForwardDocker && len(f.Owner) > 12 {
		return f.Owner[:12]
	}
	return f.Owner
}

func conflictLine(f api.Forward) string {
	why := f.Error
	if why == "" {
		why = "something else on this Mac has the port"
	}
	return fmt.Sprintf("port %s (%s) of %s %s is not served: %s; \"fev ports --retry\" tries again", strings.Join(f.Mac, ", "), f.Proto, f.Source, owner(f), why)
}

func portsText(w io.Writer, forwards []api.Forward) {
	rows := make([][]string, 0, len(forwards))
	for _, f := range forwards {
		rows = append(rows, []string{f.Proto, strings.Join(f.Mac, ","), f.Guest, f.State, exposure(f), f.Source + " " + owner(f), f.Error})
	}
	table(w, []string{"PROTO", "MAC", "GUEST", "STATE", "REACH", "FOR", "NOTE"}, rows)
}

func (a *app) portsCmd() *cobra.Command {
	var retry bool
	cmd := &cobra.Command{
		Use:   "ports [ID]",
		Short: "List published ports, with their conflicts and who can reach them",
		Long: `List the ports of this Mac that lead into the VM. STATE is "conflict" when
something else on the Mac had the port, and NOTE says what was found; REACH
is "LAN" for a port that answers other machines on the network.

A port in conflict is not tried again by itself. --retry tries every one
again, or with an ID (as --format json shows it) that one.

The exit status is 1 if any port listed is in conflict or failed.`,
		Args: args(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			if len(argv) == 1 && !retry {
				return usagef("ports: an ID is for --retry")
			}
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				var forwards []api.Forward
				var err error
				if retry {
					id := ""
					if len(argv) == 1 {
						id = argv[0]
					}
					forwards, err = c.ForwardsRetry(cmd.Context(), id)
				} else {
					forwards, err = c.Forwards(cmd.Context())
				}
				if err != nil {
					return err
				}
				if forwards == nil {
					forwards = []api.Forward{}
				}
				if err := a.emit(api.Forwards{Forwards: forwards}, func(w io.Writer) { portsText(w, forwards) }); err != nil {
					return err
				}
				bad := 0
				for _, f := range forwards {
					if f.State != api.ForwardActive {
						bad++
					}
				}
				if bad > 0 {
					return &exit{code: ExitFailed, silent: a.json(), err: fmt.Errorf("%d of %d ports are not served", bad, len(forwards))}
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&retry, "retry", false, "try again to listen for ports in conflict")
	return cmd
}

func (a *app) logsCmd() *cobra.Command {
	var lines int
	var sources []string
	var all, files bool
	cmd := &cobra.Command{
		Use:   "logs [MACHINE]",
		Short: "Print a Linux machine's console, or follow fev's own log",
		Long: `With MACHINE, print the machine's console as it holds it (its last 1,000
lines, a stopped machine's last run included) and end. -a puts the machine
runtime's lines about it (its starts and stops) first and, for a running
machine, its init system's journal of this boot after (journalctl -b, or
the end of /var/log/messages where there is no journal).

With no machine, print the last lines of fev's own unified log and then
every new line, until interrupted. Sources are daemon, vmm, console,
kernel, init and engine. --files prints everything fev's log files hold
instead, rotated files included, merged by time, and ends; that reads the
files, asks no daemon and starts none.

With --format json each line is one object.`,
		Args: args(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			if len(argv) == 1 {
				if files || len(sources) > 0 || cmd.Flags().Changed("lines") {
					return usagef("logs: --files, --source and --lines are fev's own log's, not a machine's")
				}
				return a.machineLogs(cmd.Context(), argv[0], all)
			}
			if all {
				return usagef("logs -a: name a machine: -a gives all of a machine's sources")
			}
			if lines < 0 {
				return usagef("logs: --lines must not be negative")
			}
			if files {
				return a.logsAll(sources)
			}
			ctx := cmd.Context()
			return a.withDaemon(ctx, false, func(c *api.Client) error {
				_, err := c.SubscribeLogs(ctx, api.LogSubscribeParams{Sources: sources, Backlog: lines}, func(e api.LogEntry) {
					if e.Dropped > 0 {
						fmt.Fprintf(a.Stderr, "fev: %d lines of %s were dropped: this terminal was not reading fast enough\n", e.Dropped, e.Source)
					}
					if a.json() {
						data, _ := jsonLine(e)
						fmt.Fprintln(a.Stdout, data)
						return
					}
					fmt.Fprintf(a.Stdout, "%s %-7s %s\n", e.Time, e.Source, e.Text)
				})
				if err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-c.Done():
					<-c.Drained()
					return &exit{code: ExitNotRunning, err: fmt.Errorf("the daemon has stopped")}
				}
			})
		},
	}
	cmd.Flags().IntVarP(&lines, "lines", "n", 20, "earlier lines of each source to print first")
	cmd.Flags().StringArrayVar(&sources, "source", nil, "only this source; repeatable")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "with MACHINE: the runtime's lines, the console, and the journal")
	cmd.Flags().BoolVar(&files, "files", false, "print everything in fev's log files, rotated ones included, and end")
	return cmd
}

const machineBacklog = 1000

func (a *app) machineLogs(ctx context.Context, name string, all bool) error {
	return a.withDaemon(ctx, false, func(c *api.Client) error {
		m, err := c.Machine(ctx, name)
		if err != nil {
			return err
		}
		sources := []string{api.MachineConsoleLog(name)}
		if all {
			sources = []string{api.MachineRuntimeLog(name), api.MachineConsoleLog(name)}
		}
		for _, source := range sources {
			if err := a.printBacklog(ctx, c, source); err != nil {
				return err
			}
		}
		if !all || m.State != api.MachineRunning {
			return nil
		}
		line := "if command -v journalctl >/dev/null 2>&1; then journalctl -b --no-pager; " +
			"elif [ -r /var/log/messages ]; then tail -n 1000 /var/log/messages; fi"
		p := api.SessionStartParams{Machine: name, User: "root", Login: true, Shell: true, Argv: []string{line}}
		e, err := a.session(ctx, c, p, sessionStreams{in: strings.NewReader(""), out: a.Stdout, errw: a.Stderr})
		if err != nil {
			return err
		}
		if _, err := exitStatus(e); err != nil {
			return err
		}
		return nil
	})
}

func (a *app) printBacklog(ctx context.Context, c *api.Client, source string) error {
	done := make(chan struct{})
	ended := false
	stop, err := c.SubscribeLogsMarked(ctx, api.LogSubscribeParams{Sources: []string{source}, Backlog: machineBacklog}, func(e api.LogEntry) {
		if ended {
			return
		}
		if e.Dropped > 0 {
			fmt.Fprintf(a.Stderr, "fev: %d lines of %s were dropped: this terminal was not reading fast enough\n", e.Dropped, e.Source)
		}
		if a.json() {
			data, _ := jsonLine(e)
			fmt.Fprintln(a.Stdout, data)
			return
		}
		fmt.Fprintln(a.Stdout, e.Text)
	}, func() { ended = true; close(done) })
	if err != nil {
		return err
	}
	defer stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.Done():
		<-c.Drained()
		return &exit{code: ExitNotRunning, err: fmt.Errorf("the daemon has stopped")}
	}
}

func (a *app) logsAll(sources []string) error {
	for _, source := range sources {
		known := false
		for _, s := range logs.Sources {
			known = known || s == source
		}
		if !known {
			return usagef("logs: no source %q; the sources are %s", source, strings.Join(logs.Sources, ", "))
		}
	}
	l, err := a.layout()
	if err != nil {
		return err
	}
	out := bufio.NewWriter(a.Stdout)
	defer out.Flush()
	return logs.ReadAll(l.LogDir(), sources, func(e logs.Entry) {
		at := e.Time.UTC().Format("2006-01-02T15:04:05.000000Z")
		if a.json() {
			data, _ := jsonLine(api.LogEntry{Source: e.Source, Time: at, Level: e.Level, Text: e.Text})
			fmt.Fprintln(out, data)
			return
		}
		fmt.Fprintf(out, "%s %-7s %s\n", at, e.Source, e.Text)
	})
}

func (a *app) caffeinateCmd() *cobra.Command {
	var display bool
	cmd := &cobra.Command{
		Use:   "caffeinate [-d] [--] [COMMAND [ARG...]]",
		Short: "Keep the Mac awake while a command runs",
		Long: `Keep the Mac from going to sleep by itself while COMMAND runs, and exit
with COMMAND's status. With no command, until interrupted.

It prevents the sleep the Mac starts when idle. It does not prevent the one
you ask for by closing the lid.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, argv []string) error {
			ctx := cmd.Context()
			return a.withDaemon(ctx, false, func(c *api.Client) error {
				reason := "fev caffeinate"
				if len(argv) > 0 {
					reason = strings.Join(argv, " ")
				}
				if _, err := c.CaffeinateHold(ctx, api.CaffeinateParams{Reason: reason, Display: display}); err != nil {
					return err
				}
				if len(argv) == 0 {
					<-ctx.Done()
					return ctx.Err()
				}
				child := exec.Command(argv[0], argv[1:]...)
				child.Stdin, child.Stdout, child.Stderr = a.Stdin, a.Stdout, a.Stderr
				return runChild(child)
			})
		},
	}
	cmd.Flags().BoolVarP(&display, "display", "d", false, "keep the display awake as well")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func runChild(child *exec.Cmd) error {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	if err := child.Start(); err != nil {
		return failf(127, "cannot run %s: %v", child.Path, err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	for {
		select {
		case sig := <-signals:
			if sig == syscall.SIGTERM || sig == syscall.SIGHUP || !foreground() {
				child.Process.Signal(sig)
			}
		case err := <-done:
			return childStatus(child, err)
		}
	}
}

var foreground = func() bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	defer tty.Close()
	pgrp, err := unix.IoctlGetInt(int(tty.Fd()), unix.TIOCGPGRP)
	return err == nil && pgrp == unix.Getpgrp()
}

func childStatus(child *exec.Cmd, err error) error {
	if err == nil {
		return nil
	}
	code := child.ProcessState.ExitCode()
	if ws, ok := child.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		code = 128 + int(ws.Signal())
	}
	return &exit{code: code, silent: true, err: err}
}
