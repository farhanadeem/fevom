package debugremote

import (
	"context"
	"os"
	"os/exec"
)

func (o *Options) fallbackExec(ctx context.Context) (int, error) {
	args := o.Ctx.args("exec")
	if o.TTY {
		args = append(args, "-t")
	}
	if o.Stdin {
		args = append(args, "-i")
	}
	args = append(args, o.Target)
	if len(o.Argv) > 0 {
		args = append(args, o.Argv...)
	} else {
		args = append(args, "sh")
	}
	cmd := exec.CommandContext(ctx, o.Ctx.DockerCLI, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = o.In, o.Out, o.Err
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 126, err
	}
	return 0, nil
}
