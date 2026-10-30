package launch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fev/host/bundle"
	"fev/host/daemon/boottrace"
	"fev/host/daemon/home"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

type Ready struct {
	Status string `json:"status"`
	PID    int    `json:"pid,omitempty"`
	Build  string `json:"build,omitempty"`
	Error  string `json:"error,omitempty"`
}

const (
	StatusReady          = "ready"
	StatusAlreadyRunning = "already_running"
	StatusError          = "error"
)

const DefaultReadyTimeout = 45 * time.Second

const DaemonEnv = "FEV_DAEMON"

type Options struct {
	Layout                 home.Layout
	Daemon                 string
	Args                   []string
	Client                 string
	SSHAuthSock            string
	Terminfo, TerminfoDirs string
	ReadyTimeout           time.Duration
	OriginNS               uint64
}

func (o Options) HelloOptions() []api.ClientOption {
	return []api.ClientOption{api.WithSSHAuthSock(o.SSHAuthSock), api.WithTerminfo(o.Terminfo, o.TerminfoDirs)}
}

func (o Options) DaemonPath() (string, error) { return o.daemon() }

func (o Options) daemon() (string, error) {
	if o.Daemon != "" {
		return o.Daemon, nil
	}
	if path := os.Getenv(DaemonEnv); path != "" {
		return path, nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	path := bundle.Peer(self, "fevd")
	if real := bundle.Self(); path == "" && real != "" {
		path = bundle.Peer(real, "fevd")
	}
	if path == "" {
		return "", fmt.Errorf("no daemon is running in %s and there is no fevd beside %s to start; set %s, or start one with fevd --home",
			o.Layout.Home, filepath.Base(self), DaemonEnv)
	}
	return path, nil
}

func Spawn(o Options) (Ready, error) {
	var ready Ready
	bin, err := o.daemon()
	if err != nil {
		return ready, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return ready, err
	}
	defer r.Close()
	origin := o.OriginNS
	if origin == 0 {
		origin = boottrace.Now()
	}
	args := append([]string{"--foreground", "--ready-fd", "3", "--origin-ns", strconv.FormatUint(origin, 10)}, o.Args...)
	cmd := exec.Command(bin, args...)
	cmd.ExtraFiles = []*os.File{w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	w.Close()
	if err != nil {
		return ready, fmt.Errorf("cannot start %s: %w", bin, err)
	}
	go cmd.Wait()

	timeout := o.ReadyTimeout
	if timeout <= 0 {
		timeout = DefaultReadyTimeout
	}
	r.SetReadDeadline(time.Now().Add(timeout))
	line, err := bufio.NewReader(r).ReadBytes('\n')
	if len(line) == 0 {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return ready, fmt.Errorf("the daemon did not report ready within %v; see %s", timeout, filepath.Join(o.Layout.LogDir(), "fevd.log"))
		}
		return ready, fmt.Errorf("the daemon exited before it was ready%s", logTail(o.Layout))
	}
	if err := json.Unmarshal(line, &ready); err != nil {
		return ready, fmt.Errorf("the daemon wrote %q where its ready line was expected", strings.TrimSpace(string(line)))
	}
	if ready.Status == StatusError {
		return ready, fmt.Errorf("the daemon could not start: %s", ready.Error)
	}
	return ready, nil
}

func logTail(l home.Layout) string {
	data, err := os.ReadFile(filepath.Join(l.LogDir(), "fevd.log"))
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return "; its log ends:\n  " + strings.Join(lines, "\n  ")
}

func accepts(l home.Layout) bool {
	conn, err := net.Dial("unix", l.Socket())
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func Ensure(o Options) error {
	if accepts(o.Layout) {
		return nil
	}
	for attempt := 0; ; attempt++ {
		if _, err := Spawn(o); err != nil {
			return err
		}
		if accepts(o.Layout) {
			return nil
		}
		if attempt == 1 {
			return fmt.Errorf("the daemon reported ready but %s does not accept connections", o.Layout.Socket())
		}
	}
}

func Dial(ctx context.Context, o Options) (*api.Client, *api.HelloResult, error) {
	name := o.Client
	if name == "" {
		name = filepath.Base(os.Args[0])
	}
	connect := func() (*api.Client, *api.HelloResult, error) {
		conn, err := net.Dial("unix", o.Layout.Socket())
		if err != nil {
			return nil, nil, err
		}
		return api.NewClient(ctx, conn, name, o.HelloOptions()...)
	}
	retry := func(err error) bool {
		var op *net.OpError
		return api.IsShuttingDown(err) || errors.As(err, &op) || errors.Is(err, jsonrpc.ErrClosed)
	}
	client, hello, err := connect()
	for attempt := 0; err != nil && retry(err) && attempt < 2; attempt++ {
		if _, serr := Spawn(o); serr != nil {
			return nil, nil, serr
		}
		client, hello, err = connect()
	}
	return client, hello, err
}

func Again(ctx context.Context, o Options) (*api.Client, *api.HelloResult, error) {
	if _, err := Spawn(o); err != nil {
		return nil, nil, err
	}
	return Dial(ctx, o)
}
