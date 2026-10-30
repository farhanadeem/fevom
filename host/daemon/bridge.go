package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"fev/host/daemon/spawn"
	"fev/host/docker/vsock"
	"fev/proto/agent"
	"fev/proto/bridge"
	"fev/proto/relay"
)

const bridgeDialBound = 5 * time.Second

const loginBound = 10 * time.Second

const lingerBound = 5 * time.Second

const failedStatus = 255

const drainBound = 2 * time.Second

type bridgeServer struct {
	view func(machine string) string

	log       *slog.Logger
	share     func(machine string) (share, known bool)
	root      string
	agentSock func() string
	notify    func(title, message string) error
	slots     chan struct{}

	envOnce   sync.Once
	loginPath string
	shell     string
	home      string
	user      string
}

func newBridgeServer(m *Machines, log *slog.Logger) *bridgeServer {
	b := &bridgeServer{log: log, share: m.machineShare, root: m.ShareRoot, notify: m.Notify, view: m.View, slots: make(chan struct{}, agent.MaxStreams)}
	if m.Agent != nil {
		b.agentSock = m.Agent.Path
	} else {
		sock := m.SSHAuthSock
		b.agentSock = func() string { return sock }
	}
	if b.root == "" {
		b.root = "/"
	}
	if b.notify == nil {
		b.notify = osascriptNotify
	}
	b.home, b.user = os.Getenv("HOME"), m.MacUser
	if u, err := user.Current(); err == nil {
		if b.home == "" {
			b.home = u.HomeDir
		}
		if b.user == "" {
			b.user = u.Username
		}
	}
	return b
}

func (b *bridgeServer) requests(vm VM) func(agent.StreamRequest) {
	return func(req agent.StreamRequest) {
		if (relay.Request{Kind: relay.Stream, Target: req.Token}).Validate() != nil {
			return
		}
		select {
		case b.slots <- struct{}{}:
		default:
			go b.refuse(vm, req.Token, "the daemon serves too many of the machines' streams")
			return
		}
		go func() {
			defer func() { <-b.slots }()
			b.serve(vm, req)
		}()
	}
}

func (b *bridgeServer) refuse(vm VM, token, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), bridgeDialBound)
	defer cancel()
	if a := vm.Agent(); a != nil {
		a.StreamRefuse(ctx, token, reason)
	}
	b.log.Debug("bridge: refused a stream a machine asked for", "reason", reason)
}

func (b *bridgeServer) serve(vm VM, req agent.StreamRequest) {
	if req.UID == nil || relay.CheckMachineName(req.Machine) != nil {
		b.refuse(vm, req.Token, "a request from a machine names the machine and the uid")
		return
	}
	switch req.Service {
	case agent.StreamSSHAgent:
		b.serveAgent(vm, req)
	case agent.StreamMac:
		b.serveMac(vm, req)
	default:
		b.refuse(vm, req.Token, "the daemon has no service "+clip(req.Service, 40)+" for machines")
	}
}

func (b *bridgeServer) open(vm VM, token string) (*net.UnixConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bridgeDialBound)
	defer cancel()
	return vsock.Relay{Path: vm.VsockPath(relay.Port)}.Open(ctx, relay.Request{Kind: relay.Stream, Target: token})
}

func (b *bridgeServer) serveAgent(vm VM, req agent.StreamRequest) {
	sock := b.agentSock()
	if sock == "" {
		b.refuse(vm, req.Token, "SSH_AUTH_SOCK is not set")
		return
	}
	d := net.Dialer{Timeout: bridgeDialBound}
	service, err := d.Dial("unix", sock)
	if err != nil {
		b.refuse(vm, req.Token, "the SSH agent at SSH_AUTH_SOCK does not answer: "+err.Error())
		return
	}
	defer service.Close()
	stream, err := b.open(vm, req.Token)
	if err != nil {
		b.log.Debug("bridge: the guest did not take the agent's stream", "machine", req.Machine, "err", err)
		return
	}
	defer stream.Close()
	splice(stream, service.(*net.UnixConn))
}

func splice(a, b *net.UnixConn) {
	done := make(chan struct{}, 2)
	one := func(dst, src *net.UnixConn) {
		io.Copy(dst, src)
		dst.CloseWrite()
		done <- struct{}{}
	}
	go one(a, b)
	go one(b, a)
	<-done
	<-done
}

