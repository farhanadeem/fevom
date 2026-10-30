package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"fev/host/daemon/boottrace"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	priv "fev/proto/privhelper"
	"fev/proto/version"
)

func stateText(st *api.Status) string {
	switch {
	case st.State == api.StatePaused && st.PauseReason == api.PauseSleep:
		return "running (paused for sleep)"
	case st.State == api.StatePaused:
		return "running (paused)"
	case st.State == api.StateStarting && st.Phase == api.PhaseBackoff:
		return fmt.Sprintf("starting (waiting %.1f s to try again)", float64(st.RetryInMS)/1000)
	case st.State == api.StateStarting && st.Phase != "":
		return fmt.Sprintf("starting (%s)", st.Phase)
	}
	return st.State
}

func statusText(w io.Writer, st *api.Status) {
	kv(w, "state", stateText(st))
	kv(w, "engine", st.Engine)
	if st.StopRequested {
		kv(w, "stopped by", `"fev stop"; "fev start" starts it again`)
	}
	if st.Since != "" {
		kv(w, "since", st.Since)
	}
	if st.BootID != "" {
		kv(w, "boot", st.BootID)
	}
	if st.Fake {
		kv(w, "vm", "a stand-in (--fake-vm)")
	}
	if st.Windows != nil && st.Windows.DataDisk != nil {
		kv(w, "read window", windowText(st.Windows.DataDisk))
	}
	if st.Data != nil && st.Data.Budget != nil {
		kv(w, "data", budgetText(st.Data.Budget))
	}
	if st.Network != nil {
		kv(w, "direct access", directText(st.Network))
	}
	if st.Domains != nil {
		kv(w, "names", namesText(st.Domains))
		for _, l := range st.Domains.Local {
			kv(w, "name "+l.Name, localNameText(l))
		}
	}
	if st.HTTPS != nil {
		kv(w, "https", httpsLine(st.HTTPS))
	}
	if st.View != nil {
		kv(w, "view", viewText(st.View))
	}
	if st.K8s != nil {
		kv(w, "kubernetes", k8sLine(st.K8s))
	}
	if st.Emulation != nil {
		kv(w, "x86 emulation", emulationText(st.Emulation))
		for _, choice := range st.Emulation.Workloads {
			kv(w, "  "+choice.Kind+" "+choice.Name, choice.Engine+": "+choice.Reason)
		}
	}
	if st.Debug != nil && st.Debug.Exists {
		kv(w, "debug tools", debugLineText(st.Debug))
	}
	if st.Error != "" {
		kv(w, "error", st.Error)
	}
	if e := st.LastExit; e != nil && !e.Expected {
		how := fmt.Sprintf("status %d", e.Code)
		if e.Signal != "" {
			how = e.Signal
		}
		kv(w, "last exit", fmt.Sprintf("unexpected, %s at %s", how, e.At))
	}
	for _, key := range st.PendingRestart {
		kv(w, "warning", fmt.Sprintf(`%s has a new value that waits for "fev restart"`, key))
	}
	for _, warning := range st.Warnings {
		kv(w, "warning", warning)
	}
}

func debugLineText(d *api.DebugLine) string {
	s := fmt.Sprintf("%s, %d packages", bytesText(d.Bytes), d.Packages)
	if d.Packages == 1 {
		s = fmt.Sprintf("%s, 1 package", bytesText(d.Bytes))
	}
	if d.Sessions == 1 {
		s += ", 1 session open"
	} else if d.Sessions > 1 {
		s += fmt.Sprintf(", %d sessions open", d.Sessions)
	}
	return s
}

func budgetText(b *api.DataBudget) string {
	if !b.Limited {
		return fmt.Sprintf("%s used, with no limit set from the Mac's free space", bytesText(b.ReferencedBytes))
	}
	return fmt.Sprintf("%s used, %s more can be written (the Mac's free space less what fev keeps free; df in a container shows it)",
		bytesText(b.ReferencedBytes), bytesText(b.BudgetBytes))
}

