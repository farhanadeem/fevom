package cli

import (
	"context"
	"io"
	"os/exec"
)

type remoteDebug struct {
	Target   string
	ReadOnly bool
	Reset    bool
	Fallback bool
	Dir      string
	Command  []string
	In       io.Reader
	Out, Err io.Writer
}

type debugRemote interface {
	Debug(ctx context.Context, p remoteDebug) (int, error)
	Reset(ctx context.Context) (int64, error)
	Close() error
}

var newDebugRemote = func(ctx context.Context, a *app, context string) (debugRemote, error) {
	return nil, failf(ExitFailed, "debugging a remote Docker context (-c %s) is not built in this binary", context)
}

func runDocker(ctx context.Context, path string, args, env []string, in io.Reader, out, errw io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errw
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return 0, failf(ExitFailed, "the fallback could not run docker: %v", err)
}