func (b *bridgeServer) viewOf(machine string) string {
	if b.view == nil {
		return ""
	}
	return b.view(machine)
}

func (b *bridgeServer) serveMac(vm VM, req agent.StreamRequest) {
	conn, err := b.open(vm, req.Token)
	if err != nil {
		b.log.Debug("bridge: the guest did not take the bridge's stream", "machine", req.Machine, "err", err)
		return
	}
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 64<<10)
	hdr, _, err := bridge.ReadHeader(r)
	if err == nil {
		err = hdr.Check()
	}
	if err != nil {
		bridge.WriteError(conn, err.Error())
		return
	}
	share, known := b.share(req.Machine)
	if !known {
		bridge.WriteError(conn, "the daemon has no machine "+req.Machine)
		return
	}
	switch hdr.Verb {
	case bridge.VerbInfo:
		b.loginEnv()
		bridge.WriteOK(conn)
		json.NewEncoder(conn).Encode(bridge.Info{Home: b.home, User: b.user, Share: share, Root: b.root, View: b.viewOf(req.Machine)})
	case bridge.VerbNotify:
		title := hdr.Title
		if title == "" {
			title = req.Machine
		}
		if err := b.notify(title, hdr.Message); err != nil {
			bridge.WriteError(conn, "the notification: "+err.Error())
			return
		}
		bridge.WriteOK(conn)
	case bridge.VerbRun, bridge.VerbExec:
		b.log.Debug("bridge: a command from a machine", "machine", req.Machine, "uid", *req.UID, "user", hdr.User, "program", clip(hdr.Argv[0], 80), "path", clip(hdr.Path, 80))
		b.run(conn, r, hdr, req.Machine, share)
	default:
		bridge.WriteError(conn, hdr.Verb+" is not the daemon's")
	}
}

func (b *bridgeServer) loginEnv() {
	b.envOnce.Do(func() {
		b.shell = os.Getenv("SHELL")
		if b.shell == "" {
			b.shell = "/bin/zsh"
		}
		b.loginPath = os.Getenv("PATH")
		ctx, cancel := context.WithTimeout(context.Background(), loginBound)
		defer cancel()
		const mark = "__fev_login_path__"
		cmd := exec.CommandContext(ctx, b.shell, "-l", "-c", `printf '\n%s\n%s\n' `+mark+` "$PATH"`)
		cmd.Dir = b.home
		cmd.Env = append(os.Environ(), "SHELL="+b.shell)
		out, err := cmd.Output()
		if err != nil {
			b.log.Warn("bridge: the login shell did not say its PATH; the daemon's is used", "shell", b.shell, "err", err)
			return
		}
		lines := strings.Split(string(out), "\n")
		for i := len(lines) - 2; i >= 0; i-- {
			if lines[i] == mark && lines[i+1] != "" {
				b.loginPath = lines[i+1]
				return
			}
		}
		b.log.Warn("bridge: the login shell's output held no PATH; the daemon's is used", "shell", b.shell)
	})
}

func lookPath(name, path string) (string, error) {
	if strings.Contains(name, "/") {
		return name, executable(name)
	}
	var found error = os.ErrNotExist
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		err := executable(p)
		if err == nil {
			return p, nil
		}
		if errors.Is(err, os.ErrPermission) {
			found = err
		}
	}
	return "", found
}

func executable(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if st.IsDir() || st.Mode()&0o111 == 0 {
		return os.ErrPermission
	}
	return nil
}

var macSignals = map[string]syscall.Signal{
	"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT,
	"TERM": syscall.SIGTERM, "USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2,
}

type frameWriter struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

func (f *frameWriter) frame(stream byte, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return net.ErrClosed
	}
	return bridge.WriteFrame(f.w, stream, p)
}

func (f *frameWriter) last(e bridge.Exit) {
	b, _ := json.Marshal(e)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		bridge.WriteFrame(f.w, bridge.FrameExit, b)
		f.closed = true
	}
}

