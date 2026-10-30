package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	api "fev/proto/daemon"
)

var distroAliases = map[string]string{"arch": "archlinux"}

func machineText(w io.Writer, m *api.Machine) {
	kv(w, "name", m.Name)
	kv(w, "state", m.State)
	kv(w, "distro", m.Image.Distro)
	kv(w, "version", m.Image.Release)
	kv(w, "arch", m.Image.Arch)
	if m.Emulation != "" {
		text := m.Emulation
		if m.EmulationReason != "" {
			text += ": " + m.EmulationReason
		}
		kv(w, "emulation", text)
	}
	if m.Image.Variant != "" {
		kv(w, "variant", m.Image.Variant)
	}
	if m.Image.Serial != "" {
		kv(w, "build", m.Image.Serial)
	}
	kv(w, "user", m.User)
	if m.Default {
		kv(w, "default", "yes")
	}
	if m.Address != "" {
		kv(w, "address", m.Address)
	}
	if m.Address6 != "" {
		kv(w, "address6", m.Address6)
	}
	kv(w, "disk usage", bytesText(m.DiskBytes))
	if m.Since != "" {
		kv(w, "since", m.Since)
	}
	if m.LastExit != "" && m.State != api.MachineRunning {
		kv(w, "last exit", m.LastExit)
	}
	if m.Error != "" {
		kv(w, "error", m.Error)
	}
}

func (a *app) formatFlag(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&a.format, "format", "f", "text", "output format: text or json")
}