func directText(n *api.NetworkStatus) string {
	if n.Via == api.ViaVmnet && n.DirectAccess != api.DirectOff {
		text := fmt.Sprintf("on, %s reached through %s with no admin helper", n.Subnet, n.Interface)
		if n.DirectAccess == api.DirectDegraded {
			text += " (but a route of this Mac's is in the way of some: see the warnings)"
		}
		return text
	}
	switch n.DirectAccess {
	case api.DirectOn:
		nets := n.Subnet
		if n.Address6 != "" && n.Subnet6 != "" {
			nets += " and " + n.Subnet6
		}
		text := fmt.Sprintf("on, %s routed to containers and machines through %s", nets, n.Interface)
		if len(n.NAT64) > 0 {
			text += fmt.Sprintf(", and as this network's NAT64 prefix %s makes them", strings.Join(n.NAT64, ", "))
		}
		return text
	case api.DirectDegraded:
		var off []string
		for _, c := range n.Conflicts {
			if c.Withdrawn {
				off = append(off, c.Prefix)
			}
		}
		return fmt.Sprintf("on through %s, but not for %s: a route of this Mac's is in the way (see the warnings)", n.Interface, strings.Join(off, ", "))
	}
	why := map[string]string{
		api.DirectSetting:   `turned off (fev config set network.direct_access true)`,
		api.DirectAdminOff:  `admin features off (fev config set admin.enabled true)`,
		api.DirectNoHelper:  `needs "sudo fev setup --admin" once`,
		api.DirectOutdated:  `the admin helper is out of date: "sudo fev setup --admin"`,
		api.DirectOtherUser: "the admin helper is another user's on this Mac",
		api.DirectHelperOff: "the admin helper is switched off (System Settings, General, Login Items)",
		api.DirectFailed:    "the admin helper failed (see the warnings)",
		api.DirectHelperHeld: `the admin helper holds fev's addresses while it is not used ` +
			`(fev config set admin.enabled true, or "sudo fev setup --admin --undo")`,
	}[n.Reason]
	if why == "" {
		why = n.Reason
	}
	return "off: " + why
}

func namesText(d *api.DomainsStatus) string {
	n := fmt.Sprintf("%d registered", d.Names)
	switch d.Resolver {
	case priv.StateFev:
		return fmt.Sprintf("*.fev.local answered on this Mac (127.0.0.1:%d), %s", d.Port, n)
	case priv.StateForeign:
		return "*.fev.local is not fev's on this Mac: another program's resolver is configured for fev.local (scutil --dns names it); " + n
	}
	if d.MDNS != "" {
		return fmt.Sprintf("*.fev.local answered on this Mac by mDNS on %s, %s", d.MDNS, n)
	}
	return fmt.Sprintf(`*.fev.local only inside the VM until "sudo fev setup --admin"; %s`, n)
}

func localNameText(l api.LocalNameStatus) string {
	switch l.State {
	case priv.NamePresent:
		return "answered on this Mac, for " + l.Container
	case api.LocalWaiting:
		return `for ` + l.Container + `, on this Mac once "sudo fev setup --admin" is done`
	case api.LocalMDNS:
		return "answered on this Mac by mDNS, for " + l.Container + " (a device on the network answering it too may be taken first)"
	}
	text := "not answered on this Mac, for " + l.Container
	if l.Reason != "" {
		text += ": " + l.Reason
	}
	return text
}

func viewText(v *api.FilesView) string {
	switch v.State {
	case api.ViewMounted:
		if v.Hidden {
			return v.Path + " mounted, hidden from Finder's sidebar and the desktop"
		}
		return v.Path + " mounted"
	case api.ViewMounting:
		return v.Path + " being mounted"
	case api.ViewFailed:
		return v.Path + " not mounted: " + v.Error
	}
	return v.Path + " not mounted (it is while the VM runs)"
}

func emulationText(e *api.EmulationStatus) string {
	var text string
	switch e.State {
	case api.EmulationOn:
		text = "on: x86-64 and i386 Linux programs run through " + strings.TrimSpace(e.Translator+" "+e.Version)
		if e.Translator == "Rosetta" {
			text = "on: x86-64 Linux programs run through Rosetta; i386 through FEX-Emu"
		}
		if e.Mode == "auto" {
			text = "on: automatic Rosetta/FEX selection per workload; i386 through FEX-Emu"
		}
		if e.Reason != "" {
			text += "; " + e.Reason
		}
		if e.HardwareOrdering {
			text += ", with the CPU's memory ordering"
		} else {
			text += ", with memory ordering in software (this kernel cannot switch it)"
		}
	case api.EmulationOff:
		text = `off: x86 programs fail with "exec format error"`
		if !e.Pending {
			text += " (fev config set emulation.x86 true)"
		}
	default:
		text = "unavailable: " + e.Error
	}
	if e.Pending {
		text += `; emulation.x86's new value takes effect when the engine next starts ("fev restart docker")`
	}
	if e.EnginePending {
		text += `; emulation.engine's new value takes effect when the VM next starts ("fev restart")`
	}
	return text
}