func (b *bridgeServer) run(conn *net.UnixConn, r *bufio.Reader, hdr *bridge.Header, machine string, share bool) {
	b.loginEnv()
	if bridge.WriteOK(conn) != nil {
		return
	}
	w := &frameWriter{w: conn}
	linger := func() {
		conn.CloseWrite()
		conn.SetReadDeadline(time.Now().Add(lingerBound))
	}
	fail := func(code int, format string, args ...any) {
		w.last(bridge.Exit{Code: code, Error: fmt.Sprintf(format, args...)})
		linger()
		io.Copy(io.Discard, r)
	}
	argv := make([]string, len(hdr.Argv))
	for i, a := range hdr.Argv {
		argv[i] = bridge.MacArg(a, b.root, share)
	}
	var prog string
	var err error
	if hdr.Verb == bridge.VerbExec {
		mp, seen := bridge.MacProgram(hdr.Path, b.root, share, b.viewOf(machine))
		if !seen {
			fail(126, "a Mac program in the machine's own files, which the Mac cannot see; run it from the Mac's files (under %s or a folder the Mac shares at its own path)", bridge.ShareTarget)
			return
		}
		prog, err = mp, executable(mp)
		if errors.Is(err, os.ErrNotExist) {
			fail(127, "not found on the Mac at %s", mp)
			return
		}
	} else {
		prog, err = lookPath(argv[0], b.loginPath)
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		fail(127, "%s: command not found on the Mac", argv[0])
		return
	case err != nil:
		fail(126, "%s: cannot be run on the Mac: permission denied", argv[0])
		return
	}
	dir, seen := bridge.MacDirIn(hdr.Dir, b.root, share, b.viewOf(machine))
	if seen {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			w.frame(bridge.FrameStderr, []byte(fmt.Sprintf("mac: %s is not a directory on the Mac; the command runs in an empty one\n", dir)))
			seen = false
		}
	}
	scratch := ""
	if !seen {
		var err error
		if scratch, err = os.MkdirTemp("", "fev-mac-"); err != nil {
			fail(126, "a directory for the command on the Mac: %v", err)
			return
		}
		defer os.Remove(scratch)
		dir = scratch
	}
	env := []string{
		"HOME=" + b.home, "USER=" + b.user, "LOGNAME=" + b.user, "SHELL=" + b.shell, "PATH=" + b.loginPath,
		"FEV_MACHINE=" + machine,
	}
	for _, k := range []string{"TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "__CF_USER_TEXT_ENCODING"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, hdr.Env...)

	c, err := startCommand(prog, argv, dir, env, hdr)
	if err != nil {
		code := 126
		if errors.Is(err, os.ErrNotExist) {
			code = 127
		}
		fail(code, "%s: %v", argv[0], err)
		return
	}
	defer c.release()

	var pumps sync.WaitGroup
	for _, o := range c.outputs {
		pumps.Add(1)
		go func() {
			defer pumps.Done()
			buf := make([]byte, bridge.MaxFrame)
			if o.f == c.pty {
				defer o.f.Close()
			}
			for {
				n, err := o.f.Read(buf)
				if n > 0 && o.stream != discard && w.frame(o.stream, buf[:n]) != nil {
					io.Copy(io.Discard, o.f)
					return
				}
				if err != nil {
					return
				}
			}
		}()
	}
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		buf := make([]byte, bridge.MaxFrame)
		for {
			stream, n, err := bridge.ReadFrame(r, buf)
			if err != nil {
				c.signal(syscall.SIGHUP)
				c.closeInput()
				return
			}
			switch stream {
			case bridge.FrameStdin:
				if n == 0 {
					c.closeInput()
					continue
				}
				c.write(buf[:n])
			case bridge.FrameControl:
				var ctl bridge.Control
				if json.Unmarshal(buf[:n], &ctl) != nil {
					continue
				}
				if ctl.Rows > 0 && ctl.Cols > 0 {
					c.resize(ctl.Rows, ctl.Cols)
				}
				if s, ok := macSignals[ctl.Signal]; ok {
					c.signal(s)
				}
			}
		}
	}()

	st, _ := c.proc.Wait()
	c.ended()
	c.closeInput()
	drained := make(chan struct{})
	go func() {
		pumps.Wait()
		close(drained)
	}()
	c.closeTTY()
	timer := time.NewTimer(drainBound)
	select {
	case <-drained:
		timer.Stop()
	case <-timer.C:
	}
	if scratch != "" {
		os.Remove(scratch)
	}
	e := bridge.Exit{Code: failedStatus}
	if st != nil {
		if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			e.Code, e.Signal = 0, int(ws.Signal())
		} else {
			e.Code = st.ExitCode()
		}
	}
	w.last(e)
	linger()
	<-inputDone
}