func (a *app) listCmd() *cobra.Command {
	var names, running bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the Linux machines",
		Long: `List the Linux machines: each one's state, distribution, release,
architecture, the disk space it has written and its address.

It asks the daemon's record and starts no VM: with the VM stopped every
machine is stopped. -q prints the names alone, -r only the running ones.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				ms, err := c.Machines(cmd.Context())
				if err != nil {
					return err
				}
				if running {
					kept := ms.Machines[:0]
					for _, m := range ms.Machines {
						if m.State == api.MachineRunning {
							kept = append(kept, m)
						}
					}
					ms.Machines = kept
				}
				if names {
					for _, m := range ms.Machines {
						fmt.Fprintln(a.Stdout, m.Name)
					}
					return nil
				}
				return a.emit(ms, func(w io.Writer) {
					var rows [][]string
					for _, m := range ms.Machines {
						rows = append(rows, []string{m.Name, m.State, m.Image.Distro, m.Image.Release, m.Image.Arch, bytesText(m.DiskBytes), m.Address})
					}
					table(w, []string{"NAME", "STATE", "DISTRO", "VERSION", "ARCH", "SIZE", "ADDRESS"}, rows)
				})
			})
		},
	}
	a.formatFlag(cmd)
	cmd.Flags().BoolVarP(&names, "quiet", "q", false, "print the names alone")
	cmd.Flags().BoolVarP(&running, "running", "r", false, "only the running machines")
	return cmd
}

func (a *app) infoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "info [MACHINE]",
		Short: "Describe a Linux machine",
		Long: `Describe a Linux machine, the default one if none is named: its state,
image, default user, address and the disk space it has written (a clone
counts what the clone wrote). It starts no VM.`,
		Args: args(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				name, err := a.machineName(cmd.Context(), c, argv)
				if err != nil {
					return err
				}
				m, err := c.Machine(cmd.Context(), name)
				if err != nil {
					return err
				}
				return a.emit(m, func(w io.Writer) { machineText(w, m) })
			})
		},
	}
	a.formatFlag(cmd)
	return cmd
}

func (a *app) machineName(ctx context.Context, c *api.Client, argv []string) (string, error) {
	if len(argv) > 0 {
		return argv[0], nil
	}
	ms, err := c.Machines(ctx)
	if err != nil {
		return "", err
	}
	if ms.Default == "" {
		return "", failf(ExitFailed, `there is no default machine: name one, or make one with "fev create"`)
	}
	return ms.Default, nil
}

type refusedFlag struct {
	name, why string
}

func (a *app) createCmd() *cobra.Command {
	var user, userData, arch string
	var password, isolated, isolateNet, forwardAgent bool
	var mounts []string
	var cpus, memory, disk string
	var emulation string
	cmd := &cobra.Command{
		Use:   "create [flags] DISTRO[:VERSION] [NAME]",
		Short: "Make a Linux machine",
		Long: `Make a Linux machine from a distribution's image and start it: ubuntu,
debian, fedora, alpine, arch, almalinux, rockylinux, centos, oracle,
opensuse, kali, devuan, gentoo or voidlinux, at fev's default release or
the VERSION named (ubuntu:noble, debian:12, fedora:43). NAME is the distribution's name
when not given. The first machine made becomes the default.

The image is fetched from the LXC image server and checked against its
signature on the Mac. The machine's default user has your Mac user's name
(or -u's) and uid, sudo without a password, and no password unless -p asks
for one; the password is sent as a crypt string, never itself.

-c FILE gives cloud-init user data: the image's cloud variant is used, and
create waits for cloud-init to finish. If cloud-init reports an error the
machine is left running, the end of its output is printed, and the exit
status is 1. Arch has no arm64 cloud variant and refuses -c (its amd64 build has one).

-a amd64 makes an amd64 machine from the image's amd64 build: its whole
userland, init included, runs under fev's x86 translator, slower than
an arm64 machine (arm64, the Mac's own, is the default).

--isolated makes an isolated machine: a user namespace of its own, no Mac
files, no mac command, no vsock and no Docker socket. --mount SRC[:DEST]
gives it a Mac folder (SRC an absolute path on the Mac, DEST where it
appears, the same path by default; more than once for more),
--forward-ssh-agent the Mac's SSH agent, and --isolate-network a network of
its own: the internet only, no other machine, container or the Mac. They
are the machine's settings (machine.NAME.isolated, mounts,
forward_ssh_agent, isolate_network), applied at each start. Isolation is a
boundary against code that is not trying to break out, not against a
kernel exploit: every machine shares one kernel.`,
		Args: args(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			if emulation != "" && emulation != "auto" && emulation != "fex" && emulation != "rosetta" {
				return usagef("--emulation must be auto, fex or rosetta")
			}
			for _, r := range []refusedFlag{
				{"cpus", "machines share the VM's limits for now"},
				{"memory", "machines share the VM's limits for now"},
				{"disk", "machines share the VM's limits for now"},
			} {
				if cmd.Flags().Changed(r.name) {
					return usagef("create --%s is not built yet: %s", r.name, r.why)
				}
			}
			switch strings.ToLower(arch) {
			case "", "arm64", "aarch64":
				arch = ""
			case "amd64", "x86_64":
				arch = "amd64"
			default:
				return usagef("create --arch: no architecture %q: arm64 (the Mac's own) or amd64 (under x86 emulation)", arch)
			}
			if emulation != "" && emulation != "auto" && arch != "amd64" {
				return usagef("create --emulation is for amd64 machines (-a amd64): an arm64 machine runs natively")
			}
			distro, release, _ := strings.Cut(argv[0], ":")
			name := distro
			if len(argv) == 2 {
				name = argv[1]
			}
			if d, ok := distroAliases[distro]; ok {
				distro = d
			}
			if !isolated {
				for _, f := range []string{"isolate-network", "forward-ssh-agent", "mount"} {
					if cmd.Flags().Changed(f) {
						return usagef("create --%s is for an isolated machine: add --isolated", f)
					}
				}
			}
			for i, m := range mounts {
				src, dest, _ := strings.Cut(m, ":")
				if !filepath.IsAbs(src) {
					abs, err := filepath.Abs(src)
					if err != nil {
						return usagef("create --mount %s: %v", m, err)
					}
					src = abs
				}
				mounts[i] = src
				if dest != "" {
					mounts[i] += ":" + dest
				}
			}
			p := api.MachinesCreateParams{Name: name, Distro: distro, Release: release, Arch: arch, User: user, Emulation: emulation,
				Isolated: isolated, IsolateNetwork: isolateNet, ForwardSSHAgent: forwardAgent, Mounts: mounts}
			if userData != "" {
				data, err := readUserData(userData, a.Stdin)
				if err != nil {
					return err
				}
				p.UserData = data
			}
			if password {
				hash, err := a.askPassword()
				if err != nil {
					return err
				}
				p.PasswordHash = hash
			}
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				return a.create(cmd.Context(), c, p)
			})
		},
	}
	f := cmd.Flags()
	f.StringVarP(&user, "user", "u", "", "the default user's name (default: your Mac user's)")
	f.BoolVarP(&password, "set-password", "p", false, "ask for a password for the default user")
	f.StringVarP(&userData, "user-data", "c", "", "cloud-init user data from this file (- for standard input)")
	f.StringVarP(&arch, "arch", "a", "", "the machine's architecture: arm64 (the Mac's own) or amd64 (under x86 emulation)")
	f.BoolVar(&isolated, "isolated", false, "an isolated machine: no Mac files, commands or sockets but what is named")
	f.StringVar(&emulation, "emulation", "", "amd64 translator: auto, fex or rosetta (default: follow emulation.engine)")
	f.BoolVar(&isolateNet, "isolate-network", false, "with --isolated: the internet only, nothing of fev's or the Mac's")
	f.BoolVar(&forwardAgent, "forward-ssh-agent", false, "with --isolated: the Mac's SSH agent in the machine")
	f.StringArrayVar(&mounts, "mount", nil, "with --isolated: a Mac folder SRC[:DEST] in the machine (more than once for more)")
	for _, l := range []struct{ name, usage string }{{"cpus", "CPUs"}, {"memory", "memory"}, {"disk", "disk space"}} {
		f.StringVar(map[string]*string{"cpus": &cpus, "memory": &memory, "disk": &disk}[l.name], l.name, "", "the machine's "+l.usage+" (not built yet)")
		f.MarkHidden(l.name)
	}
	return cmd
}

func readUserData(path string, stdin io.Reader) (string, error) {
	var r io.Reader = stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return "", usagef("create -c: %v", err)
		}
		defer f.Close()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, api.MaxUserDataBytes+1))
	if err != nil {
		return "", usagef("create -c: %v", err)
	}
	if len(data) > api.MaxUserDataBytes {
		return "", usagef("create -c: %s is larger than cloud-init's %d bytes", path, api.MaxUserDataBytes)
	}
	return string(data), nil
}

func (a *app) askPassword() (string, error) {
	pw, err := readSecret(a.Stdin, a.Stderr, "Password for the machine's user: ")
	if err != nil {
		return "", usagef("create -p: %v", err)
	}
	if isTerminal(a.Stdin) {
		again, err := readSecret(a.Stdin, a.Stderr, "Again: ")
		if err != nil {
			return "", usagef("create -p: %v", err)
		}
		if again != pw {
			return "", usagef("create -p: the two passwords differ")
		}
	}
	if pw == "" {
		return "", usagef("create -p: an empty password; leave -p out for none")
	}
	salt, err := newSalt()
	if err != nil {
		return "", err
	}
	return sha512Crypt(pw, salt), nil
}

func (a *app) follow(ctx context.Context, c *api.Client, what string, start func() (*api.Operation, error)) (api.Operation, error) {
	ops := &operations{early: map[string][]api.Operation{}}
	if !a.quiet && !a.json() {
		phase := ""
		ops.moved = func(op api.Operation) {
			if op.Phase != "" && op.Phase != phase {
				phase = op.Phase
				fmt.Fprintf(a.Stderr, "fev: %s: %s\n", what, strings.ReplaceAll(phase, "_", " "))
			}
		}
	}
	_, unsubscribe, err := c.Subscribe(ctx, api.Events{Operation: ops.seen})
	if err != nil {
		return api.Operation{}, err
	}
	defer unsubscribe()
	op, err := start()
	if err != nil {
		return api.Operation{}, err
	}
	select {
	case end := <-ops.wait(op.ID):
		return end, nil
	case <-c.Done():
		return api.Operation{}, failf(ExitUnreachable, "%s: the daemon's connection ended before it did", what)
	case <-ctx.Done():
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c.OperationCancel(cctx, op.ID)
		return api.Operation{}, ctx.Err()
	}
}

func ended(what string, op api.Operation) error {
	if op.State == api.OperationDone {
		return nil
	}
	why := op.Error
	if why == "" {
		why = op.State
	}
	return failf(ExitFailed, "%s: %s", what, why)
}

func (a *app) create(ctx context.Context, c *api.Client, p api.MachinesCreateParams) error {
	end, err := a.follow(ctx, c, "create "+p.Name, func() (*api.Operation, error) { return c.CreateMachine(ctx, p) })
	if err != nil {
		return err
	}
	if err := ended("create "+p.Name, end); err != nil {
		return err
	}
	var made api.MachineCreated
	if err := json.Unmarshal(end.Result, &made); err != nil {
		return fmt.Errorf("create: the result: %w", err)
	}
	if err := a.emit(end, func(w io.Writer) { machineText(w, &made.Machine) }); err != nil {
		return err
	}
	if ci := made.CloudInit; ci != nil && ci.Status == "error" {
		if !a.json() {
			fmt.Fprintf(a.Stderr, "fev: create %s: cloud-init reported errors; the machine is running\n", p.Name)
			for _, e := range ci.Errors {
				fmt.Fprintf(a.Stderr, "  %s\n", e)
			}
			if len(ci.OutputTail) > 0 {
				fmt.Fprintln(a.Stderr, "the end of /var/log/cloud-init-output.log:")
				for _, l := range ci.OutputTail {
					fmt.Fprintf(a.Stderr, "  %s\n", l)
				}
			}
		}
		return &exit{code: ExitFailed, err: fmt.Errorf("create %s: cloud-init reported errors", p.Name), silent: !a.json()}
	}
	if ci := made.CloudInit; ci != nil && len(ci.RecoverableErrors) > 0 && !a.json() {
		fmt.Fprintf(a.Stderr, "fev: create %s: warning: cloud-init finished with recoverable errors:\n", p.Name)
		for _, e := range ci.RecoverableErrors {
			fmt.Fprintf(a.Stderr, "  %s\n", e)
		}
	}
	return nil
}

func (a *app) confirm(question, flag string) (bool, error) {
	if !isTerminal(a.Stdin) {
		return false, usagef("%s: no terminal to ask on; %s does it without asking", question, flag)
	}
	fmt.Fprintf(a.Stderr, "%s [y/N] ", question)
	line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

func (a *app) deleteCmd() *cobra.Command {
	var force, all bool
	cmd := &cobra.Command{
		Use:   "delete [flags] MACHINE...",
		Short: "Delete Linux machines and their files",
		Long: `Delete Linux machines: each is stopped if it runs, and its files and
settings removed. It asks first on a terminal; -f does not ask, and is
needed without a terminal. -a deletes every machine.`,
		Args: args(func(cmd *cobra.Command, argv []string) error {
			if all == (len(argv) > 0) {
				return errors.New("name the machines, or -a for all, not both")
			}
			return nil
		}),
		RunE: func(cmd *cobra.Command, argv []string) error {
			ctx := cmd.Context()
			return a.withDaemon(ctx, false, func(c *api.Client) error {
				names := argv
				if all {
					ms, err := c.Machines(ctx)
					if err != nil {
						return err
					}
					names = nil
					for _, m := range ms.Machines {
						names = append(names, m.Name)
					}
					if len(names) == 0 {
						return nil
					}
				}
				if !force {
					ok, err := a.confirm(fmt.Sprintf("Delete %s and all of %s files?", strings.Join(names, ", "), map[bool]string{true: "its", false: "their"}[len(names) == 1]), "-f")
					if err != nil {
						return err
					}
					if !ok {
						return failf(ExitFailed, "nothing deleted")
					}
				}
				for _, name := range names {
					if err := c.DeleteMachine(ctx, api.MachinesStopParams{Name: name}); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "delete without asking")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "delete every machine")
	return cmd
}

func (a *app) cloneCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clone MACHINE NEW",
		Short: "Copy a Linux machine",
		Long: `Copy a Linux machine, running or not, as a stopped machine NEW that shares
its files until either changes them. The copy gets an identity of its own:
host name, machine-id, address, SSH host keys.`,
		Args: args(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				m, err := c.CloneMachine(cmd.Context(), argv[0], argv[1])
				if err != nil {
					return err
				}
				return a.emit(m, func(w io.Writer) { machineText(w, m) })
			})
		},
	}
}

func (a *app) renameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename MACHINE NEW",
		Short: "Rename a Linux machine",
		Long: `Rename a Linux machine, running or not, and its settings with it. A running
machine answers to the new name at once and takes it as its host name at
its next start.`,
		Args: args(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				m, err := c.RenameMachine(cmd.Context(), argv[0], argv[1])
				if err != nil {
					return err
				}
				return a.emit(m, func(w io.Writer) { machineText(w, m) })
			})
		},
	}
}

func (a *app) defaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "default [MACHINE]",
		Short: "Print the default Linux machine, or make one the default",
		Long: `Print the default Linux machine: the one "fev", "fev run" and "ssh
fev" reach when none is named. With MACHINE, make that one the default.`,
		Args: args(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				var ms *api.Machines
				var err error
				if len(argv) == 1 {
					ms, err = c.SetDefaultMachine(cmd.Context(), argv[0])
				} else {
					ms, err = c.Machines(cmd.Context())
				}
				if err != nil {
					return err
				}
				if ms.Default == "" {
					return failf(ExitFailed, `there is no default machine; "fev default MACHINE" makes one`)
				}
				if a.json() {
					return a.printJSON(ms)
				}
				if len(argv) == 0 || !a.quiet {
					fmt.Fprintln(a.Stdout, ms.Default)
				}
				return nil
			})
		},
	}
}

