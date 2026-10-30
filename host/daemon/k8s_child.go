package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"fev/host/bundle"
	"fev/host/k8s"
)

const k8sChildReady = 10 * time.Second

const k8sChildStop = 5 * time.Second

func K8sChildPath() string {
	if p := os.Getenv("FEV_K8S_CHILD"); p != "" {
		return p
	}
	return bundle.Peer(bundle.Self(), "fev-k8s")
}

func (k *K8s) child(ctx context.Context, vm VM) (*k8s.Client, func(), error) {
	if k.host.Fake() {
		c, err := k8s.Dial(ctx, standinK8sSocket(vm))
		if err != nil {
			return nil, nil, fmt.Errorf("the stand-in's cluster: %w", err)
		}
		return c, func() {}, nil
	}
	path := K8sChildPath()
	if path == "" {
		return nil, nil, errors.New("fev-k8s is not beside fevd; this build cannot reach a cluster")
	}
	socket := filepath.Join(k.host.Layout().RunDir(), "k8s.sock")
	cmd := exec.Command(path, "--socket", socket)
	lifeline, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	errs, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	exited := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(errs)
		for sc.Scan() {
			k.log.Info("fev-k8s: " + sc.Text())
		}
	}()
	go func() {
		cmd.Wait()
		close(exited)
	}()
	stop := func() {
		lifeline.Close()
		select {
		case <-exited:
		case <-time.After(k8sChildStop):
			cmd.Process.Kill()
			<-exited
		}
		os.Remove(socket)
	}
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(out).ReadString('\n')
		if err == nil && strings.TrimSpace(line) != "ready" {
			err = fmt.Errorf("said %q", strings.TrimSpace(line))
		}
		ready <- err
		io.Copy(io.Discard, out)
	}()
	select {
	case err = <-ready:
	case <-exited:
		err = errors.New("exited before it was ready")
	case <-time.After(k8sChildReady):
		err = errors.New("not ready within 10s")
	}
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("fev-k8s: %w", err)
	}
	c, err := k8s.Dial(ctx, socket)
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("fev-k8s's socket: %w", err)
	}
	k.log.Info("k8s: fev-k8s started", "pid", cmd.Process.Pid)
	return c, stop, nil
}
