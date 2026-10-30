package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"fev/host/terminfo"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/relay"
)

const FevomEnvVar = "FEVENV"

const signalExitCode = 255

var defaultEnvNames = []string{"COLORTERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION"}

type runFlags struct {
	machine, user, workdir string
	path, shell            bool
}

func (a *app) runCmd() *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "run [flags] [COMMAND [ARG...]]",
		Short: "Run a command in a Linux machine",
		Long: `Run a command in a Linux machine, the default one unless -m names another,
as its default user unless -u names another, in the Mac's working directory
as the machine sees it unless -w names a Linux one. With no command it
opens a login shell. A stopped machine is started first.

"fev COMMAND ..." is the same as "fev run COMMAND ..." for any word
that is not one of fev's commands, and "fev" alone opens a shell.

The command runs through the user's login shell with each word quoted, so
it gets the words as given; -s passes them unquoted, as one shell command
line. With -p, an absolute Mac path among the arguments that the machine
does not have is given as /mnt/mac/PATH. The exit status is the command's,
255 if a signal ended it. Variables named in FEVENV (NAME:NAME...) are
passed from the Mac, besides TERM and the terminal program's.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, argv []string) error {
			return a.run(cmd.Context(), f, argv)
		},
	}
	cmd.Flags().SetInterspersed(false)
	fl := cmd.Flags()
	fl.StringVarP(&f.machine, "machine", "m", "", "the machine, by its name exactly as written, case included (default: the default one)")
	fl.StringVarP(&f.user, "user", "u", "", "the user (default: the machine's default user)")
	fl.StringVarP(&f.workdir, "workdir", "w", "", "the working directory, a Linux path (default: the Mac's, as the machine sees it)")
	fl.BoolVarP(&f.path, "path", "p", false, "give absolute Mac paths the machine lacks as /mnt/mac/PATH")
	fl.BoolVarP(&f.shell, "shell", "s", false, "pass the command to the shell unquoted, as one command line")
	return cmd
}

func fallThrough(args []string, commands map[string]bool) []string {
	valued := map[string]bool{"--home": true, "--format": true, "-m": true, "--machine": true, "-u": true, "--user": true, "-w": true, "--workdir": true}
	plain := map[string]bool{"-q": true, "--quiet": true, "-p": true, "--path": true, "-s": true, "--shell": true}
	for i := 0; i < len(args); i++ {
		w := args[i]
		switch {
		case w == "--":
			return append([]string{"run"}, args...)
		case !strings.HasPrefix(w, "-") || w == "-":
			if commands[w] {
				return args
			}
			return append([]string{"run"}, args...)
		case plain[w]:
		case valued[w]:
			i++
		case strings.HasPrefix(w, "--") && valued[w[:max(strings.IndexByte(w, '='), 0)]]:
		case len(w) > 2 && w[1] != '-' && valued[w[:2]]:
		case len(w) > 2 && w[1] != '-' && strings.Trim(w[1:], "qps") == "":
		default:
			return args
		}
	}
	return append([]string{"run"}, args...)
}

func (a *app) run(ctx context.Context, f runFlags, argv []string) error {
	p := api.SessionStartParams{Machine: f.machine, User: f.user, Dir: f.workdir}
	if len(argv) == 0 {
		p.Login = true
	} else {
		p.Login, p.Shell = true, true
		line := argv
		if f.path {
			line = translateArgs(argv, os.Lstat)
		}
		p.Argv = commandLine(line, f.shell)
	}
	if p.Dir == "" {
		if wd, err := os.Getwd(); err == nil {
			p.Dir = linuxPath(wd)
		}
	}
	tty, stdinTTY := terminal(a.Stdin) && terminal(a.Stdout), terminal(a.Stdin) && !terminal(a.Stdout)
	p.TTY, p.StdinTTY = tty, stdinTTY
	if tty || stdinTTY {
		in, _ := fileOf(a.Stdin)
		p.Rows, p.Cols = termSize(in)
		if p.Rows == 0 || p.Cols == 0 {
			p.Rows, p.Cols = 24, 80
		}
	}
	p.Env = sessionEnv(os.Getenv, tty || stdinTTY)
	if tty || stdinTTY {
		p.Terminfo = terminfo.FromEnv(os.Getenv)
	}
	var code int
	err := a.withDaemon(ctx, false, func(c *api.Client) error {
		e, err := a.session(ctx, c, p, sessionStreams{in: a.Stdin, out: a.Stdout, errw: a.Stderr})
		if err != nil {
			return err
		}
		code, err = exitStatus(e)
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

func exitStatus(e api.SessionExit) (int, error) {
	switch {
	case e.Error != "":
		return 0, failf(ExitFailed, "%s", e.Error)
	case e.Signal != 0:
		return signalExitCode, nil
	case e.ExitCode < 0:
		return signalExitCode, nil
	}
	return e.ExitCode, nil
}

func sessionEnv(getenv func(string) string, term bool) []string {
	var env []string
	seen := map[string]bool{}
	add := func(name string) {
		if name == "" || seen[name] || strings.ContainsAny(name, "=\x00") {
			return
		}
		seen[name] = true
		if v := getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	if term {
		add("TERM")
	}
	for _, name := range defaultEnvNames {
		add(name)
	}
	for _, name := range strings.Split(getenv(FevomEnvVar), ":") {
		add(strings.TrimSpace(name))
	}
	return env
}

func linuxPath(mac string) string {
	mac = filepath.Clean(mac)
	if under, ok := sameInMachine(mac); ok {
		return under
	}
	if mac == "/" {
		return agent.ShareTarget
	}
	return agent.ShareTarget + mac
}

func sameInMachine(mac string) (string, bool) {
	for _, sp := range agent.MachineSharePaths {
		if mac == sp.Source || strings.HasPrefix(mac, sp.Source+"/") {
			return sp.Target + strings.TrimPrefix(mac, sp.Source), true
		}
	}
	return "", false
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+:,./-]+$`)

