package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"fev/host/daemon/home"
	"fev/host/daemon/launch"
	api "fev/proto/daemon"
	"fev/proto/version"
)

var terminate = func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func besideBuild(opts launch.Options) (string, error) {
	path, err := opts.DaemonPath()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", path, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func mayReplace(opts launch.Options, data api.BuildMismatch) error {
	builds := fmt.Sprintf("the running daemon is build %q and this fev is build %q", data.DaemonBuild, version.Build)
	beside, err := besideBuild(opts)
	switch {
	case err != nil:
		return failf(ExitUnreachable, "%s; the daemon to restart it into could not be asked for its build: %v", builds, err)
	case beside != version.Build:
		return failf(ExitUnreachable, "%s, and the fevd beside this fev is build %q: the install is half-updated; build or install everything in one pass", builds, beside)
	case data.State == api.StateStopped:
		return nil
	case data.RunningContainers == nil:
		return failf(ExitUnreachable, "%s; the VM is %s and may be running containers. Restarting the daemon into this build stops the VM: run \"fev stop\" (with the docker of that build, stop what must be stopped first), then run this again", builds, data.State)
	case *data.RunningContainers > 0:
		n, noun, verb := *data.RunningContainers, "containers", "are"
		if n == 1 {
			noun, verb = "container", "is"
		}
		return failf(ExitUnreachable, "%s; %d %s %s running. Restarting the daemon into this build stops the VM: stop them or run \"fev stop\", then run this again", builds, n, noun, verb)
	}
	return nil
}

func (a *app) stopOther(ctx context.Context, l home.Layout, data api.BuildMismatch) error {
	watch, err := net.Dial("unix", l.Socket())
	if err != nil {
		return nil
	}
	defer watch.Close()
	if err := terminate(data.PID); err != nil && !errors.Is(err, syscall.ESRCH) {
		return failf(ExitUnreachable, "the running daemon (build %q, pid %d) could not be asked to stop: %v", data.DaemonBuild, data.PID, err)
	}
	stop := context.AfterFunc(ctx, func() { watch.Close() })
	defer stop()
	watch.SetReadDeadline(time.Now().Add(launch.DefaultReadyTimeout))
	if _, err := io.Copy(io.Discard, watch); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return failf(ExitUnreachable, "the running daemon (build %q, pid %d) was asked to stop and had not gone after %v", data.DaemonBuild, data.PID, launch.DefaultReadyTimeout)
	}
	return nil
}

func (a *app) replaceDaemon(ctx context.Context, l home.Layout, opts launch.Options, data api.BuildMismatch) error {
	if err := mayReplace(opts, data); err != nil {
		return err
	}
	if !a.quiet {
		fmt.Fprintf(a.Stderr, "fev: the running daemon is build %q; restarting it into build %q\n", data.DaemonBuild, version.Build)
	}
	return a.stopOther(ctx, l, data)
}

func replaceable(err error) (api.BuildMismatch, bool) {
	data, ok := api.AsBuildMismatch(err)
	if !ok || data.PID <= 1 || data.PID == os.Getpid() {
		return data, false
	}
	return data, true
}
