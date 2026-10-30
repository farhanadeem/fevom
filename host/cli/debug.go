package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"fev/host/terminfo"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

type debugFlags struct {
	context  string
	workdir  string
	reset    bool
	fallback bool
	readOnly bool
}

func (a *app) debugCmd() *cobra.Command {
	var f debugFlags
	cmd := &cobra.Command{
		Use:   "debug [flags] CONTAINER|IMAGE [COMMAND...]",
		Short: "Open a shell with a full tool set inside a container or image",
		Long: `Open a shell with a full set of tools (editors, process and network
inspection, tracing, HTTP clients) inside a container, a stopped container
or an image, without changing the target. The tools are under /nix and are
not in the target's own filesystem, so a container with no shell, a
distroless one, a read-only one, a stopped one and a bare image all work.

With no command it opens the debug shell; a COMMAND runs in its place and
its exit status is the CLI's. -w sets the working directory in the target.
--read-only keeps a stopped container's root read-only. A missing command
typed in the shell offers to install its package, and installed packages
are there in every later session.

-c debugs a container on a remote Docker context (its running containers
and images). --reset deletes the debug tool store, alone or before opening
a session. -f opens a plain shell in the container's own userland through
"docker exec" if the debug shell cannot start (the tools cannot be fetched,
or a remote host refuses the helper); without -f that is an error that says
why.

CONTAINER|IMAGE is a container's name or ID, or failing that a local
image's reference or ID. Nothing is pulled.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, argv []string) error {
			return a.debug(cmd.Context(), f, argv)
		},
		ValidArgsFunction: func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
	}
	cmd.Flags().SetInterspersed(false)
	fl := cmd.Flags()
	fl.StringVarP(&f.context, "context", "c", "", "a remote Docker context whose engine holds the target (its own containers and images)")
	fl.StringVarP(&f.workdir, "workdir", "w", "", "the working directory in the target (default: the target's)")
	fl.BoolVar(&f.reset, "reset", false, "delete the debug tool store; alone, or before opening the session")
	fl.BoolVarP(&f.fallback, "fallback", "f", false, `open "docker exec" in the container's own userland if the debug shell cannot start`)
	fl.BoolVar(&f.readOnly, "read-only", false, "keep a stopped container's root read-only (an image is always read-only)")
	return cmd
}

func (a *app) debug(ctx context.Context, f debugFlags, argv []string) error {
	switch {
	case f.reset && len(argv) == 0:
		return a.debugResetOnly(ctx, f)
	case len(argv) == 0:
		return a.debugNoTarget(ctx, f)
	}
	target, command := argv[0], argv[1:]
	if f.context != "" {
		return a.debugViaContext(ctx, f, target, command)
	}
	return a.debugLocal(ctx, f, target, command)
}

func (a *app) debugParams(f debugFlags, target string, command []string) api.DebugStartParams {
	tty, stdinTTY := terminal(a.Stdin) && terminal(a.Stdout), terminal(a.Stdin) && !terminal(a.Stdout)
	p := api.DebugStartParams{Target: target, Argv: command, Dir: f.workdir, ReadOnly: f.readOnly, TTY: tty, StdinTTY: stdinTTY}
	if tty || stdinTTY {
		if in, ok := fileOf(a.Stdin); ok {
			p.Rows, p.Cols = termSize(in)
		}
		if p.Rows == 0 || p.Cols == 0 {
			p.Rows, p.Cols = 24, 80
		}
		p.Terminfo = terminfo.FromEnv(os.Getenv)
	}
	env := sessionEnv(os.Getenv, tty || stdinTTY)
	if lang := os.Getenv("LANG"); lang != "" && !strings.ContainsAny(lang, "=\x00") {
		env = append(env, "LANG="+lang)
	}
	p.Env = env
	return p
}

func (a *app) debugLocal(ctx context.Context, f debugFlags, target string, command []string) error {
	p := a.debugParams(f, target, command)
	var code int
	err := a.withDaemon(ctx, false, func(c *api.Client) error {
		if f.reset {
			if _, err := c.DebugReset(ctx, false); err != nil {
				return err
			}
		}
		e, err := a.debugSession(ctx, c, p)
		if err != nil {
			if f.fallback && isToolStoreError(err) {
				fmt.Fprintf(a.Stderr, "fev debug: the debug tools could not be prepared (%v); opening a shell in the container's own userland (-f)\n", err)
				code, err = a.debugFallback(ctx, f, target, command)
				return err
			}
			return err
		}
		code, err = a.debugExit(ctx, f, e, func() (int, error) { return a.debugFallback(ctx, f, target, command) })
		return err
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return &exit{code: code, silent: true, err: fmt.Errorf("exit status %d", code)}
	}
	return nil
}

func (a *app) debugSession(ctx context.Context, c *api.Client, p api.DebugStartParams) (api.SessionExit, error) {
	sigs, sctx, stop := sessionSignals(ctx)
	defer stop()
	exits := make(chan api.SessionExit, 1)
	slow := time.AfterFunc(2*time.Second, func() {
		if !a.quiet {
			fmt.Fprintln(a.Stderr, "fev debug: preparing the debug tools (a VM's first session fetches them)...")
		}
	})
	started, err := c.DebugStart(sctx, p, func(e api.SessionExit) { exits <- e })
	slow.Stop()
	if err != nil {
		return api.SessionExit{}, err
	}
	a.debugBanner(started)
	return a.pump(ctx, c, sctx, sigs, started.ID, started.Token, p.TTY, exits, sessionStreams{in: a.Stdin, out: a.Stdout, errw: a.Stderr})
}

func (a *app) debugBanner(d *api.DebugStarted) {
	if a.quiet {
		return
	}
	who := d.Name
	if d.Image != "" {
		who = fmt.Sprintf("%s (%s)", d.Name, d.Image)
	}
	line := fmt.Sprintf("fev debug: %s, %s", who, d.Kind)
	if len(d.Notes) > 0 {
		line += "; " + d.Notes[0]
	}
	fmt.Fprintln(a.Stderr, line)
	for _, n := range d.Notes[min(1, len(d.Notes)):] {
		fmt.Fprintln(a.Stderr, "fev debug: "+n)
	}
}

func (a *app) debugExit(ctx context.Context, f debugFlags, e api.SessionExit, fallback func() (int, error)) (int, error) {
	if e.Note != "" {
		for _, l := range strings.Split(e.Note, "\n") {
			fmt.Fprintln(a.Stderr, "fev debug: "+l)
		}
	}
	switch e.Reason {
	case api.DebugReasonToolsUnavailable:
		why := e.Error
		if why == "" {
			why = "the debug tools could not be fetched (offline, or the cache is unreachable)"
		}
		if f.fallback {
			fmt.Fprintf(a.Stderr, "fev debug: %s; opening a shell in the container's own userland (-f)\n", why)
			return fallback()
		}
		return 0, failf(ExitFailed, "%s; retry with -f to open a shell in the container's own userland", why)
	case api.DebugReasonTargetGone:
		return 0, failf(ExitFailed, "the target ended while the session was open")
	}
	return exitStatus(e)
}

func (a *app) debugResetOnly(ctx context.Context, f debugFlags) error {
	if f.context != "" {
		r, err := newDebugRemote(ctx, a, f.context)
		if err != nil {
			return err
		}
		defer r.Close()
		freed, err := r.Reset(ctx)
		if err != nil {
			return err
		}
		return a.emitDebugReset(f.context, freed)
	}
	return a.withDaemon(ctx, false, func(c *api.Client) error {
		r, err := c.DebugReset(ctx, false)
		if err != nil {
			return err
		}
		return a.emitDebugReset("", r.FreedBytes)
	})
}

func (a *app) emitDebugReset(context string, freed int64) error {
	return a.emit(map[string]any{"freed_bytes": freed, "context": context}, func(w io.Writer) {
		where := ""
		if context != "" {
			where = " on " + context
		}
		if freed == 0 {
			fmt.Fprintf(w, "the debug tool store%s was already empty\n", where)
			return
		}
		if freed < 0 {
			fmt.Fprintf(w, "the debug tool store%s is gone\n", where)
			return
		}
		fmt.Fprintf(w, "the debug tool store%s is gone; %s freed\n", where, bytesText(freed))
	})
}

func (a *app) debugNoTarget(ctx context.Context, f debugFlags) error {
	return usagef("name a container or image to debug, for example \"fev debug CONTAINER\"; " +
		"\"docker ps\" lists running containers and \"docker images\" the images")
}

func isToolStoreError(err error) bool {
	var e *jsonrpc.Error
	return errors.As(err, &e) && e.Code == api.CodeDebugToolsUnavailable
}

func (a *app) debugFallback(ctx context.Context, f debugFlags, target string, command []string) (int, error) {
	l, err := a.layout()
	if err != nil {
		return 0, err
	}
	docker := filepath.Join(toolsDir(l), "docker")
	if _, err := os.Stat(docker); err != nil {
		return 0, failf(ExitFailed, "the fallback needs the docker CLI (%s); run \"fev setup\"", docker)
	}
	args := fallbackArgs(f, target, command, terminal(a.Stdin) && terminal(a.Stdout))
	env := append(os.Environ(), "DOCKER_HOST=unix://"+filepath.Join(l.RunDir(), "docker.sock"))
	return runDocker(ctx, docker, args, env, a.Stdin, a.Stdout, a.Stderr)
}

func fallbackArgs(f debugFlags, target string, command []string, tty bool) []string {
	args := []string{"exec", "-i"}
	if tty {
		args[1] = "-it"
	}
	if f.workdir != "" {
		args = append(args, "-w", f.workdir)
	}
	args = append(args, target)
	if len(command) == 0 {
		return append(args, "sh")
	}
	return append(args, command...)
}

func (a *app) debugViaContext(ctx context.Context, f debugFlags, target string, command []string) error {
	r, err := newDebugRemote(ctx, a, f.context)
	if err != nil {
		return err
	}
	defer r.Close()
	code, err := r.Debug(ctx, remoteDebug{
		Target: target, ReadOnly: f.readOnly, Reset: f.reset, Fallback: f.fallback,
		Dir: f.workdir, Command: command,
		In: a.Stdin, Out: a.Stdout, Err: a.Stderr,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return &exit{code: code, silent: true, err: fmt.Errorf("exit status %d", code)}
	}
	return nil
}