func httpsLine(h *api.HTTPSStatus) string {
	if !h.Enabled {
		return "off (fev config set network.https true)"
	}
	text := map[string]string{
		api.TrustTrusted:   "on, fev's root trusted by this Mac",
		api.TrustUntrusted: `on, fev's root not trusted by this Mac: "fev setup --https"`,
		api.TrustAbsent:    `on, no root yet: "fev setup --https"`,
		api.TrustDeclined:  `on, fev's root not trusted (the dialog was cancelled): "fev setup --https"`,
	}[h.Trust]
	if h.Trust == api.TrustAbsent && h.Root != "" {
		text = `on, fev's root not in this Mac's keychain: "fev setup --https"`
	}
	if h.Key == api.KeyFile {
		text = "on, a development CA's root, which this Mac is never asked to trust"
	}
	if text == "" {
		text = h.Trust
	}
	switch h.Key {
	case api.KeySigner:
		text += "; its key in the login keychain, held by fev's key signer, never exportable"
	case api.KeyIdentity:
		text += "; its key in the login keychain, only for fev signed by " + h.Identity
	case api.KeyFile:
		text += "; its key in a file (--ca-dir)"
	case api.KeyUnusable:
		text += "; its key cannot be used by this build"
		if h.Identity != "" {
			text += " (bound to " + h.Identity + ")"
		}
		if !strings.Contains(h.Error, "fev setup --https") {
			text += `: "fev setup --https" replaces it with a new key and root`
		}
	}
	if h.Error != "" {
		text += "; " + h.Error
	}
	return text
}

func windowText(win *api.Window) string {
	if win.State == api.WindowOn {
		return fmt.Sprintf("on, %d GiB of the data disk", win.SizeBytes>>30)
	}
	why := ""
	if win.Reason != "" {
		why = " (" + win.Reason + ")"
	}
	return win.State + why + "; every read of the data disk is a request"
}

func (a *app) stoppedStatus() (*api.Status, error) {
	l, err := a.layout()
	if err != nil {
		return nil, err
	}
	st := &api.Status{State: api.StateStopped, Engine: api.EngineStopped, PendingRestart: []string{}}
	if _, err := os.Stat(api.StopMarkPath(l.Home)); err == nil {
		st.StopRequested = true
	}
	return st, nil
}

func (a *app) printStatus(st *api.Status) error {
	if a.quiet {
		_, err := fmt.Fprintln(a.Stdout, st.State)
		return err
	}
	return a.emit(st, func(w io.Writer) { statusText(w, st) })
}

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Say whether fev is running, and every warning it holds",
		Long: `Say whether fev is running, and every warning it holds.

It starts nothing. The exit status is 0 while the VM is running or on its
way up or down, and 3 while it is stopped. With -q only the state is
printed: stopped, starting, running, stopping or paused.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var st *api.Status
			client, err := a.connect(cmd.Context())
			switch {
			case err == errNotRunning:
				if st, err = a.stoppedStatus(); err != nil {
					return err
				}
			case err != nil:
				return unreachable(err)
			default:
				defer client.Close()
				if st, err = client.Status(cmd.Context()); err != nil {
					return err
				}
			}
			if err := a.printStatus(st); err != nil {
				return err
			}
			if client != nil && !a.quiet && !a.json() && st.State == api.StateRunning {
				if forwards, err := client.Forwards(cmd.Context()); err == nil {
					if note := lanNote(forwards); note != "" {
						kv(a.Stdout, "note", note)
					}
				}
			}
			if st.State == api.StateStopped {
				return &exit{code: ExitNotRunning, silent: true, err: errors.New("stopped")}
			}
			return nil
		},
	}
}

func lanNote(forwards []api.Forward) string {
	var ports []string
	for _, f := range forwards {
		if f.State != api.ForwardActive || exposure(f) != "LAN" || len(f.Mac) == 0 {
			continue
		}
		ports = append(ports, f.Mac[0][strings.LastIndex(f.Mac[0], ":")+1:]+"/"+f.Proto)
	}
	if len(ports) == 0 {
		return ""
	}
	listed := ports
	if len(listed) > 8 {
		listed = append(append([]string{}, listed[:8]...), "...")
	}
	noun, verb := "published ports", "answer"
	if len(ports) == 1 {
		noun, verb = "published port", "answers"
	}
	return fmt.Sprintf(`%d %s %s other devices on your network (%s); "fev config set %s false" keeps them to this Mac`,
		len(ports), noun, verb, strings.Join(listed, ", "), api.KeyNetworkPublishLAN)
}

func (a *app) startCmd() *cobra.Command {
	var noWait, all bool
	cmd := &cobra.Command{
		Use:   "start [MACHINE...]",
		Short: "Start fev, or Linux machines",
		Long: `Start fev: the daemon if none is running, then the VM and the Docker
