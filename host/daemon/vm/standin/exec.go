package standin

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
	"fev/proto/relay"
)

const attachBound = 10 * time.Second

type fakeSession struct {
	id      uint64
	token   string
	conn    *jsonrpc.Conn
	params  api.ExecStartParams
	user    string
	machine string
	debug   *api.DebugStarted

	mu       sync.Mutex
	rows     int
	cols     int
	attached bool
	ended    bool
	signal   chan int
	timer    *time.Timer
	over     chan struct{}
}

func newToken() string {
	var b [relay.TokenLen / 2]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (a *Agent) execStart(c *jsonrpc.Conn, p api.ExecStartParams) (any, error) {
	machineOnly := p.Machine != "" || p.User != "" || p.Login || p.Shell || p.Subsystem != ""
	switch p.Context {
	case api.ExecInit, api.ExecEngine:
		if len(p.Argv) == 0 || machineOnly {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "exec.start in %s's context needs argv and none of machine, user, login or shell", p.Context)
		}
	case api.ExecMachine:
		if p.Subsystem != "" {
			if p.Subsystem != api.SubsystemSFTP || len(p.Argv) != 0 || p.Login || p.Shell || p.TTY || p.StdinTTY {
				return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "exec.start of a subsystem names %q and no argv, login, shell or terminal", api.SubsystemSFTP)
			}
		} else if (p.Login && !p.Shell) != (len(p.Argv) == 0) {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "exec.start in a machine needs argv, or login and no argv")
		}
		if err := api.CheckMachineName(p.Machine); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
	case api.ExecDebug:
		if machineOnly || p.Copy != "" {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "exec.start in debug's context takes none of machine, user, login, shell, subsystem or copy")
		}
		if err := api.CheckDebugStart(p); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
	default:
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "exec.start needs a context of %q, %q, %q or %q", api.ExecInit, api.ExecEngine, api.ExecMachine, api.ExecDebug)
	}
	var dbg *api.DebugStarted
	if p.Context == api.ExecDebug {
		d, err := a.resolveDebug(p.Debug)
		if err != nil {
			return nil, err
		}
		dbg = d
	}
	if p.TTY && p.StdinTTY {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "tty and stdin_tty are not both set")
	}
	if err := api.CheckExecTerminfo(p); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	if err := api.CheckExecCopy(p); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	user := "root"
	if p.Context == api.ExecMachine {
		m := a.machines()
		m.mu.Lock()
		fm := m.byName[p.Machine]
		running := fm != nil && fm.Info.State == api.MachineRunning
		isolated := false
		if fm != nil {
			user, isolated = fm.Config.User, fm.Config.Isolated
		}
		m.mu.Unlock()
		switch {
		case fm == nil:
			return nil, jsonrpc.Errorf(api.CodeNoMachine, "no machine %q", p.Machine)
		case !running:
			return nil, jsonrpc.Errorf(api.CodeFailed, "the machine %s is not running", p.Machine)
		}
		if p.User != "" {
			user = p.User
		}
		if p.Copy != "" && !isolated {
			return nil, jsonrpc.Errorf(api.CodeFailed, "a copy over the session is an isolated machine's; %s copies through /mnt/mac", p.Machine)
		}
		if user != "root" && user != fmUser(m, p.Machine) {
			return nil, jsonrpc.Errorf(api.CodeFailed, "the machine %s has no user %q", p.Machine, user)
		}
	}
	a.mu.Lock()
	if p.Context == api.ExecEngine && a.engine.State != api.EngineReady {
		a.mu.Unlock()
		return nil, jsonrpc.Errorf(api.CodeFailed, "the engine is not ready")
	}
	a.nextID++
	s := &fakeSession{id: a.nextID, token: newToken(), conn: c, params: p, user: user, machine: p.Machine, debug: dbg, rows: p.Rows, cols: p.Cols, signal: make(chan int, 1), over: make(chan struct{})}
	if a.sessions == nil {
		a.sessions = map[uint64]*fakeSession{}
	}
	a.sessions[s.id] = s
	a.mu.Unlock()
	s.timer = time.AfterFunc(attachBound, func() {
		if a.endSession(s, true) {
			c.Notify(api.NotifyExecExit, api.ExecExit{ID: s.id, ExitCode: -1, Error: "the session's stream was not attached within 10s"})
		}
	})
	go func() {
		select {
		case <-c.Done():
			a.endSession(s, false)
			select {
			case s.signal <- 9:
			default:
			}
		case <-s.over:
		}
	}()
	return api.ExecStarted{ID: s.id, Token: s.token, Debug: s.debug}, nil
}

