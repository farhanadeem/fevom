package sshfront

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"fev/proto/agent"
	"fev/proto/relay"
)

var AcceptEnv = []string{"COLORTERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "TZ"}

var signals = map[string]int{
	"ABRT": 6, "ALRM": 14, "FPE": 8, "HUP": 1, "ILL": 4, "INT": 2, "KILL": 9,
	"PIPE": 13, "QUIT": 3, "SEGV": 11, "TERM": 15, "USR1": 10, "USR2": 12,
}

func signalName(n int) string {
	for name, v := range signals {
		if v == n {
			return name
		}
	}
	return ""
}

type sessionState struct {
	cs *connState
	ch ssh.Channel

	mu      sync.Mutex
	tty     bool
	term    string
	rows    int
	cols    int
	env     []string
	started bool
	proc    Process
	once    sync.Once
}

func (cs *connState) session(nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	ss := &sessionState{cs: cs, ch: ch}
	for r := range reqs {
		ss.request(r)
	}
	ss.mu.Lock()
	p := ss.proc
	ss.mu.Unlock()
	if p != nil {
		p.Close()
		cs.untrack(p)
	}
}

func (ss *sessionState) request(r *ssh.Request) {
	reply := func(ok bool) {
		if r.WantReply {
			r.Reply(ok, nil)
		}
	}
	switch r.Type {
	case "pty-req":
		var p struct {
			Term          string
			Cols, Rows    uint32
			Width, Height uint32
			Modes         string
		}
		if ssh.Unmarshal(r.Payload, &p) != nil || strings.ContainsRune(p.Term, 0) {
			reply(false)
			return
		}
		ss.mu.Lock()
		ok := !ss.started
		if ok {
			ss.tty, ss.term, ss.rows, ss.cols = true, p.Term, clampWindow(p.Rows), clampWindow(p.Cols)
		}
		ss.mu.Unlock()
		reply(ok)
	case "env":
		var p struct{ Name, Value string }
		if ssh.Unmarshal(r.Payload, &p) != nil {
			reply(false)
			return
		}
		ok := false
		for _, name := range AcceptEnv {
			ok = ok || p.Name == name
		}
		if ok && !strings.ContainsRune(p.Value, 0) {
			ss.mu.Lock()
			ss.env = append(ss.env, p.Name+"="+p.Value)
			ss.mu.Unlock()
		}
		reply(ok)
	case "window-change":
		var p struct{ Cols, Rows, Width, Height uint32 }
		if ssh.Unmarshal(r.Payload, &p) != nil {
			return
		}
		ss.mu.Lock()
		ss.rows, ss.cols = clampWindow(p.Rows), clampWindow(p.Cols)
		proc, tty := ss.proc, ss.tty
		ss.mu.Unlock()
		if proc != nil && tty {
			proc.Resize(clampWindow(p.Rows), clampWindow(p.Cols))
		}
	case "signal":
		var p struct{ Signal string }
		if ssh.Unmarshal(r.Payload, &p) != nil {
			return
		}
		ss.mu.Lock()
		proc := ss.proc
		ss.mu.Unlock()
		if n, ok := signals[strings.TrimPrefix(p.Signal, "SIG")]; ok && proc != nil {
			proc.Signal(n)
		}
		reply(true)
	case "auth-agent-req@openssh.com":
		reply(true)
	case "shell", "exec", "subsystem":
		var req ExecRequest
		switch r.Type {
		case "shell":
			req.Login = true
		case "exec":
			var p struct{ Command string }
			if ssh.Unmarshal(r.Payload, &p) != nil {
				reply(false)
				return
			}
			req.Shell, req.Argv = true, []string{p.Command}
		case "subsystem":
			var p struct{ Name string }
			if ssh.Unmarshal(r.Payload, &p) != nil || p.Name != agent.SubsystemSFTP {
				reply(false)
				return
			}
			req.Subsystem = agent.SubsystemSFTP
		}
		ss.mu.Lock()
		if ss.started {
			ss.mu.Unlock()
			reply(false)
			return
		}
		ss.started = true
		if req.Subsystem == "" {
			req.TTY, req.Rows, req.Cols = ss.tty, ss.rows, ss.cols
		}
		req.Env = append(ss.connEnv(), ss.env...)
		if ss.tty && req.Subsystem == "" {
			term := ss.term
			if term == "" {
				term = "vt100"
			}
			req.Env = append(req.Env, "TERM="+term)
		}
		ss.mu.Unlock()
		reply(true)
		go ss.run(req)
	default:
		reply(false)
	}
}

func clampWindow(n uint32) int { return int(min(n, 10000)) }

func (ss *sessionState) connEnv() []string {
	ra, ok1 := ss.cs.conn.RemoteAddr().(*net.TCPAddr)
	la, ok2 := ss.cs.conn.LocalAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return []string{"SSH_CONNECTION=::1 0 ::1 22", "SSH_CLIENT=::1 0 22"}
	}
	rip, lip := ra.AddrPort().Addr().Unmap(), la.AddrPort().Addr().Unmap()
	return []string{
		fmt.Sprintf("SSH_CONNECTION=%s %d %s %d", rip, ra.Port, lip, la.Port),
		fmt.Sprintf("SSH_CLIENT=%s %d %d", rip, ra.Port, la.Port),
	}
}

