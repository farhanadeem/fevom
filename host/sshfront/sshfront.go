package sshfront

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type Target struct {
	Machine string
	User    string
}

type ExecRequest struct {
	Argv      []string
	Login     bool
	Shell     bool
	Subsystem string
	Env       []string
	TTY       bool
	Rows      int
	Cols      int
}

type Exit struct {
	Code   int
	Signal int
	Err    string
}

type Process interface {
	Stream() net.Conn
	Resize(rows, cols int) error
	Signal(sig int) error
	Exit() <-chan Exit
	Close() error
}

type Backend interface {
	Resolve(login string) (Target, error)
	Exec(ctx context.Context, t Target, r ExecRequest) (Process, error)
	Dial(ctx context.Context, t Target, addr netip.AddrPort) (net.Conn, error)
}

const LoginGrace = 30 * time.Second

const maxUnauthenticated = 64

type Server struct {
	Files   *Files
	Backend Backend
	Log     *slog.Logger
	Version string

	mu      sync.Mutex
	config  *ssh.ServerConfig
	conns   map[*ssh.ServerConn]struct{}
	closed  bool
	pending chan struct{}
}

func (s *Server) Start() error {
	signer, err := s.Files.HostSigner()
	if err != nil {
		return err
	}
	cfg := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-fev_" + versionWord(s.Version),
		Config: ssh.Config{
			KeyExchanges: []string{ssh.KeyExchangeMLKEM768X25519, ssh.KeyExchangeCurve25519,
				ssh.KeyExchangeECDHP256, ssh.KeyExchangeECDHP384, ssh.KeyExchangeECDHP521},
		},
		PublicKeyAuthAlgorithms: []string{
			ssh.KeyAlgoED25519, ssh.KeyAlgoSKED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoSKECDSA256,
			ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256,
		},
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			ok, why := s.Files.Authorized(key)
			if !ok {
				if why != "" {
					s.logf(slog.LevelWarn, "ssh: a key was refused", "user", meta.User(), "from", meta.RemoteAddr().String(), "why", why)
				}
				return nil, errors.New("not an authorized key")
			}
			return &ssh.Permissions{Extensions: map[string]string{"fingerprint": ssh.FingerprintSHA256(key)}}, nil
		},
	}
	cfg.AddHostKey(signer)
	s.mu.Lock()
	s.config = cfg
	s.conns = map[*ssh.ServerConn]struct{}{}
	s.pending = make(chan struct{}, maxUnauthenticated)
	s.mu.Unlock()
	return nil
}

func versionWord(v string) string {
	out := []byte{}
	for _, c := range []byte(v) {
		if c > ' ' && c < 0x7f && c != '-' {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return "dev"
	}
	return string(out)
}

func (s *Server) logf(level slog.Level, msg string, args ...any) {
	if s.Log != nil {
		s.Log.Log(context.Background(), level, msg, args...)
	}
}

func (s *Server) Serve(l net.Listener) error {
	var delay time.Duration
	for {
		c, err := l.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			time.Sleep(delay)
			continue
		}
		delay = 0
		go s.ServeConn(c)
	}
}

func (s *Server) ServeConn(c net.Conn) {
	s.mu.Lock()
	cfg, closed, pending := s.config, s.closed, s.pending
	s.mu.Unlock()
	if cfg == nil || closed {
		c.Close()
		return
	}
	select {
	case pending <- struct{}{}:
	default:
		c.Close()
		return
	}
	c.SetDeadline(time.Now().Add(LoginGrace))
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	<-pending
	if err != nil {
		c.Close()
		return
	}
	c.SetDeadline(time.Time{})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		conn.Close()
		return
	}
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()
	cs := &connState{s: s, conn: conn, login: conn.User()}
	defer cs.closeAll()
	go cs.global(reqs)
	for nc := range chans {
		switch nc.ChannelType() {
		case "session":
			go cs.session(nc)
		case "direct-tcpip":
			go cs.directTCPIP(nc)
		default:
			nc.Reject(ssh.UnknownChannelType, fmt.Sprintf("fev does not open %s channels", nc.ChannelType()))
		}
	}
	conn.Wait()
}

func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	conns := s.conns
	s.conns = map[*ssh.ServerConn]struct{}{}
	s.mu.Unlock()
	for c := range conns {
		c.Close()
	}
}

type connState struct {
	s     *Server
	conn  *ssh.ServerConn
	login string

	mu      sync.Mutex
	target  *Target
	targErr error
	procs   map[Process]struct{}
	ended   bool
}

func (cs *connState) resolve() (Target, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.target == nil && cs.targErr == nil {
		t, err := cs.s.Backend.Resolve(cs.login)
		if err != nil {
			cs.targErr = err
		} else {
			cs.target = &t
		}
	}
	if cs.targErr != nil {
		return Target{}, cs.targErr
	}
	return *cs.target, nil
}

func (cs *connState) track(p Process) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.ended {
		return false
	}
	if cs.procs == nil {
		cs.procs = map[Process]struct{}{}
	}
	cs.procs[p] = struct{}{}
	return true
}

func (cs *connState) untrack(p Process) {
	cs.mu.Lock()
	delete(cs.procs, p)
	cs.mu.Unlock()
}

func (cs *connState) closeAll() {
	cs.mu.Lock()
	cs.ended = true
	procs := cs.procs
	cs.procs = nil
	cs.mu.Unlock()
	for p := range procs {
		p.Close()
	}
}

func (cs *connState) global(reqs <-chan *ssh.Request) {
	for r := range reqs {
		if r.WantReply {
			r.Reply(r.Type == "keepalive@openssh.com", nil)
		}
	}
}

func (cs *connState) directTCPIP(nc ssh.NewChannel) {
	var p struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil {
		nc.Reject(ssh.ConnectionFailed, "malformed direct-tcpip request")
		return
	}
	addr, err := ForwardAddress(p.Host, p.Port)
	if err != nil {
		nc.Reject(ssh.Prohibited, err.Error())
		return
	}
	t, err := cs.resolve()
	if err != nil {
		nc.Reject(ssh.Prohibited, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialBound)
	peer, err := cs.s.Backend.Dial(ctx, t, addr)
	cancel()
	if err != nil {
		nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		peer.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		io.Copy(peer, ch)
		if hc, ok := peer.(interface{ CloseWrite() error }); ok {
			hc.CloseWrite()
		} else {
			peer.Close()
		}
	}()
	io.Copy(ch, peer)
	ch.CloseWrite()
	wg.Wait()
	ch.Close()
	peer.Close()
}

const dialBound = 3 * time.Minute

func ForwardAddress(host string, port uint32) (netip.AddrPort, error) {
	if port == 0 || port > 65535 {
		return netip.AddrPort{}, fmt.Errorf("port %d is not a port", port)
	}
	if host == "localhost" || host == "localhost." {
		host = "127.0.0.1"
	}
	a, err := netip.ParseAddr(host)
	if err != nil || a.Zone() != "" {
		return netip.AddrPort{}, fmt.Errorf("fev forwards to localhost and to addresses, not to the name %q", host)
	}
	return netip.AddrPortFrom(a.Unmap(), uint16(port)), nil
}