func fmUser(m *machineTable, name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fm := m.byName[name]; fm != nil {
		return fm.User.Name
	}
	return ""
}

func (a *Agent) endSession(s *fakeSession, unattached bool) bool {
	s.mu.Lock()
	if s.ended || (unattached && s.attached) {
		s.mu.Unlock()
		return false
	}
	s.ended = true
	close(s.over)
	s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
	}
	a.mu.Lock()
	delete(a.sessions, s.id)
	a.mu.Unlock()
	return true
}

func (a *Agent) session(c *jsonrpc.Conn, id uint64) *fakeSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.sessions[id]; s != nil && s.conn == c {
		return s
	}
	return nil
}

func (a *Agent) execCall(c *jsonrpc.Conn, method string, params []byte) (any, error) {
	p, err := jsonrpc.Bind[api.ExecResizeParams](params)
	if err != nil {
		return nil, err
	}
	sig, _ := jsonrpc.Bind[api.ExecSignalParams](params)
	s := a.session(c, p.ID)
	if s == nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "no session %d", p.ID)
	}
	switch method {
	case api.MethodExecResize:
		s.mu.Lock()
		s.rows, s.cols = p.Rows, p.Cols
		s.mu.Unlock()
	case api.MethodExecSignal:
		select {
		case s.signal <- sig.Signal:
		default:
		}
	case api.MethodExecClose:
		a.endSession(s, false)
		select {
		case s.signal <- 9:
		default:
		}
	}
	return api.Empty{}, nil
}

func (a *Agent) AttachExec(token string, conn net.Conn) bool {
	a.mu.Lock()
	var s *fakeSession
	for _, x := range a.sessions {
		if x.token == token {
			s = x
		}
	}
	a.mu.Unlock()
	if s == nil {
		return false
	}
	s.mu.Lock()
	if s.attached || s.ended {
		s.mu.Unlock()
		return false
	}
	s.attached = true
	s.mu.Unlock()
	s.timer.Stop()
	if relay.WriteOK(conn) != nil {
		a.endSession(s, false)
		return true
	}
	code, signal := s.run(conn)
	conn.Close()
	if a.endSession(s, false) {
		exit := api.ExecExit{ID: s.id, ExitCode: code}
		if signal != 0 {
			exit.ExitCode, exit.Signal = -1, signal
		}
		s.conn.Notify(api.NotifyExecExit, exit)
	}
	return true
}

type out struct {
	conn  net.Conn
	raw   bool
	mu    sync.Mutex
	stdin chan []byte
}

func (o *out) write(stream byte, p []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.raw {
		o.conn.Write(p)
		return
	}
	relay.WriteFrame(o.conn, stream, p)
}

