package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"fev/host/bundle"
	"fev/host/docker/vsock"
	"fev/host/sshfront"
	"fev/host/terminfo"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/relay"
	"fev/proto/version"
)

type SSH struct {
	Machines               *Machines
	Fevom                  string
	OwnPortOnly            bool
	Terminfo, TerminfoDirs string

	host    Host
	log     *slog.Logger
	files   *sshfront.Files
	srv     *sshfront.Server
	unix    net.Listener
	private net.Listener
	holdFor time.Duration

	mu        sync.Mutex
	tcp       []net.Listener
	port      int
	expose    bool
	listening bool
	bound     []string

	termMu   sync.Mutex
	term     terminfo.Env
	termSeen bool
}

func (s *SSH) Name() string    { return "ssh" }
func (s *SSH) Marks() []string { return nil }

const sshWarnKey = "ssh.listen"

func (s *SSH) Start(h Host) error {
	s.host, s.log = h, h.Log()
	h.Handle(api.MethodSSHInfo, s.info)
	s.files = &sshfront.Files{Dir: filepath.Join(h.Layout().Home, "ssh")}
	if err := s.files.Ensure(); err != nil {
		h.Warn(sshWarnKey, fmt.Sprintf("the SSH server for Linux machines is off: its keys in %s: %v", s.files.Dir, err))
		return nil
	}
	s.srv = &sshfront.Server{Files: s.files, Backend: sshBackend{m: s.Machines, ssh: s}, Log: s.log, Version: version.Build}
	if err := s.srv.Start(); err != nil {
		h.Warn(sshWarnKey, fmt.Sprintf("the SSH server for Linux machines is off: %v", err))
		return nil
	}
	path := api.SSHSocketPath(h.Layout().Home)
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return err
	}
	s.unix = l
	if s.private, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		l.Close()
		return err
	}
	go s.srv.Serve(s.private)
	go s.handOut(l)
	s.rebind(s.wanted())
	return nil
}

func (s *SSH) wanted() (port int, expose, listen bool) {
	cfg := s.host.Config()
	port, expose, listen = int(cfg.Int(api.KeySSHPort)), cfg.Bool(api.KeySSHExposePort), true
	if s.OwnPortOnly {
		if v, err := cfg.Get(api.KeySSHPort); err != nil || v[api.KeySSHPort].IsDefault {
			listen = false
		}
	}
	return port, expose, listen
}

func (s *SSH) proxyCommand() string {
	fev := s.Fevom
	home := s.host.Layout().Home
	if fev == "" {
		fev = filepath.Join(home, "bin", "fev")
		if _, err := os.Stat(fev); err != nil {
			fev = "fev"
			if beside := bundle.Peer(bundle.Self(), "fev"); beside != "" {
				fev = beside
			}
		}
	}
	cmd := sshfront.ShellQuote(fev)
	if user, err := os.UserHomeDir(); err != nil || filepath.Join(user, ".fev") != home {
		cmd += " --home " + sshfront.ShellQuote(home)
	}
	return cmd + " ssh --proxy"
}

func (s *SSH) rebind(port int, expose, listen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return
	}
	if s.port != port {
		if err := s.files.WriteClient(sshfront.Client{Port: port, ProxyCommand: s.proxyCommand()}); err != nil {
			s.log.Warn("ssh: the client's files could not be written", "dir", s.files.Dir, "err", err)
		}
	}
	for _, l := range s.tcp {
		l.Close()
	}
	s.tcp, s.bound, s.port, s.expose, s.listening = nil, nil, port, expose, false
	if !listen {
		s.host.Warn(sshWarnKey, "")
		s.log.Info("ssh: no TCP port: a daemon with a home of its own listens on one only when its home sets "+api.KeySSHPort,
			"socket", api.SSHSocketPath(s.host.Layout().Home))
		return
	}
	addrs := []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), net.JoinHostPort("::1", strconv.Itoa(port))}
	if expose {
		addrs = []string{":" + strconv.Itoa(port)}
	}
	var failed []string
	for _, a := range addrs {
		l, err := net.Listen("tcp", a)
		if err != nil {
			failed = append(failed, err.Error())
			continue
		}
		s.tcp = append(s.tcp, l)
		s.bound = append(s.bound, l.Addr().String())
		go s.srv.Serve(l)
	}
	s.listening = len(s.tcp) > 0 && (expose || strings.HasPrefix(s.bound[0], "127.0.0.1:"))
	if !s.listening {
		s.host.Warn(sshWarnKey, fmt.Sprintf("the SSH server for Linux machines cannot listen on port %d (%s); \"ssh fev\" still works through its ProxyCommand; another port: fev config set %s PORT",
			port, strings.Join(failed, "; "), api.KeySSHPort))
	} else {
		s.host.Warn(sshWarnKey, "")
		if len(failed) > 0 {
			s.log.Info("ssh: not every address could be had", "port", port, "failed", failed)
		}
	}
	s.log.Info("ssh: listening", "addresses", s.bound, "socket", api.SSHSocketPath(s.host.Layout().Home))
}