var reserved = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "fi": true, "case": true, "esac": true, "for": true, "while": true, "until": true, "do": true, "done": true, "in": true, "function": true, "select": true, "time": true, "!": true, "{": true, "}": true, "[[": true, "]]": true, "coproc": true}

func shellQuote(w string, first bool) string {
	if shellSafe.MatchString(w) && !(first && reserved[w]) {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}

const translated = "\x00fev-p\x00"

func commandLine(argv []string, raw bool) []string {
	out := make([]string, len(argv))
	for i, w := range argv {
		switch {
		case strings.HasPrefix(w, translated):
			out[i] = strings.TrimPrefix(w, translated)
		case raw:
			out[i] = w
		default:
			out[i] = shellQuote(w, i == 0)
		}
	}
	return out
}

var pathChars = regexp.MustCompile(`^/[A-Za-z0-9_@%+:,./-]+$`)

func pathCandidate(w string, lstat func(string) (os.FileInfo, error)) bool {
	if w == "/" || !pathChars.MatchString(w) || strings.HasPrefix(w, "//") {
		return false
	}
	clean := filepath.Clean(w)
	if clean != strings.TrimSuffix(w, "/") {
		return false
	}
	if _, same := sameInMachine(clean); same {
		return false
	}
	if clean == agent.ShareTarget || strings.HasPrefix(clean, agent.ShareTarget+"/") {
		return false
	}
	_, err := lstat(clean)
	return err == nil
}

func translateArgs(argv []string, lstat func(string) (os.FileInfo, error)) []string {
	out := append([]string{}, argv...)
	for i := 1; i < len(argv); i++ {
		w := argv[i]
		if w == "--" {
			break
		}
		prefix, value := "", w
		if strings.HasPrefix(w, "-") {
			eq := strings.IndexByte(w, '=')
			if eq < 0 {
				continue
			}
			prefix, value = w[:eq+1], w[eq+1:]
		}
		if !pathCandidate(value, lstat) {
			continue
		}
		q := shellQuote(value, false)
		alt := shellQuote(agent.ShareTarget+value, false)
		expr := `"$(if [ -e ` + q + ` ] || [ -h ` + q + ` ]; then printf %s ` + q + `; else printf %s ` + alt + `; fi)"`
		if prefix != "" {
			expr = shellQuote(prefix, false) + expr
		}
		out[i] = translated + expr
	}
	return out
}

type sessionStreams struct {
	in        io.Reader
	out, errw io.Writer
}

var linuxSignals = map[os.Signal]int{
	syscall.SIGHUP: 1, syscall.SIGINT: 2, syscall.SIGQUIT: 3, syscall.SIGTERM: 15,
	syscall.SIGUSR1: 10, syscall.SIGUSR2: 12,
}

func (a *app) session(ctx context.Context, c *api.Client, p api.SessionStartParams, s sessionStreams) (api.SessionExit, error) {
	sigs, sctx, stop := sessionSignals(ctx)
	defer stop()
	exits := make(chan api.SessionExit, 1)
	started, err := c.SessionStart(sctx, p, func(e api.SessionExit) { exits <- e })
	if err != nil {
		return api.SessionExit{}, err
	}
	return a.pump(ctx, c, sctx, sigs, started.ID, started.Token, p.TTY, exits, s)
}

func sessionSignals(ctx context.Context) (chan os.Signal, context.Context, func()) {
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH)
	return sigs, context.WithoutCancel(ctx), func() { signal.Stop(sigs) }
}