type command struct {
	proc     *os.Process
	pty, tty *os.File
	inMu     sync.Mutex
	stdin    *os.File
	viaPTY   bool
	over     bool
	outputs  []output
	pipes    []*os.File
}

type output struct {
	f      *os.File
	stream byte
}

const discard = 0xff

func startCommand(prog string, argv []string, dir string, env []string, hdr *bridge.Header) (*command, error) {
	c := &command{}
	files := make([]*os.File, 3)
	var theirs []*os.File
	cleanup := func() {
		for _, f := range theirs {
			f.Close()
		}
		c.release()
		if c.pty != nil {
			c.pty.Close()
		}
	}
	sys := &syscall.SysProcAttr{}
	if hdr.TTY() {
		master, tty, err := openPTY()
		if err != nil {
			return nil, err
		}
		c.pty, c.tty = master, tty
		setSize(tty, hdr.Rows, hdr.Cols)
		sys.Setsid, sys.Setctty = true, true
		for i, on := range []bool{hdr.Stdin, hdr.Stdout, hdr.Stderr} {
			if on {
				files[i] = tty
				sys.Ctty = i
				break
			}
		}
		stream := byte(discard)
		switch {
		case hdr.Stdout:
			stream = bridge.FrameStdout
		case hdr.Stderr:
			stream = bridge.FrameStderr
		}
		c.outputs = append(c.outputs, output{master, stream})
	}
	if hdr.Stdin {
		files[0] = c.tty
		c.viaPTY = true
	} else {
		pr, pw, err := os.Pipe()
		if err != nil {
			cleanup()
			return nil, err
		}
		files[0], c.stdin = pr, pw
		theirs = append(theirs, pr)
	}
	for i, on := range []bool{hdr.Stdout, hdr.Stderr} {
		fd := i + 1
		if on {
			files[fd] = c.tty
			continue
		}
		pr, pw, err := os.Pipe()
		if err != nil {
			cleanup()
			return nil, err
		}
		files[fd] = pw
		theirs = append(theirs, pw)
		c.pipes = append(c.pipes, pr)
		c.outputs = append(c.outputs, output{pr, byte(fd)})
	}
	var proc *os.Process
	var err error
	if hdr.TTY() {
		proc, err = os.StartProcess(prog, argv, &os.ProcAttr{Dir: dir, Env: env, Files: files, Sys: sys})
	} else {
		proc, err = spawn.Start(prog, argv, env, dir, [3]*os.File{files[0], files[1], files[2]})
	}
	if err != nil {
		cleanup()
		return nil, err
	}
	for _, f := range theirs {
		f.Close()
	}
	c.proc = proc
	return c, nil
}

func (c *command) write(p []byte) {
	if c.viaPTY {
		c.pty.Write(p)
		return
	}
	c.inMu.Lock()
	in := c.stdin
	c.inMu.Unlock()
	if in != nil {
		in.Write(p)
	}
}

func (c *command) closeInput() {
	c.inMu.Lock()
	defer c.inMu.Unlock()
	if c.stdin != nil {
		c.stdin.Close()
		c.stdin = nil
	}
}

func (c *command) resize(rows, cols int) {
	c.inMu.Lock()
	defer c.inMu.Unlock()
	if c.tty != nil {
		setSize(c.tty, rows, cols)
	}
}

func (c *command) signal(sig syscall.Signal) {
	c.inMu.Lock()
	defer c.inMu.Unlock()
	if c.proc != nil && !c.over {
		syscall.Kill(-c.proc.Pid, sig)
	}
}

func (c *command) ended() {
	c.inMu.Lock()
	c.over = true
	c.inMu.Unlock()
}

func (c *command) closeTTY() {
	c.inMu.Lock()
	defer c.inMu.Unlock()
	if c.tty != nil {
		c.tty.Close()
		c.tty = nil
	}
}

func (c *command) release() {
	c.closeInput()
	c.closeTTY()
	for _, f := range c.pipes {
		f.Close()
	}
}

func osascriptNotify(title, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), bridgeDialBound)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/osascript",
		"-e", "on run argv",
		"-e", "display notification (item 1 of argv) with title (item 2 of argv)",
		"-e", "end run", message, title).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