func (s *SSH) handOut(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go s.handOne(c.(*net.UnixConn))
	}
}

func (s *SSH) handOne(c *net.UnixConn) {
	defer c.Close()
	t, err := net.Dial("tcp", s.private.Addr().String())
	if err != nil {
		relay.WriteError(c, relay.CodeFailed, err.Error())
		return
	}
	defer t.Close()
	raw, err := t.(*net.TCPConn).SyscallConn()
	if err != nil {
		return
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		_, _, serr = c.WriteMsgUnix([]byte("ok\n"), syscall.UnixRights(int(fd)), nil)
	}); err != nil || serr != nil {
		return
	}
	bound := holdBound
	if s.holdFor > 0 {
		bound = s.holdFor
	}
	if !waitClosed(c, bound) {
		s.log.Warn("ssh: a client of the SSH socket did not take its connection within the bound", "bound", bound)
	}
}

func (s *SSH) StateChanged(api.Status) {}

func (s *SSH) Hello(p api.HelloParams) {
	if p.Terminfo == "" && p.TerminfoDirs == "" {
		return
	}
	s.termMu.Lock()
	s.term.Terminfo, s.term.TerminfoDirs, s.termSeen = p.Terminfo, p.TerminfoDirs, true
	s.termMu.Unlock()
}

func (s *SSH) terminfo(term string) []byte {
	if term == "" {
		return nil
	}
	s.termMu.Lock()
	env := s.term
	if !s.termSeen {
		env.Terminfo, env.TerminfoDirs = s.Terminfo, s.TerminfoDirs
	}
	s.termMu.Unlock()
	env.Home, _ = os.UserHomeDir()
	return terminfo.Find(term, env.Dirs())
}

func (s *SSH) ConfigChanged(ch api.ConfigChange) {
	if ch.Key != api.KeySSHPort && ch.Key != api.KeySSHExposePort {
		return
	}
	s.rebind(s.wanted())
}

func (s *SSH) Close() {
	s.mu.Lock()
	for _, l := range s.tcp {
		l.Close()
	}
	s.tcp = nil
	s.mu.Unlock()
	if s.unix != nil {
		s.unix.Close()
	}
	if s.private != nil {
		s.private.Close()
	}
	if s.srv != nil {
		s.srv.Close()
	}
}

func (s *SSH) Addresses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bound...)
}

func (s *SSH) info(ctx context.Context, _ json.RawMessage) (any, error) {
	cfg := s.host.Config()
	r := api.SSHInfo{
		Host: "127.0.0.1", Port: int(cfg.Int(api.KeySSHPort)), ExposePort: cfg.Bool(api.KeySSHExposePort),
		Alias: sshfront.Alias, User: sshfront.DefaultUser,
		Socket: api.SSHSocketPath(s.host.Layout().Home),
	}
	if r.ExposePort {
		r.Host = "0.0.0.0"
	}
	if s.files == nil {
		return r, nil
	}
	r.IdentityFile, r.KnownHostsFile, r.ConfigFile = s.files.IdentityPath(), s.files.KnownHostsPath(), s.files.ConfigPath()
	r.AuthorizedKeys, r.AuthorizedKeysError = s.files.AuthorizedKeysPath(), s.files.AuthorizedKeysError()
	r.HostKey = s.files.HostFingerprint()
	r.ProxyCommand = s.proxyCommand()
	r.Included = sshIncluded(r.ConfigFile)
	s.mu.Lock()
	r.Listening = s.listening
	s.mu.Unlock()
	return r, nil
}

func sshIncluded(configFile string) bool {
	user, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(user, ".ssh", "config"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && strings.EqualFold(f[0], "Include") {
			for _, p := range f[1:] {
				if strings.Trim(p, `"`) == configFile {
					return true
				}
			}
		}
	}
	return false
}

type sshBackend struct {
	m   *Machines
	ssh *SSH
}

func (b sshBackend) Resolve(login string) (sshfront.Target, error) {
	m := b.m
	m.mu.Lock()
	def := m.def
	names := map[string]bool{}
	for name := range m.entries {
		names[name] = true
	}
	m.mu.Unlock()
	return sshfront.ParseUser(login, func(n string) bool { return names[n] }, def)
}