func (ss *sessionState) run(req ExecRequest) {
	t, err := ss.cs.resolve()
	if err != nil {
		ss.fail(err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialBound)
	proc, err := ss.cs.s.Backend.Exec(ctx, t, req)
	cancel()
	if err != nil {
		ss.fail(err)
		return
	}
	if !ss.cs.track(proc) {
		proc.Close()
		return
	}
	ss.mu.Lock()
	ss.proc = proc
	rows, cols := ss.rows, ss.cols
	ss.mu.Unlock()
	if req.TTY && (rows != req.Rows || cols != req.Cols) {
		proc.Resize(rows, cols)
	}
	stream := proc.Stream()
	if req.TTY {
		go func() {
			io.Copy(stream, ss.ch)
		}()
		io.Copy(ss.ch, stream)
	} else {
		var wmu sync.Mutex
		go func() {
			buf := make([]byte, relay.MaxFrame)
			for {
				n, err := ss.ch.Read(buf)
				if n > 0 {
					wmu.Lock()
					werr := relay.WriteFrame(stream, relay.Stdin, buf[:n])
					wmu.Unlock()
					if werr != nil {
						return
					}
				}
				if err != nil {
					if err == io.EOF {
						wmu.Lock()
						relay.WriteFrame(stream, relay.Stdin, nil)
						wmu.Unlock()
					}
					return
				}
			}
		}()
		buf := make([]byte, relay.MaxFrame)
		for {
			kind, n, err := relay.ReadFrame(stream, buf)
			if err != nil {
				break
			}
			w := io.Writer(ss.ch)
			if kind == relay.Stderr {
				w = ss.ch.Stderr()
			}
			if _, err := w.Write(buf[:n]); err != nil {
				proc.Close()
				break
			}
		}
	}
	exit := <-proc.Exit()
	ss.cs.untrack(proc)
	ss.end(exit)
}

func (ss *sessionState) fail(err error) {
	ss.end(Exit{Code: 1, Err: err.Error()})
}

func (ss *sessionState) newline() string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.tty {
		return "\r\n"
	}
	return "\n"
}

func (ss *sessionState) end(e Exit) {
	ss.once.Do(func() {
		switch {
		case e.Err != "":
			fmt.Fprintf(ss.ch.Stderr(), "fev: %s%s", e.Err, ss.newline())
			ss.ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{1}))
		case e.Signal != 0 && signalName(e.Signal) != "":
			ss.ch.SendRequest("exit-signal", false, ssh.Marshal(struct {
				Signal     string
				CoreDumped bool
				Message    string
				Lang       string
			}{signalName(e.Signal), false, "", ""}))
		case e.Signal != 0:
			ss.ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(128 + e.Signal)}))
		case e.Code < 0 || e.Code > 255:
			ss.ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{255}))
		default:
			ss.ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(e.Code)}))
		}
		ss.ch.CloseWrite()
		ss.ch.Close()
	})
}