func (a *app) exportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export MACHINE FILE",
		Short: "Write a Linux machine to an archive",
		Long: `Write a Linux machine, running or not, to FILE on the Mac: a zstd tar of its
files and settings, which "fev import" makes a machine of. An existing
FILE is not replaced.`,
		Args: args(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			path, err := filepath.Abs(argv[1])
			if err != nil {
				return usagef("export: %v", err)
			}
			ctx := cmd.Context()
			return a.withDaemon(ctx, false, func(c *api.Client) error {
				what := "export " + argv[0]
				end, err := a.follow(ctx, c, what, func() (*api.Operation, error) {
					return c.ExportMachine(ctx, api.MachinesExportParams{Name: argv[0], Path: path})
				})
				if err != nil {
					return err
				}
				if err := ended(what, end); err != nil {
					return err
				}
				var r api.MachineExported
				json.Unmarshal(end.Result, &r)
				return a.emit(end, func(w io.Writer) {
					kv(w, "archive", r.Path)
					kv(w, "size", bytesText(r.Bytes))
				})
			})
		},
	}
}

func (a *app) importCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "import [flags] FILE",
		Short: "Make a Linux machine from an archive",
		Long: `Make a stopped Linux machine from FILE, an archive "fev export" wrote,
under the name it was exported from or -n's. The machine takes the new
name as its host name.`,
		Args: args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			path, err := filepath.Abs(argv[0])
			if err != nil {
				return usagef("import: %v", err)
			}
			if _, err := os.Stat(path); err != nil {
				return failf(ExitFailed, "import: %v", err)
			}
			ctx := cmd.Context()
			return a.withDaemon(ctx, false, func(c *api.Client) error {
				end, err := a.follow(ctx, c, "import", func() (*api.Operation, error) {
					return c.ImportMachine(ctx, api.MachinesImportParams{Path: path, Name: name})
				})
				if err != nil {
					return err
				}
				if err := ended("import", end); err != nil {
					return err
				}
				var m api.Machine
				json.Unmarshal(end.Result, &m)
				return a.emit(end, func(w io.Writer) { machineText(w, &m) })
			})
		},
	}
	cmd.Flags().StringVarP(&name, "name", "n", "", "the new machine's name (default: the one it was exported from)")
	return cmd
}

