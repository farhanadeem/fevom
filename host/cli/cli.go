package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"fev/host/bundle"
	"fev/host/daemon/home"
	"fev/host/daemon/launch"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/version"
)

const (
	ExitOK          = 0
	ExitFailed      = 1
	ExitUsage       = 2
	ExitNotRunning  = 3
	ExitUnreachable = 4
	ExitConfig      = 5
	ExitInterrupted = 130
)

const DaemonArgsEnv = "FEV_DAEMON_ARGS"

type Env struct {
	Name          string
	Args          []string
	Stdin         io.Reader
	Stdout        io.Writer
	Stderr        io.Writer
	OriginNS      uint64
	Admin         Admin
	Keychain      Keychain
	GraphicAccess func() bool
}

type exit struct {
	code   int
	err    error
	key    string
	silent bool
}

func (e *exit) Error() string { return e.err.Error() }
func (e *exit) Unwrap() error { return e.err }

func usagef(format string, args ...any) error {
	return &exit{code: ExitUsage, err: fmt.Errorf(format, args...)}
}

func failf(code int, format string, args ...any) error {
	return &exit{code: code, err: fmt.Errorf(format, args...)}
}

var errNotRunning = &exit{code: ExitNotRunning, err: errors.New(`not running; run "fev start"`)}

type app struct {
	Env
	homeFlag     string
	format       string
	quiet        bool
	hello        *api.HelloResult
	ran          bool
	uninstalling bool
}

func (a *app) json() bool { return a.format == "json" }

func Main(e Env) int {
	a := &app{Env: e}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	root := a.root()
	if e.Name != "fevctl" {
		commands := map[string]bool{"help": true, "completion": true, cobra.ShellCompRequestCmd: true, cobra.ShellCompNoDescRequestCmd: true}
		for _, c := range root.Commands() {
			commands[c.Name()] = true
			for _, alias := range c.Aliases {
				commands[alias] = true
			}
		}
		e.Args = fallThrough(e.Args, commands)
	}
	root.SetArgs(e.Args)
	root.SetIn(e.Stdin)
	root.SetOut(e.Stdout)
	root.SetErr(e.Stderr)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	var ex *exit
	if errors.As(err, &ex) && ex.silent {
		return ex.code
	}
	if ctx.Err() != nil {
		return ExitInterrupted
	}
	code, message, key := a.classify(err)
	if a.json() {
		data, _ := json.Marshal(struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Key     string `json:"key,omitempty"`
		}{code, message, key})
		fmt.Fprintln(e.Stderr, string(data))
	} else {
		fmt.Fprintf(e.Stderr, "fev: %s\n", message)
		if code == ExitUsage {
			fmt.Fprintf(e.Stderr, "Run \"%s --help\" for the commands.\n", e.Name)
		}
	}
	return code
}