engine. It returns when the engine is ready, or at once with --no-wait.

After "fev stop" this is what lets docker commands start fev again.

With MACHINE, start those Linux machines (and the VM first if it is
stopped); -a starts every machine.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, argv []string) error {
			if len(argv) > 0 || all {
				if len(argv) > 0 && all {
					return usagef("start: name the machines, or -a for all, not both")
				}
				return a.startMachines(cmd.Context(), argv, all)
			}
			return a.withDaemon(cmd.Context(), true, func(c *api.Client) error {
				a.resuming()
				st, err := c.VMStart(cmd.Context(), api.StartParams{NoWait: noWait})
				if err != nil {
					return err
				}
				return a.started(st, noWait)
			})
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return at once, not when the engine is ready")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "start every Linux machine")
	return cmd
}

func (a *app) started(st *api.Status, noWait bool) error {
	if !noWait && st.State != api.StateRunning && st.State != api.StatePaused {
		why := st.Error
		if why == "" {
			why = "the VM is " + stateText(st)
		}
		return failf(ExitFailed, "not started: %s", why)
	}
	return a.emit(st, func(w io.Writer) { statusText(w, st) })
}

func (a *app) restartCmd() *cobra.Command {
	var noWait, all bool
	cmd := &cobra.Command{
		Use:   "restart [docker | MACHINE...]",
		Short: "Restart the VM, the Docker engine, or Linux machines",
		Long: `Restart the VM, which puts configuration that waits for a restart into
effect. "fev restart docker" restarts the Docker engine alone: the VM and
everything else in it stay up, and docker commands issued meanwhile are held.

With MACHINE, restart those Linux machines; -a restarts every running one.
("docker" is no machine's name.)`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, argv []string) error {
			engine := len(argv) == 1 && argv[0] == "docker"
			if (len(argv) > 0 && !engine) || all {
				if len(argv) > 0 && all {
					return usagef("restart: name the machines, or -a for all, not both")
				}
				return a.restartMachines(cmd.Context(), argv, all)
			}
			return a.withDaemon(cmd.Context(), true, func(c *api.Client) error {
				if engine {
					a.resuming()
					ds, err := c.DockerRestart(cmd.Context())
					if err != nil {
						return err
					}
					return a.emit(ds, func(w io.Writer) { dockerText(w, ds) })
				}
				st, err := c.VMRestart(cmd.Context(), api.StartParams{NoWait: noWait})
				if err != nil {
					return err
				}
				return a.started(st, noWait)
			})
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return at once, not when the engine is ready")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "restart every running Linux machine")
	return cmd
}

func dockerText(w io.Writer, ds *api.DockerStatus) {
	kv(w, "engine", ds.Engine)
	kv(w, "socket", ds.Socket)
	if ds.ConfigFile != "" {
		kv(w, "engine configuration", ds.ConfigFile+" ("+engineConfigText(ds)+")")
	}
}

func engineConfigText(ds *api.DockerStatus) string {
	switch ds.ConfigState {
	case api.EngineConfigApplied:
		return "applied"
	case api.EngineConfigRefused:
		return "refused: " + ds.ConfigError
	case api.EngineConfigNone, "":
		return "none"
	}
	return ds.ConfigState
}

func (a *app) stopCmd() *cobra.Command {
	var force, all bool
	var timeout int
	cmd := &cobra.Command{
		Use:   "stop [MACHINE...]",
		Short: "Stop fev, or Linux machines",
		Long: `Stop the VM and then the daemon. It starts nothing: with no daemon running
it only records that a stop was asked for.

A stop stays stopped. Afterwards a docker command starts nothing and says
how to start; "fev start" (or any fev command that needs the daemon)
starts fev again.

Requests take effect in the order they arrive: a request for fev made
while this stop is under way (fev start, a machine's session, ssh fev
through its ProxyCommand) waits for the VM to stop and then starts it
again, and the stop then leaves the daemon and that VM running and says
so.

With MACHINE, stop those Linux machines and nothing else, each given
--timeout seconds (default 10) to halt, or killed at once with -f; -a
stops every machine. A machine stopped so is not started again with the
VM.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, argv []string) error {
			if timeout < 0 {
				return usagef("--timeout must not be negative")
			}
			if len(argv) > 0 || all {
				if len(argv) > 0 && all {
					return usagef("stop: name the machines, or -a for all, not both")
				}
				return a.stopMachines(cmd.Context(), argv, all, force, timeout)
			}
			l, err := a.layout()
			if err != nil {
				return err
			}
			_, statErr := os.Stat(api.StopMarkPath(l.Home))
			had := statErr == nil
			began := boottrace.Now()
			if err := leaveStopMark(l.Home); err != nil {
				return err
			}
			client, err := a.connect(cmd.Context())
			if err == errNotRunning {
				return a.stopped(l.Home)
			}
			if data, ok := replaceable(err); ok {
				if err := a.stopOther(cmd.Context(), l, data); err != nil {
					return err
				}
				return a.stopped(l.Home)
			}
			if err != nil {
				if !had {
					os.Remove(api.StopMarkPath(l.Home))
				}
				return unreachable(err)
			}
			defer client.Close()
			gone := func(err error) bool { return api.IsShuttingDown(err) || errors.Is(err, jsonrpc.ErrClosed) }
			_, err = client.VMStop(cmd.Context(), api.StopParams{Force: force, TimeoutMS: timeout * 1000, StopBeganNS: began})
			if err != nil && !gone(err) {
				return err
			}
			res, err := client.Shutdown(cmd.Context(), api.ShutdownParams{Force: force, StopRequested: true, StopBeganNS: began})
			if err != nil && !gone(err) {
				return err
			}
			if err == nil && res.Superseded {
				st, err := client.Status(cmd.Context())
				if err != nil {
					return err
				}
				if !a.quiet && !a.json() {
					fmt.Fprintln(a.Stderr, "fev: started again by a request that came after this stop began (fev start, a machine's session, an ssh login)")
				}
				return a.emit(st, func(w io.Writer) { statusText(w, st) })
			}
			select {
			case <-client.Done():
			case <-cmd.Context().Done():
				return cmd.Context().Err()
			}
			return a.stopped(l.Home)
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "do not ask the guest: what it has not synced is lost, as on a power cut")
	cmd.Flags().IntVar(&timeout, "timeout", 0, "seconds the guest is given to shut down before the stop is forced (default 15)")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "stop every Linux machine")
	return cmd
}

func leaveStopMark(home string) error {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(api.StopMarkPath(home), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("the mark that keeps docker commands from starting fev could not be left: %w", err)
	}
	return f.Close()
}

func (a *app) stopped(home string) error {
	if err := leaveStopMark(home); err != nil {
		return err
	}
	st, err := a.stoppedStatus()
	if err != nil {
		return err
	}
	return a.emit(st, func(w io.Writer) { statusText(w, st) })
}

func (a *app) resuming() {
	l, err := a.layout()
	if err != nil {
		return
	}
	if _, err := os.Stat(api.PauseMarkPath(l.Home)); err == nil && !a.quiet && !a.json() {
		fmt.Fprintln(a.Stderr, "fev: paused for the Mac's sleep; resuming")
	}
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print this build, and whether the daemon running is the same one",
		Long: `Print this program's build identifier. Every part of fev carries one and
refuses a part from another build, so if a daemon is running and is another
build this says so and exits 4. It starts nothing.

--format json prints what this program tells the daemon about itself.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			self := api.HelloParams{APIVersion: api.APIVersion, Build: version.Build, Client: a.Name}
			if a.quiet {
				fmt.Fprintln(a.Stdout, version.Build)
			} else if a.json() {
				if err := a.printJSON(self); err != nil {
					return err
				}
			} else {
				kv(a.Stdout, "fev", version.Build)
				kv(a.Stdout, "api", api.APIVersion)
			}
			daemon, err := a.daemonLine(cmd.Context())
			if err != nil {
				return unreachable(err)
			}
			if !a.quiet && !a.json() {
				kv(a.Stdout, "daemon", daemon)
			}
			return nil
		},
	}
}

func (a *app) daemonLine(ctx context.Context) (string, error) {
	client, err := a.connect(ctx)
	if err == errNotRunning {
		return "not running", nil
	}
	if err != nil {
		return "", err
	}
	client.Close()
	return fmt.Sprintf("running, the same build (pid %d)", a.hello.PID), nil
}
