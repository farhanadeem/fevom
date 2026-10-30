package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"fev/host/docker/vsock"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/relay"
)

const attachBound = 10 * time.Second

const holdBound = 10 * time.Second

const ErrNotTaken = "the session's stream was handed to the client and not taken within 10s, so its output may have been lost; the session was ended"

type session struct {
	id      uint64
	token   string
	caller  Caller
	client  *agent.Client
	vm      VM
	machine string

	debug bool

	remote *remoteRun

	mu       sync.Mutex
	execID   uint64
	attached bool
	exited   bool
	timer    *time.Timer
	over     chan struct{}
}

func (t *sessionTable) openDebug() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, s := range t.byID {
		if s.debug && s.remote == nil {
			n++
		}
	}
	return n
}

func (s *session) finish() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return false
	}
	s.exited = true
	if s.timer != nil {
		s.timer.Stop()
	}
	close(s.over)
	return true
}

func (s *session) exit(e api.SessionExit) {
	if !s.finish() {
		return
	}
	e.ID = s.id
	s.caller.Notify(api.NotifySessionExit, e)
}

type sessionTable struct {
	m        *Machines
	listener net.Listener

	mu        sync.Mutex
	nextID    uint64
	byID      map[uint64]*session
	byToken   map[string]*session
	resetting bool
}

func (t *sessionTable) start(m *Machines, home string) error {
	t.m = m
	t.byID, t.byToken = map[uint64]*session{}, map[string]*session{}
	path := api.SessionSocketPath(home)
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return err
	}
	t.listener = l
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go t.attach(conn.(*net.UnixConn))
		}
	}()
	return nil
}

func (t *sessionTable) close() {
	if t.listener != nil {
		t.listener.Close()
	}
}

func (t *sessionTable) forget(s *session) {
	t.mu.Lock()
	_, was := t.byID[s.id]
	delete(t.byID, s.id)
	if t.byToken[s.token] == s {
		delete(t.byToken, s.token)
	}
	t.mu.Unlock()
	if was && s.debug && s.remote == nil && t.m != nil {
		t.m.debugSessionEnded()
	}
}

func (t *sessionTable) kill(s *session) {
	t.forget(s)
	if s.remote != nil {
		s.remote.close()
		return
	}
	s.mu.Lock()
	id := s.execID
	s.mu.Unlock()
	if id == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), machineCallBound)
	defer cancel()
	s.client.ExecClose(ctx, id)
}

func (t *sessionTable) startSession(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.SessionStartParams](params)
	if err != nil {
		return nil, err
	}
	caller, ok := CallerOf(ctx)
	if !ok {
		return nil, jsonrpc.Errorf(api.CodeFailed, "session.start needs a client connection")
	}
	if p.TTY && p.StdinTTY {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "tty and stdin_tty are not both set")
	}
	if err := agent.CheckExecTerminfo(agent.ExecStartParams{Context: agent.ExecMachine, TTY: p.TTY, StdinTTY: p.StdinTTY, Env: p.Env, Terminfo: p.Terminfo}); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	if err := agent.CheckExecCopy(agent.ExecStartParams{Context: agent.ExecMachine, Copy: p.Copy, Argv: p.Argv,
		Login: p.Login, Shell: p.Shell, TTY: p.TTY, StdinTTY: p.StdinTTY, Terminfo: p.Terminfo}); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	if (p.Login && !p.Shell) != (len(p.Argv) == 0) {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "a session runs argv, or a login shell with no argv")
	}
	m := t.m
	name := p.Machine
	if name == "" {
		m.mu.Lock()
		name = m.def
		m.mu.Unlock()
		if name == "" {
			return nil, jsonrpc.Errorf(api.CodeNoMachine, "there is no default machine: make one (fev create) or name one")
		}
	}
	if _, err := m.startMachine(ctx, name); err != nil {
		return nil, err
	}
	user := p.User
	if user == "" {
		user = m.username(name)
	}
	m.mu.Lock()
	client, vm := m.client, m.vm
	m.mu.Unlock()
	if client == nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "the VM stopped as the session was starting")
	}
	t.mu.Lock()
	t.nextID++
	s := &session{id: t.nextID, caller: caller, client: client, vm: vm, machine: name, over: make(chan struct{})}
	t.byID[s.id] = s
	t.mu.Unlock()
	started, err := client.ExecStart(ctx, agent.ExecStartParams{
		Context: agent.ExecMachine, Machine: name, User: user, Argv: p.Argv, Env: p.Env, Dir: p.Dir,
		TTY: p.TTY, StdinTTY: p.StdinTTY, Rows: p.Rows, Cols: p.Cols, Login: p.Login, Shell: p.Shell,
		Terminfo: p.Terminfo, Copy: p.Copy,
	}, t.exited(s))
	if err != nil {
		t.forget(s)
		return nil, guestError(err)
	}
	t.arm(s, started.ID, started.Token)
	return api.SessionStarted{ID: s.id, Token: s.token, Machine: name, User: user}, nil
}

func (t *sessionTable) exited(s *session) func(agent.ExecExit) {
	return func(e agent.ExecExit) {
		t.forget(s)
		s.exit(api.SessionExit{ExitCode: e.ExitCode, Signal: e.Signal, Error: e.Error, Reason: e.Reason, Note: e.Note, StreamEnded: e.Error == ""})
	}
}