func machinesFor(ctx context.Context, c *api.Client, names []string, all bool, want func(api.Machine) bool) ([]string, error) {
	if !all {
		return names, nil
	}
	ms, err := c.Machines(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range ms.Machines {
		if want(m) {
			out = append(out, m.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (a *app) startMachines(ctx context.Context, names []string, all bool) error {
	return a.withDaemon(ctx, false, func(c *api.Client) error {
		names, err := machinesFor(ctx, c, names, all, func(m api.Machine) bool { return m.State != api.MachineRunning })
		if err != nil {
			return err
		}
		a.resuming()
		for _, name := range names {
			m, err := c.StartMachine(ctx, name)
			if err != nil {
				return err
			}
			if a.json() {
				a.printJSON(m)
			}
		}
		return nil
	})
}

func (a *app) stopMachines(ctx context.Context, names []string, all, force bool, timeout int) error {
	return a.withDaemon(ctx, false, func(c *api.Client) error {
		names, err := machinesFor(ctx, c, names, all, func(m api.Machine) bool { return m.State != api.MachineStopped })
		if err != nil {
			return err
		}
		for _, name := range names {
			m, err := c.StopMachine(ctx, api.MachinesStopParams{Name: name, Force: force, GraceMS: timeout * 1000})
			if err != nil {
				return err
			}
			if a.json() {
				a.printJSON(m)
			}
		}
		return nil
	})
}

func (a *app) restartMachines(ctx context.Context, names []string, all bool) error {
	return a.withDaemon(ctx, false, func(c *api.Client) error {
		names, err := machinesFor(ctx, c, names, all, func(m api.Machine) bool { return m.State == api.MachineRunning })
		if err != nil {
			return err
		}
		a.resuming()
		for _, name := range names {
			m, err := c.RestartMachine(ctx, api.MachinesStopParams{Name: name})
			if err != nil {
				return err
			}
			if a.json() {
				a.printJSON(m)
			}
		}
		return nil
	})
}
