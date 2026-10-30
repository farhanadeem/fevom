package nfsview

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const GuardArg = "__fev-view-guard"

func StartGuard(prog []string, dir, source string) (stop func(), err error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	args := append(append([]string{}, prog[1:]...), GuardArg, dir, source)
	cmd := exec.Command(prog[0], args...)
	cmd.ExtraFiles = []*os.File{r}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin, cmd.Stdout = nil, nil
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	r.Close()
	return func() {
		w.Close()
		cmd.Wait()
	}, nil
}

func GuardMain(args []string) int {
	if len(args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: fevd %s DIR SOURCE\n", GuardArg)
		return 2
	}
	dir, source := args[0], args[1]
	pipe := os.NewFile(3, "daemon")
	io.Copy(io.Discard, pipe)
	m, ok, err := Find(dir)
	if err != nil || !ok || m.Type != "nfs" || m.From != source || m.Owner != uint32(os.Getuid()) {
		return 0
	}
	t := time.Now()
	if err := ForceUnmount(dir); err != nil {
		fmt.Fprintf(os.Stderr, "fevd's view guard: the daemon ended and left its view mounted: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "fevd's view guard: the daemon ended and left its view mounted; forced off in %d ms\n", time.Since(t).Milliseconds())
	return 0
}