func (t *sessionTable) arm(s *session, execID uint64, token string) {
	s.mu.Lock()
	s.execID, s.token = execID, token
	s.timer = time.AfterFunc(attachBound, func() {
		s.mu.Lock()
		attached := s.attached
		s.mu.Unlock()
		if !attached {
			t.kill(s)
			s.exit(api.SessionExit{ExitCode: -1, Error: "the session's stream was not attached within 10s"})
		}
	})
	s.mu.Unlock()
	t.mu.Lock()
	t.byToken[s.token] = s
	t.mu.Unlock()
	s.caller.OnGone(func() {
		if s.finish() {
			t.kill(s)
		}
	})
	go func() {
		select {
		case <-s.client.Done():
			t.forget(s)
			s.exit(api.SessionExit{ExitCode: -1, Error: "the VM stopped"})
		case <-s.over:
		}
	}()
}

func (t *sessionTable) call(method string) MethodFunc {
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		var p struct {
			ID     uint64 `json:"id"`
			Rows   int    `json:"rows"`
			Cols   int    `json:"cols"`
			Signal int    `json:"signal"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
		caller, _ := CallerOf(ctx)
		t.mu.Lock()
		s := t.byID[p.ID]
		t.mu.Unlock()
		if s == nil || s.caller != caller {
			return nil, jsonrpc.Errorf(api.CodeFailed, "no session %d on this connection", p.ID)
		}
		if s.remote != nil {
			switch method {
			case api.MethodSessionResize:
				s.remote.setSize(p.Rows, p.Cols)
			case api.MethodSessionSignal:
			case api.MethodSessionClose:
				s.finish()
				t.forget(s)
				s.remote.close()
			}
			return api.Empty{}, nil
		}
		s.mu.Lock()
		id := s.execID
		s.mu.Unlock()
		var err error
		switch method {
		case api.MethodSessionResize:
			err = s.client.ExecResize(ctx, id, p.Rows, p.Cols)
		case api.MethodSessionSignal:
			err = s.client.ExecSignal(ctx, id, p.Signal)
		case api.MethodSessionClose:
			s.finish()
			t.forget(s)
			err = s.client.ExecClose(ctx, id)
		}
		if err != nil {
			return nil, guestError(err)
		}
		return api.Empty{}, nil
	}
}

func (t *sessionTable) attach(c *net.UnixConn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(attachBound))
	req, err := relay.ReadRequest(c)
	if err != nil {
		relay.WriteError(c, relay.CodeBadRequest, err.Error())
		return
	}
	t.mu.Lock()
	s := t.byToken[req.Target]
	delete(t.byToken, req.Target)
	t.mu.Unlock()
	if req.Kind != relay.Exec || s == nil {
		relay.WriteError(c, relay.CodeDenied, "no such session")
		return
	}
	s.mu.Lock()
	s.attached = true
	s.timer.Stop()
	s.mu.Unlock()
	if s.remote != nil {
		defer s.remote.closeApp()
		c.SetDeadline(time.Time{})
		if err := passStream(c, s.remote.appEnd); err != nil {
			return
		}
		bound := holdBound
		if t.m.holdFor > 0 {
			bound = t.m.holdFor
		}
		if !waitClosed(c, bound) {
			t.m.log.Warn("sessions: a client did not take a remote debug session's stream within the bound; the session is ended", "session", s.id, "client", s.caller.Name())
			t.kill(s)
			s.exit(api.SessionExit{ExitCode: -1, Error: ErrNotTaken})
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), attachBound)
	stream, err := vsock.Relay{Path: s.vm.VsockPath(relay.Port)}.Open(ctx, req)
	cancel()
	if err != nil {
		code, text := relay.CodeFailed, err.Error()
		var re *relay.Error
		if errors.As(err, &re) {
			code, text = re.Code, re.Text
		}
		c.SetDeadline(time.Now().Add(2 * time.Second))
		relay.WriteError(c, code, text)
		t.kill(s)
		s.exit(api.SessionExit{ExitCode: -1, Error: "the session's stream could not be opened: " + text})
		return
	}
	defer stream.Close()
	c.SetDeadline(time.Time{})
	if err := passStream(c, stream); err != nil {
		return
	}
	bound := holdBound
	if t.m.holdFor > 0 {
		bound = t.m.holdFor
	}
	if !waitClosed(c, bound) {
		t.m.log.Warn("sessions: a client did not take a session's stream within the bound; the session is ended", "session", s.id, "client", s.caller.Name())
		t.kill(s)
		s.exit(api.SessionExit{ExitCode: -1, Error: ErrNotTaken})
	}
}

func passStream(c *net.UnixConn, stream *net.UnixConn) error {
	raw, err := stream.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	err = raw.Control(func(fd uintptr) {
		_, _, serr = c.WriteMsgUnix([]byte("ok\n"), syscall.UnixRights(int(fd)), nil)
	})
	if err != nil {
		return err
	}
	return serr
}

func waitClosed(c *net.UnixConn, bound time.Duration) bool {
	c.SetReadDeadline(time.Now().Add(bound))
	var b [64]byte
	for {
		if _, err := c.Read(b[:]); err != nil {
			var ne net.Error
			return !(errors.As(err, &ne) && ne.Timeout())
		}
	}
}
