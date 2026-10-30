package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"fev/host/daemon/doctor"
	"fev/host/daemon/home"
	"fev/host/docker/dockerctx"
	"fev/host/docker/tools"
	api "fev/proto/daemon"
)

func (a *app) doctorCmd() *cobra.Command {
	var fix bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Find what keeps fev or its docker from working, and fix what can be",
		Long: `Look for what stands between you and a working fev on this Mac:

  - another docker ahead of fev's on PATH, or fev's not on PATH;
  - DOCKER_HOST, DOCKER_CONTEXT or the current Docker context sending
    docker commands elsewhere, or a context whose socket is gone;
  - /var/run/docker.sock pointing at nothing or at another engine;
  - sockets a daemon that is gone left behind, a daemon that does not
    answer, and fev's links and tools missing;
  - what the running daemon warns of: a Docker engine that failed, a data
    disk that needed a recovery to mount, and its other warnings.

Each finding says what to do. --fix makes the fixes fev can make: it
removes stale sockets, runs fev setup's steps again (links, tools,
PATH), makes the fev Docker context the current one, and checks every
block of a recovered data disk (storage.check). It never edits a line you
wrote, never changes the running shell's variables, and never uses sudo:
those are said, not done.

Exit status: 0 when no problem is left, 1 when one is. With -q only
problems are printed, and nothing when there are none.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.doctor(cmd.Context(), fix)
		},
	}
	cmd.Flags().BoolVarP(&fix, "fix", "f", false, "make the fixes fev can make")
	return cmd
}

var systemDockerSock = "/var/run/docker.sock"

func (a *app) doctorEnv(ctx context.Context, l home.Layout) (doctor.Env, *api.Client) {
	userHome, _ := os.UserHomeDir()
	self, _ := os.Executable()
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	e := doctor.Env{Layout: l, UserHome: userHome, Self: self, Tools: toolsDir(l), LinkNames: selfNames, DockerSock: systemDockerSock}
	if lock, err := tools.Lock(); err == nil {
		for _, t := range lock {
			e.ToolNames = append(e.ToolNames, t.Name)
		}
	}
	if _, err := os.Stat(setupState(l)); err == nil {
		e.SetUp = true
	}
	c, err := a.connect(ctx)
	if err != nil {
		return e, nil
	}
	e.Answered = true
	if st, err := c.Status(ctx); err == nil {
		e.Status = st
	}
	return e, c
}

func (a *app) doctor(ctx context.Context, fix bool) error {
	l, err := a.layout()
	if err != nil {
		return err
	}
	e, c := a.doctorEnv(ctx, l)
	if c != nil {
		defer c.Close()
	}
	report := doctor.Check(e)
	var made []doctor.Finding
	var checked *api.Operation
	if fix {
		made, checked = a.doctorFix(ctx, l, c, report)
		if len(made) > 0 {
			if c != nil {
				c.Close()
			}
			e, c = a.doctorEnv(ctx, l)
			if c != nil {
				defer c.Close()
			}
			report = doctor.Check(e)
		}
	}
	out := struct {
		doctor.Report
		Fixed []doctor.Finding `json:"fixed,omitempty"`
		Check *api.Operation   `json:"check,omitempty"`
	}{report, made, checked}
	if a.json() {
		if err := a.printJSON(out); err != nil {
			return err
		}
	} else {
		a.doctorText(a.Stdout, report, made, checked)
	}
	if n := report.Problems(); n > 0 {
		return &exit{code: ExitFailed, err: fmt.Errorf("%d problem(s)", n), silent: true}
	}
	return nil
}

func (a *app) doctorFix(ctx context.Context, l home.Layout, c *api.Client, r doctor.Report) ([]doctor.Finding, *api.Operation) {
	var made []doctor.Finding
	var checked *api.Operation
	done := map[string]bool{}
	say := func(format string, args ...any) {
		if !a.quiet && !a.json() {
			fmt.Fprintf(a.Stderr, "fev: doctor: "+format+"\n", args...)
		}
	}
	for _, f := range r.Findings {
		if f.Fix == "" || done[f.Fix] {
			continue
		}
		done[f.Fix] = true
		var err error
		switch f.Fix {
		case doctor.FixSockets:
			var removed []string
			removed, err = doctor.RemoveStaleSockets(l)
			for _, p := range removed {
				say("removed %s", p)
			}
		case doctor.FixSetup:
			err = a.doctorSetup(ctx, l, say)
		case doctor.FixContext:
			var dir string
			if dir, err = dockerctx.Dir(); err == nil {
				s := dockerctx.Store{Dir: dir, Socket: filepath.Join(l.RunDir(), "docker.sock"), StateFile: filepath.Join(filepath.Dir(l.ConfigFile()), "docker-context.json")}
				if err = s.Use(); err == nil {
					say("the fev Docker context is the current one in %s", dir)
				}
			}
		case doctor.FixCheck:
			if c == nil {
				err = errNotRunning
				break
			}
			say("checking every block of the data disk; this reads all of it")
			checked, err = a.storageCheck(ctx, c)
		}
		f.Fixed = err == nil
		if err != nil {
			f.FixError = err.Error()
			say("%s: %v", f.Fix, err)
		}
		made = append(made, f)
	}
	return made, checked
}

func (a *app) doctorSetup(ctx context.Context, l home.Layout, say func(string, ...any)) error {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	shell := filepath.Base(os.Getenv("SHELL"))
	if _, err := profileFor(shell, userHome); err != nil {
		return fmt.Errorf("cannot tell the shell from SHELL=%q; run fev setup --shell zsh, bash or fish", os.Getenv("SHELL"))
	}
	quiet := func(format string, args ...any) { say(format, args...) }
	return a.setUp(ctx, l, userHome, []string{shell}, false, true, "", quiet)
}

func (a *app) storageCheck(ctx context.Context, c *api.Client) (*api.Operation, error) {
	ops := &operations{early: map[string][]api.Operation{}}
	_, unsubscribe, err := c.Subscribe(ctx, api.Events{Operation: ops.seen})
	if err != nil {
		return nil, err
	}
	defer unsubscribe()
	op, err := c.StorageCheck(ctx, api.StorageCheckParams{})
	if err != nil {
		return nil, err
	}
	select {
	case end := <-ops.wait(op.ID):
		if end.State != api.OperationDone {
			why := end.Error
			if why == "" {
				why = end.State
			}
			return &end, fmt.Errorf("the check %s", why)
		}
		return &end, nil
	case <-c.Done():
		return nil, fmt.Errorf("the daemon's connection ended during the check")
	case <-ctx.Done():
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c.OperationCancel(cctx, op.ID)
		return nil, ctx.Err()
	}
}

func (a *app) doctorText(w io.Writer, r doctor.Report, made []doctor.Finding, checked *api.Operation) {
	if !a.quiet {
		for _, f := range made {
			if f.Fixed {
				fmt.Fprintf(w, "fixed: %s\n", fixText(f.Fix))
			} else {
				fmt.Fprintf(w, "not fixed: %s: %s\n", fixText(f.Fix), f.FixError)
			}
		}
	}
	if checked != nil && checked.State == api.OperationDone {
		var res api.StorageChecked
		if json.Unmarshal(checked.Result, &res) == nil {
			fmt.Fprintf(w, "data disk checked: %s read, %d sector(s) repaired, %d that no copy could repair%s\n",
				bytesText(res.ScrubbedBytes), res.Corrected, res.Uncorrectable,
				map[bool]string{true: " (read-only: nothing was written)"}[res.ReadOnly])
		}
	}
	problems, warnings := 0, 0
	for _, f := range r.Findings {
		switch f.Severity {
		case doctor.Problem:
			problems++
		case doctor.Warning:
			warnings++
		}
		if a.quiet && f.Severity != doctor.Problem {
			continue
		}
		fmt.Fprintf(w, "%s: %s\n", f.Severity, f.Text)
		if f.Advice != "" {
			fmt.Fprintf(w, "  %s\n", f.Advice)
		}
		if f.Fix != "" {
			fmt.Fprintf(w, "  --fix %s\n", fixText(f.Fix))
		}
	}
	if a.quiet {
		return
	}
	switch {
	case len(r.Findings) == 0:
		fmt.Fprintln(w, "Nothing found: fev and its docker are set up as they should be.")
	case problems == 0 && warnings == 0:
		fmt.Fprintln(w, "No problem found.")
	default:
		var parts []string
		if problems > 0 {
			parts = append(parts, fmt.Sprintf("%d problem(s)", problems))
		}
		if warnings > 0 {
			parts = append(parts, fmt.Sprintf("%d warning(s)", warnings))
		}
		fmt.Fprintln(w, strings.Join(parts, ", ")+".")
	}
}

func fixText(fix string) string {
	switch fix {
	case doctor.FixSockets:
		return "removes the stale sockets"
	case doctor.FixSetup:
		return "runs fev setup's steps again"
	case doctor.FixContext:
		return "makes the fev Docker context the current one"
	case doctor.FixCheck:
		return "checks the data disk"
	}
	return fix
}