func (a *app) classify(err error) (code int, message, key string) {
	var ex *exit
	var mismatch *version.Mismatch
	var rpc *jsonrpc.Error
	switch {
	case errors.As(err, &ex):
		return ex.code, ex.Error(), ex.key
	case !a.ran:
		return ExitUsage, err.Error(), ""
	case errors.As(err, &mismatch):
		return ExitUnreachable, err.Error(), ""
	case api.ConfigErrorOf(err) != nil:
		errors.As(err, &rpc)
		return ExitConfig, rpc.Message, api.ConfigErrorOf(err).Key
	case errors.As(err, &rpc):
		return ExitFailed, rpc.Message, ""
	}
	return ExitFailed, err.Error(), ""
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:   a.Name,
		Short: "Run Docker containers and Linux machines in one light VM",
		Long: `fev runs Docker containers and Linux machines in one light VM.

A docker command starts fev by itself. These commands manage it.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if a.format != "text" && a.format != "json" {
				return usagef("--format is text or json, not %q", a.format)
			}
			a.ran = true
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	root.CompletionOptions.HiddenDefaultCmd = false
	pf := root.PersistentFlags()
	pf.StringVar(&a.homeFlag, "home", "", "fev's directory (default $FEV_HOME, else ~/.fev)")
	pf.StringVar(&a.format, "format", "text", "output format: text or json")
	pf.BoolVarP(&a.quiet, "quiet", "q", false, "print nothing on success, or only the bare value")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usagef("%v", err) })
	root.AddCommand(
		a.startCmd(), a.stopCmd(), a.restartCmd(), a.statusCmd(), a.versionCmd(),
		a.configCmd(), a.resetCmd(), a.salvageCmd(), a.dataCmd(),
		a.portsCmd(), a.logsCmd(), a.caffeinateCmd(),
		a.setupCmd(), a.sshCmd(),
		a.runCmd(), a.createCmd(), a.listCmd(), a.infoCmd(), a.deleteCmd(),
		a.cloneCmd(), a.renameCmd(), a.defaultCmd(), a.exportCmd(), a.importCmd(),
		a.pushCmd(), a.pullCmd(),
		a.memoryCmd(),
		a.doctorCmd(),
		a.reportCmd(),
		a.topCmd(),
		a.dockerCmd(),
		a.debugCmd(),
		a.k8sCmd(),
		a.updateCmd(),
		a.uninstallCmd(),
	)
	return root
}

func args(check cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := check(cmd, args); err != nil {
			return usagef("%s: %v", cmd.CommandPath(), err)
		}
		return nil
	}
}

func (a *app) layout() (home.Layout, error) {
	return home.Resolve(a.homeFlag, "")
}

func (a *app) launchOptions(l home.Layout, autostart bool) launch.Options {
	o := launch.Options{Layout: l, Client: a.Name, OriginNS: a.OriginNS, Daemon: daemonBeside(), SSHAuthSock: os.Getenv("SSH_AUTH_SOCK"),
		Terminfo: os.Getenv("TERMINFO"), TerminfoDirs: os.Getenv("TERMINFO_DIRS")}
	if a.homeFlag != "" {
		o.Args = append(o.Args, "--home", l.Home)
	}
	if !autostart {
		o.Args = append(o.Args, "--no-autostart")
	}
	o.Args = append(o.Args, strings.Fields(os.Getenv(DaemonArgsEnv))...)
	return o
}

func helloOptions() []api.ClientOption {
	return []api.ClientOption{
		api.WithSSHAuthSock(os.Getenv("SSH_AUTH_SOCK")),
		api.WithTerminfo(os.Getenv("TERMINFO"), os.Getenv("TERMINFO_DIRS")),
	}
}

func (a *app) connect(ctx context.Context) (*api.Client, error) {
	l, err := a.layout()
	if err != nil {
		return nil, err
	}
	conn, err := net.Dial("unix", l.Socket())
	if err != nil {
		return nil, errNotRunning
	}
	client, hello, err := api.NewClient(ctx, conn, a.Name, helloOptions()...)
	a.hello = hello
	if errors.Is(err, jsonrpc.ErrClosed) {
		return nil, errNotRunning
	}
	return client, err
}

func (a *app) withDaemon(ctx context.Context, autostart bool, fn func(*api.Client) error) error {
	l, err := a.layout()
	if err != nil {
		return err
	}
	if err := l.Validate(); err != nil {
		return failf(ExitUnreachable, "%v", err)
	}
	if err := os.Remove(api.StopMarkPath(l.Home)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return failf(ExitUnreachable, "cannot remove the stop mark: %v", err)
	}
	opts := a.launchOptions(l, autostart)
	client, _, err := launch.Dial(ctx, opts)
	if data, ok := replaceable(err); ok {
		if data.State != api.StateStopped {
			opts = a.launchOptions(l, true)
		}
		if err := a.replaceDaemon(ctx, l, opts, data); err != nil {
			return err
		}
		client, _, err = launch.Dial(ctx, opts)
	}
	if err != nil {
		return unreachable(err)
	}
	err = fn(client)
	if api.IsShuttingDown(err) {
		client.Close()
		if client, _, err = launch.Again(ctx, opts); err != nil {
			return unreachable(err)
		}
		err = fn(client)
	}
	client.Close()
	return err
}

func unreachable(err error) error {
	var rpc *jsonrpc.Error
	var mismatch *version.Mismatch
	if errors.As(err, &mismatch) || errors.As(err, &rpc) {
		return err
	}
	return &exit{code: ExitUnreachable, err: err}
}

func daemonBeside() string {
	if os.Getenv(launch.DaemonEnv) != "" {
		return ""
	}
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return ""
	}
	return daemonNear(self)
}

func daemonNear(self string) string { return bundle.Peer(self, "fevd") }
