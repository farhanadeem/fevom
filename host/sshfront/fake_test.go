package sshfront

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pkg/sftp"

	"fev/proto/agent"
	"fev/proto/relay"
)

type fakeBackend struct {
	homes map[string]string
	def   string

	resolves, execs, dials atomic.Int32

	mu    sync.Mutex
	procs []*fakeProc
	last  ExecRequest
}

func newFakeBackend(dir string) *fakeBackend {
	b := &fakeBackend{homes: map[string]string{}, def: "u24"}
	for _, m := range []string{"u24", "f43"} {
		home := dir + "/" + m
		os.MkdirAll(home, 0o755)
		b.homes[m] = home
	}
	return b
}

func (b *fakeBackend) calls() int32 { return b.resolves.Load() + b.execs.Load() + b.dials.Load() }

func (b *fakeBackend) Resolve(login string) (Target, error) {
	b.resolves.Add(1)
	return ParseUser(login, func(n string) bool { return b.homes[n] != "" }, b.def)
}

func (b *fakeBackend) Exec(ctx context.Context, t Target, r ExecRequest) (Process, error) {
	b.execs.Add(1)
	home := b.homes[t.Machine]
	if home == "" {
		return nil, fmt.Errorf("no machine %q", t.Machine)
	}
	if t.User != "" && t.User != "root" {
		return nil, fmt.Errorf("the machine %s has no user %q", t.Machine, t.User)
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	mk := func(fd int) net.Conn {
		f := os.NewFile(uintptr(fd), "pair")
		defer f.Close()
		c, _ := net.FileConn(f)
		return c
	}
	p := &fakeProc{front: mk(fds[0]), back: mk(fds[1]), exit: make(chan Exit, 1), resized: make(chan [2]int, 4), req: r, home: home}
	b.mu.Lock()
	b.procs = append(b.procs, p)
	b.last = r
	b.mu.Unlock()
	go p.run()
	return p, nil
}

func (b *fakeBackend) Dial(ctx context.Context, t Target, addr netip.AddrPort) (net.Conn, error) {
	b.dials.Add(1)
	if b.homes[t.Machine] == "" {
		return nil, fmt.Errorf("no machine %q", t.Machine)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr.String())
}

func (b *fakeBackend) lastRequest() ExecRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

type fakeProc struct {
	front, back net.Conn
	exit        chan Exit
	resized     chan [2]int
	req         ExecRequest
	home        string

	mu     sync.Mutex
	cmd    *exec.Cmd
	closed bool
}

func (p *fakeProc) Stream() net.Conn  { return p.front }
func (p *fakeProc) Exit() <-chan Exit { return p.exit }
func (p *fakeProc) Resize(rows, cols int) error {
	select {
	case p.resized <- [2]int{rows, cols}:
	default:
	}
	return nil
}
func (p *fakeProc) Signal(sig int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil {
		return errors.New("not running")
	}
	return syscall.Kill(-p.cmd.Process.Pid, syscall.Signal(sig))
}
func (p *fakeProc) Close() error {
	p.mu.Lock()
	p.closed = true
	cmd := p.cmd
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	p.back.Close()
	return p.front.Close()
}

type framedIn struct {
	c   net.Conn
	buf []byte
	end bool
}

func (f *framedIn) Read(b []byte) (int, error) {
	for len(f.buf) == 0 {
		if f.end {
			return 0, io.EOF
		}
		frame := make([]byte, relay.MaxFrame)
		kind, n, err := relay.ReadFrame(f.c, frame)
		if err != nil {
			return 0, io.EOF
		}
		if kind == relay.Stdin && n == 0 {
			f.end = true
			continue
		}
		f.buf = frame[:n]
	}
	n := copy(b, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

type framedOut struct {
	c    net.Conn
	kind byte
	mu   *sync.Mutex
}

func (f framedOut) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(b), relay.WriteFrame(f.c, f.kind, b)
}

type rw struct {
	io.Reader
	io.Writer
}

func (rw) Close() error { return nil }

func (p *fakeProc) run() {
	defer p.back.Close()
	var wmu sync.Mutex
	if p.req.Subsystem == agent.SubsystemSFTP {
		srv, _ := sftp.NewServer(rw{&framedIn{c: p.back}, framedOut{p.back, relay.Stdout, &wmu}}, sftp.WithServerWorkingDirectory(p.home))
		srv.Serve()
		p.exit <- Exit{}
		return
	}
	line := "exec /bin/sh -i"
	if !p.req.Login {
		line = p.req.Argv[0]
	}
	if p.req.TTY && line == "fev-size" {
		fmt.Fprintf(p.back, "%d %d\r\n", p.req.Rows, p.req.Cols)
		select {
		case s := <-p.resized:
			fmt.Fprintf(p.back, "%d %d\r\n", s[0], s[1])
		case <-time.After(10 * time.Second):
		}
		p.exit <- Exit{}
		return
	}
	cmd := exec.Command("/bin/sh", "-c", line)
	cmd.Dir = p.home
	cmd.Env = append([]string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + p.home}, p.req.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var in io.Reader = &framedIn{c: p.back}
	if p.req.TTY {
		in = p.back
		cmd.Stdout, cmd.Stderr = p.back, p.back
	} else {
		cmd.Stdout = framedOut{p.back, relay.Stdout, &wmu}
		cmd.Stderr = framedOut{p.back, relay.Stderr, &wmu}
	}
	stdin, _ := cmd.StdinPipe()
	go func() {
		io.Copy(stdin, in)
		stdin.Close()
	}()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.exit <- Exit{Code: -1, Err: "closed"}
		return
	}
	err := cmd.Start()
	p.cmd = cmd
	p.mu.Unlock()
	if err != nil {
		p.exit <- Exit{Code: -1, Err: err.Error()}
		return
	}
	cmd.Wait()
	ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
	p.back.Close()
	if ws.Signaled() {
		p.exit <- Exit{Code: -1, Signal: int(ws.Signal())}
		return
	}
	p.exit <- Exit{Code: ws.ExitStatus()}
}
