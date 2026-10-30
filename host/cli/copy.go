package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	api "fev/proto/daemon"
)

func (a *app) pushCmd() *cobra.Command {
	var machine string
	cmd := &cobra.Command{
		Use:   "push [flags] MAC-PATH... [LINUX-DEST]",
		Short: "Copy files from the Mac into a Linux machine",
		Long: `Copy Mac files or folders into a Linux machine, the default one unless -m
names another, as its default user: into the user's home, or into
LINUX-DEST when more than one path is given (a relative LINUX-DEST is in
the home). A file already there is replaced; a folder is copied whole.`,
		Args: args(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			srcs, dest := argv, ""
			if len(argv) > 1 {
				srcs, dest = argv[:len(argv)-1], argv[len(argv)-1]
			}
			var from, paths []string
			for _, s := range srcs {
				abs, err := filepath.Abs(s)
				if err != nil {
					return usagef("push: %v", err)
				}
				if _, err := os.Lstat(abs); err != nil {
					return failf(ExitFailed, "push: %v", err)
				}
				from = append(from, shellQuote(linuxPath(abs), false))
				paths = append(paths, abs)
			}
			to := `"$HOME"/`
			if dest != "" {
				to = linuxDest(dest)
			}
			return a.copyIn(cmd.Context(), "push", machine, from, to, func(ctx context.Context, c *api.Client) error {
				return a.pushTar(ctx, c, machine, paths, copyPath(dest, ""))
			})
		},
	}
	cmd.Flags().StringVarP(&machine, "machine", "m", "", "the machine, by its name exactly as written, case included (default: the default one)")
	return cmd
}

func (a *app) pullCmd() *cobra.Command {
	var machine string
	cmd := &cobra.Command{
		Use:   "pull [flags] LINUX-PATH... [MAC-DEST]",
		Short: "Copy files from a Linux machine to the Mac",
		Long: `Copy files or folders of a Linux machine, the default one unless -m names
another, to the Mac: into the working directory, or into MAC-DEST when
more than one path is given. A relative LINUX-PATH, and one under your Mac
home's path or ~, is in the Linux user's home. A file already there is
replaced; a folder is copied whole.`,
		Args: args(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			srcs, dest := argv, "."
			if len(argv) > 1 {
				srcs, dest = argv[:len(argv)-1], argv[len(argv)-1]
			}
			macHome, _ := os.UserHomeDir()
			var from, paths []string
			for _, s := range srcs {
				from = append(from, linuxSource(s, macHome))
				paths = append(paths, copyPath(s, macHome))
			}
			abs, err := filepath.Abs(dest)
			if err != nil {
				return usagef("pull: %v", err)
			}
			if st, err := os.Stat(abs); err != nil || !st.IsDir() {
				if len(srcs) > 1 || err != nil && !os.IsNotExist(err) {
					return failf(ExitFailed, "pull: %s is not a folder", dest)
				}
			}
			return a.copyIn(cmd.Context(), "pull", machine, from, shellQuote(linuxPath(abs), false), func(ctx context.Context, c *api.Client) error {
				return a.pullTar(ctx, c, machine, paths, abs, len(srcs) > 1)
			})
		},
	}
	cmd.Flags().StringVarP(&machine, "machine", "m", "", "the machine, by its name exactly as written, case included (default: the default one)")
	return cmd
}

func linuxDest(dest string) string {
	switch {
	case dest == "~":
		return `"$HOME"/`
	case strings.HasPrefix(dest, "~/"):
		return `"$HOME"/` + shellQuote(dest[2:], false)
	case strings.HasPrefix(dest, "/"):
		return shellQuote(dest, false)
	}
	return `"$HOME"/` + shellQuote(dest, false)
}

func linuxSource(src, macHome string) string {
	switch {
	case src == "~" || src == macHome && macHome != "":
		return `"$HOME"`
	case strings.HasPrefix(src, "~/"):
		return `"$HOME"/` + shellQuote(src[2:], false)
	case macHome != "" && strings.HasPrefix(src, macHome+"/"):
		return `"$HOME"/` + shellQuote(strings.TrimPrefix(src, macHome+"/"), false)
	case strings.HasPrefix(src, "/"):
		return shellQuote(src, false)
	}
	return `"$HOME"/` + shellQuote(src, false)
}

func copyPath(p, macHome string) string {
	switch {
	case p == "" || p == "~" || macHome != "" && p == macHome:
		return "."
	case strings.HasPrefix(p, "~/"):
		return "./" + p[2:]
	case macHome != "" && strings.HasPrefix(p, macHome+"/"):
		return "./" + strings.TrimPrefix(p, macHome+"/")
	}
	return p
}

func (a *app) copyIn(ctx context.Context, what, machine string, from []string, to string, isolated func(context.Context, *api.Client) error) error {
	line := "cp -R -- " + strings.Join(from, " ") + " " + to
	p := api.SessionStartParams{Machine: machine, Login: true, Shell: true, Argv: []string{line}}
	var code int
	tarred := false
	err := a.withDaemon(ctx, false, func(c *api.Client) error {
		if isolatedMachine(ctx, c, machine) {
			tarred = true
			return isolated(ctx, c)
		}
		if share, err := c.ConfigGet(ctx, api.KeyFilesShare); err == nil {
			if v, ok := share[api.KeyFilesShare]; ok && v.Value == false {
				return failf(ExitFailed, "%s copies through the Mac's files in the machine, and file sharing is off (%s)", what, api.KeyFilesShare)
			}
		}
		e, err := a.session(ctx, c, p, sessionStreams{in: strings.NewReader(""), out: a.Stderr, errw: a.Stderr})
		if err != nil {
			return err
		}
		code, err = exitStatus(e)
		return err
	})
	if err != nil || tarred {
		return err
	}
	if code != 0 {
		return &exit{code: ExitFailed, err: fmt.Errorf("%s: cp in the machine exited with %d", what, code)}
	}
	return nil
}