func message(err error) error {
	var e *jsonrpc.Error
	if errors.As(err, &e) {
		return errors.New(e.Message)
	}
	return err
}

func (b sshBackend) started(ctx context.Context, name string) (*agent.Client, VM, error) {
	m := b.m
	if _, err := m.startMachine(ctx, name); err != nil {
		return nil, nil, message(err)
	}
	m.mu.Lock()
	client, vm := m.client, m.vm
	m.mu.Unlock()
	if client == nil || vm == nil {
		return nil, nil, errors.New("the VM stopped as the session was starting")
	}
	return client, vm, nil
}

func (b sshBackend) Exec(ctx context.Context, t sshfront.Target, r sshfront.ExecRequest) (sshfront.Process, error) {
	client, vm, err := b.started(ctx, t.Machine)
	if err != nil {
		return nil, err
	}
	user := t.User
	if user == "" {
		user = b.m.username(t.Machine)
	}
	p := &sshProc{client: client, exit: make(chan sshfront.Exit, 1), done: make(chan struct{})}
	params := agent.ExecStartParams{
		Context: agent.ExecMachine, Machine: t.Machine, User: user, Argv: r.Argv, Env: r.Env,
		TTY: r.TTY, Login: r.Login, Shell: r.Shell, Subsystem: r.Subsystem,
	}
	if r.TTY {
		params.Rows, params.Cols = r.Rows, r.Cols
		if b.ssh != nil {
			params.Terminfo = b.ssh.terminfo(agent.TermOf(r.Env))
		}
	}
	if params.Argv == nil {
		params.Argv = []string{}
	}
	started, err := client.ExecStart(ctx, params, func(e agent.ExecExit) {
		x := sshfront.Exit{Code: e.ExitCode, Signal: e.Signal, Err: e.Error}
		if e.Signal != 0 {
			x.Code = -1
		}
		p.deliver(x)
	})
	if err != nil {
		return nil, message(err)
	}
	p.id = started.ID
	stream, err := vsock.Relay{Path: vm.VsockPath(relay.Port)}.Open(ctx, relay.Request{Kind: relay.Exec, Target: started.Token})
	if err != nil {
		cctx, cancel := context.WithTimeout(context.Background(), machineCallBound)
		client.ExecClose(cctx, started.ID)
		cancel()
		return nil, fmt.Errorf("the session's stream could not be opened: %w", err)
	}
	p.stream = stream
	go func() {
		select {
		case <-client.Done():
			p.deliver(sshfront.Exit{Code: -1, Err: "the VM stopped"})
		case <-p.over():
		}
	}()
	return p, nil
}

func (b sshBackend) Dial(ctx context.Context, t sshfront.Target, addr netip.AddrPort) (net.Conn, error) {
	_, vm, err := b.started(ctx, t.Machine)
	if err != nil {
		return nil, err
	}
	conn, err := vsock.Relay{Path: vm.VsockPath(relay.Port)}.Open(ctx, relay.Request{Kind: relay.MTCP, Machine: t.Machine, Target: addr.String()})
	if err != nil {
		var re *relay.Error
		if errors.As(err, &re) && re.Text != "" {
			return nil, fmt.Errorf("%s in %s: %s", addr, t.Machine, re.Text)
		}
		return nil, err
	}
	return conn, nil
}

type sshProc struct {
	client *agent.Client
	id     uint64
	stream net.Conn

	once      sync.Once
	exit      chan sshfront.Exit
	closeOnce sync.Once
	done      chan struct{}
}

func (p *sshProc) over() <-chan struct{} { return p.done }

func (p *sshProc) deliver(e sshfront.Exit) {
	p.once.Do(func() {
		p.exit <- e
		close(p.done)
	})
}

func (p *sshProc) Stream() net.Conn           { return p.stream }
func (p *sshProc) Exit() <-chan sshfront.Exit { return p.exit }
func (p *sshProc) Resize(rows, cols int) error {
	return p.call(func(ctx context.Context) error { return p.client.ExecResize(ctx, p.id, rows, cols) })
}
func (p *sshProc) Signal(sig int) error {
	return p.call(func(ctx context.Context) error { return p.client.ExecSignal(ctx, p.id, sig) })
}
func (p *sshProc) call(fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), machineCallBound)
	defer cancel()
	return fn(ctx)
}

func (p *sshProc) Close() error {
	p.closeOnce.Do(func() {
		select {
		case <-p.over():
		default:
			p.call(func(ctx context.Context) error { return p.client.ExecClose(ctx, p.id) })
			p.deliver(sshfront.Exit{Code: -1, Err: "the session was closed"})
		}
		p.stream.Close()
	})
	return nil
}

var _ Module = (*SSH)(nil)