func (o *out) read(in chan<- []byte) {
	defer close(in)
	if o.raw {
		buf := make([]byte, 32<<10)
		for {
			n, err := o.conn.Read(buf)
			if n > 0 {
				in <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}
	buf := make([]byte, relay.MaxFrame)
	for {
		stream, n, err := relay.ReadFrame(o.conn, buf)
		if err != nil || (stream == relay.Stdin && n == 0) {
			return
		}
		if stream == relay.Stdin {
			in <- append([]byte(nil), buf[:n]...)
		}
	}
}

func (s *fakeSession) run(conn net.Conn) (code, signal int) {
	o := &out{conn: conn, raw: s.params.TTY}
	in := make(chan []byte, 16)
	go o.read(in)
	if s.params.Subsystem == api.SubsystemSFTP {
		return s.sftp(o, in)
	}
	if s.params.Copy != "" {
		if s.params.Copy == api.CopyOut {
			var b bytes.Buffer
			tar.NewWriter(&b).Close()
			o.write(relay.Stdout, b.Bytes())
			return 0, 0
		}
		for range in {
		}
		return 0, 0
	}
	argv := s.params.Argv
	if s.params.Shell {
		argv = shellWords(strings.Join(argv, " "))
	}
	if s.params.Login && !s.params.Shell {
		argv = []string{"cat"}
	}
	if s.params.Context == api.ExecDebug && len(argv) == 0 {
		argv = []string{"cat"}
	}
	if len(argv) == 0 {
		return 0, 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := 0
	go func() {
		select {
		case sig = <-s.signal:
			cancel()
		case <-ctx.Done():
		}
	}()
	arg := func(i int) int {
		if i < len(argv) {
			n, _ := strconv.Atoi(argv[i])
			return n
		}
		return 0
	}
	ended := func(code int) (int, int) {
		if ctx.Err() != nil && sig != 0 {
			return -1, sig
		}
		return code, 0
	}
	switch argv[0] {
	case "true":
		return 0, 0
	case "false":
		return 1, 0
	case "exit":
		return arg(1), 0
	case "echo":
		o.write(relay.Stdout, []byte(strings.Join(argv[1:], " ")+"\n"))
	case "stderr":
		o.write(relay.Stderr, []byte(strings.Join(argv[1:], " ")+"\n"))
	case "seq":
		var b bytes.Buffer
		for i := 1; i <= arg(1); i++ {
			fmt.Fprintf(&b, "%d\n", i)
			if b.Len() >= 32<<10 {
				o.write(relay.Stdout, b.Bytes())
				b.Reset()
			}
		}
		if b.Len() > 0 {
			o.write(relay.Stdout, b.Bytes())
		}
	case "bytes":
		chunk := bytes.Repeat([]byte("fev-standin-0123456789abcdef\n"), 1024)
		for left := arg(1); left > 0 && ctx.Err() == nil; {
			n := min(left, len(chunk))
			o.write(relay.Stdout, chunk[:n])
			left -= n
		}
	case "cat":
		for {
			select {
			case b, ok := <-in:
				if !ok {
					return ended(0)
				}
				o.write(relay.Stdout, b)
			case <-ctx.Done():
				return ended(0)
			}
		}
	case "sleep":
		select {
		case <-time.After(time.Duration(arg(1)) * time.Millisecond):
		case <-ctx.Done():
		}
	case "size":
		s.mu.Lock()
		rows, cols := s.rows, s.cols
		s.mu.Unlock()
		o.write(relay.Stdout, fmt.Appendf(nil, "%d %d\n", rows, cols))
	case "whoami":
		o.write(relay.Stdout, []byte(s.user+"\n"))
	case "pwd":
		o.write(relay.Stdout, []byte(s.params.Dir+"\n"))
	case "env":
		o.write(relay.Stdout, []byte(strings.Join(s.params.Env, "\n")+"\n"))
	case "terminfo":
		if len(s.params.Terminfo) == 0 {
			o.write(relay.Stdout, []byte("none\n"))
		} else {
			o.write(relay.Stdout, fmt.Appendf(nil, "%d bytes for %s: %v\n", len(s.params.Terminfo), api.TermOf(s.params.Env), api.CheckTerminfo(api.TermOf(s.params.Env), s.params.Terminfo)))
		}
	default:
		o.write(relay.Stderr, []byte("fev-standin: "+argv[0]+": command not found\n"))
		return 127, 0
	}
	return ended(0)
}

func shellWords(line string) []string {
	var words []string
	var w strings.Builder
	in, quoted, escaped := false, false, false
	for _, r := range line {
		switch {
		case escaped:
			w.WriteRune(r)
			escaped = false
		case r == '\\' && !in:
			escaped = true
		case r == '\'':
			in, quoted = !in, true
		case !in && (r == ' ' || r == '\t' || r == '\n'):
			if w.Len() > 0 || quoted {
				words = append(words, w.String())
			}
			w.Reset()
			quoted = false
		default:
			w.WriteRune(r)
		}
	}
	if w.Len() > 0 || quoted {
		words = append(words, w.String())
	}
	return words
}