func (a *app) pump(ctx context.Context, c *api.Client, sctx context.Context, sigs chan os.Signal, id uint64, token string, tty bool, exits chan api.SessionExit, s sessionStreams) (api.SessionExit, error) {
	l, err := a.layout()
	if err != nil {
		return api.SessionExit{}, err
	}
	stream, err := api.AttachSession(sctx, l.Home, token)
	if err != nil {
		c.SessionClose(sctx, id)
		return api.SessionExit{}, failf(ExitFailed, "the session's stream: %v", err)
	}
	defer stream.Close()

	var restore func()
	if tty {
		if f, ok := fileOf(s.in); ok {
			if restore, err = rawMode(f); err != nil {
				c.SessionClose(sctx, id)
				return api.SessionExit{}, failf(ExitFailed, "the terminal: %v", err)
			}
			defer restore()
		}
	}

	copied := make(chan struct{})
	if tty {
		go func() {
			io.Copy(stream, s.in)
		}()
		go func() {
			io.Copy(s.out, stream)
			close(copied)
		}()
	} else {
		go func() {
			buf := make([]byte, relay.MaxFrame)
			for {
				n, err := s.in.Read(buf)
				if n > 0 {
					if relay.WriteFrame(stream, relay.Stdin, buf[:n]) != nil {
						return
					}
				}
				if err != nil {
					relay.WriteFrame(stream, relay.Stdin, nil)
					return
				}
			}
		}()
		go func() {
			defer close(copied)
			buf := make([]byte, relay.MaxFrame)
			for {
				kind, n, err := relay.ReadFrame(stream, buf)
				if err != nil {
					return
				}
				switch kind {
				case relay.Stdout:
					s.out.Write(buf[:n])
				case relay.Stderr:
					s.errw.Write(buf[:n])
				}
			}
		}()
	}

	var exit api.SessionExit
	var got bool
	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGWINCH {
				if f, ok := fileOf(s.in); ok && tty {
					if rows, cols := termSize(f); rows > 0 && cols > 0 {
						c.SessionResize(sctx, id, rows, cols)
					}
				}
				continue
			}
			if n, ok := linuxSignals[sig]; ok {
				c.SessionSignal(sctx, id, n)
			}
		case e := <-exits:
			exit, got = e, true
			exits = nil
		case <-copied:
			copied = nil
		case <-c.Done():
			if got {
				return exit, nil
			}
			return api.SessionExit{}, failf(ExitNotRunning, "the daemon stopped during the session; how the command ended is not known")
		}
		if got && copied == nil {
			return exit, nil
		}
		if got && exit.Error != "" {
			return exit, nil
		}
	}
}
