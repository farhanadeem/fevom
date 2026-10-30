package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"
	"fev/proto/relay"

	"fev/guest/internal/reaper"
)

type Context interface {
	In(fn func(cgroup int) error) error
}

type Selector interface {
	Context
	Select(p api.ExecStartParams) (Context, error)
}

type Here struct{}

func (Here) In(fn func(cgroup int) error) error { return fn(-1) }

const (
	AttachTimeout = 10 * time.Second
	DrainTimeout  = time.Second
)

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/system/bin"

const MaxSessions = 256

type Manager struct {
	reaper   *reaper.Reaper
	entry    string
	contexts map[string]Context
	logf     func(format string, args ...any)
	after    func(d time.Duration, f func()) (cancel func())

	mu       sync.Mutex
	sessions map[uint64]*session
	tokens   map[string]*session
	next     uint64
	closed   bool
}

func NewManager(r *reaper.Reaper, contexts map[string]Context, logf func(format string, args ...any)) *Manager {
	entry, err := os.Executable()
	if err != nil {
		entry = "/init"
	}
	return &Manager{
		reaper: r, contexts: contexts, logf: logf, entry: entry,
		after: func(d time.Duration, f func()) func() {
			t := time.AfterFunc(d, f)
			return func() { t.Stop() }
		},
		sessions: map[uint64]*session{}, tokens: map[string]*session{},
	}
}

type session struct {
	id      uint64
	context string
	p       api.ExecStartParams
	exited  func(api.ExecExit)
	expire  func()
	done    chan struct{}

	child     *reaper.Child
	master    *os.File
	group     string
	forgotten bool
	rows      int
	cols      int
	onExit    func(*api.ExecExit)
}

func (m *Manager) Start(p api.ExecStartParams, exited func(api.ExecExit)) (*api.ExecStarted, error) {
	if _, ok := m.contexts[p.Context]; !ok {
		return nil, fmt.Errorf("no context %q", p.Context)
	}
	var raw [relay.TokenLen / 2]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	s := &session{context: p.Context, p: p, exited: exited, done: make(chan struct{}), rows: p.Rows, cols: p.Cols}
	token := hex.EncodeToString(raw[:])

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("the guest is shutting down")
	}
	if len(m.sessions) >= MaxSessions {
		return nil, fmt.Errorf("%d sessions already exist", MaxSessions)
	}
	m.next++
	s.id = m.next
	m.sessions[s.id] = s
	m.tokens[token] = s
	s.expire = m.after(AttachTimeout, func() {
		m.mu.Lock()
		waiting := m.tokens[token] == s
		if waiting {
			delete(m.tokens, token)
			delete(m.sessions, s.id)
		}
		m.mu.Unlock()
		if waiting {
			close(s.done)
			exited(api.ExecExit{ID: s.id, ExitCode: -1, Error: fmt.Sprintf("no stream attached within %v", AttachTimeout)})
		}
	})
	return &api.ExecStarted{ID: s.id, Token: token}, nil
}

func (m *Manager) Attach(token string, conn net.Conn) {
	m.mu.Lock()
	s := m.tokens[token]
	delete(m.tokens, token)
	m.mu.Unlock()
	if s == nil {
		relay.WriteError(conn, relay.CodeDenied, "no such session")
		return
	}
	s.expire()
	defer close(s.done)

	run, err := m.spawn(s)
	if err != nil {
		relay.WriteError(conn, relay.CodeFailed, err.Error())
		x := api.ExecExit{ID: s.id, ExitCode: -1, Error: err.Error()}
		if errors.Is(err, api.ErrDebugToolsUnavailable) {
			x.Reason = api.ExecReasonToolsUnavailable
		}
		m.finish(s, x)
		return
	}
	if relay.WriteOK(conn) != nil {
		m.reaper.Signal(run.child, unix.SIGKILL, true)
	}
	status := run.serve(m, conn)
	if run.group != "" {
		if !killGroup(run.group, killWait) {
			m.logf("session %d: processes left in %s will not die", s.id, run.group)
		}
		unix.Rmdir(run.group)
	}
	exit := api.ExecExit{ID: s.id, ExitCode: status.ExitStatus()}
	if status.Signaled() {
		exit.ExitCode, exit.Signal = -1, int(status.Signal())
	}
	m.finish(s, exit)
}

func (m *Manager) finish(s *session, exit api.ExecExit) {
	m.mu.Lock()
	delete(m.sessions, s.id)
	forgotten := s.forgotten
	if s.master != nil {
		s.master.Close()
		s.master = nil
	}
	onExit := s.onExit
	m.mu.Unlock()
	if onExit != nil {
		onExit(&exit)
	}
	if !forgotten {
		s.exited(exit)
	}
}

type running struct {
	child                 *reaper.Child
	master                *os.File
	stdin, stdout, stderr *os.File
	stdinTTY              bool
	group                 string
}

func lookPath(file string, env []string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil
	}
	path := defaultPath
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, file)
		if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s: not found in %s", file, path)
}

func openTerminal(ptmx string, rows, cols int) (master, slave *os.File, err error) {
	if ptmx == "" {
		ptmx = "/dev/ptmx"
	}
	fd, err := openPtmx(ptmx, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", ptmx, err)
	}
	master = os.NewFile(uintptr(fd), "ptmx")
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFCHR || unix.Major(uint64(st.Rdev)) != 5 || unix.Minor(uint64(st.Rdev)) != 2 {
		master.Close()
		return nil, nil, fmt.Errorf("%s is not a terminal multiplexer", ptmx)
	}
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("unlock terminal: %w", err)
	}
	if rows > 0 && cols > 0 {
		unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)})
	}
	sfd, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TIOCGPTPEER, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC)
	if errno != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("open terminal's other side: %w", errno)
	}
	return master, os.NewFile(sfd, "pts"), nil
}

func openPtmx(ptmx string, flags int) (int, error) {
	rest, ok := strings.CutPrefix(ptmx, "/proc/")
	pid, inRoot, ok2 := strings.Cut(rest, "/root/")
	if !ok || !ok2 || pid == "" || strings.Trim(pid, "0123456789") != "" {
		return unix.Open(ptmx, flags, 0)
	}
	root, err := unix.Open("/proc/"+pid+"/root", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	defer unix.Close(root)
	return unix.Openat2(root, inRoot, &unix.OpenHow{Flags: uint64(flags), Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS})
}

func ownTerminal(slave *os.File, ent *Entry) {
	if ent.Ptmx != "" && ent.Cred != nil {
		unix.Fchown(int(slave.Fd()), int(ent.IDBase+ent.Cred.UID), -1)
	}
}

func (m *Manager) spawn(s *session) (*running, error) {
	run := &running{}
	ctx := m.contexts[s.context]
	if sel, ok := ctx.(Selector); ok {
		var err error
		if ctx, err = sel.Select(s.p); err != nil {
			return nil, err
		}
	}
	ent := &Entry{Argv: s.p.Argv, Env: s.p.Env}
	if ec, ok := ctx.(Entered); ok {
		e, err := ec.Entry(s.p)
		if err != nil {
			return nil, err
		}
		ent = e
		ent.Env = mergeEnv(ent.Env, s.p.Env)
	}
	defer ent.release()
	m.mu.Lock()
	s.onExit = ent.OnExit
	m.mu.Unlock()
	spec := entrySpec{Argv: ent.Argv, Env: ent.Env, Dir: s.p.Dir, Home: ent.Home, Cred: ent.Cred, RuntimeDir: ent.RuntimeDir, Name: ent.Name, Path: ent.Path, Copy: ent.Copy}
	if spec.Copy != "" && (ent.UserNS == nil || s.p.TTY || s.p.StdinTTY) {
		return nil, errors.New("a copy over the session is an isolated machine's, with no terminal")
	}
	if spec.Env == nil {
		spec.Env = []string{}
	}
	if spec.Dir == "" && spec.Home == "" {
		spec.Dir = "/"
	}

	sys := &syscall.SysProcAttr{Setsid: true}
	attr := &os.ProcAttr{Dir: "/", Env: []string{}, Sys: sys}
	var theirs []*os.File
	closeAll := func(files ...*os.File) {
		for _, f := range files {
			if f != nil {
				f.Close()
			}
		}
	}
	m.mu.Lock()
	rows, cols := s.rows, s.cols
	m.mu.Unlock()
	switch {
	case s.p.TTY:
		master, slave, err := openTerminal(ent.Ptmx, rows, cols)
		if err != nil {
			return nil, err
		}
		ownTerminal(slave, ent)
		run.master = master
		theirs = []*os.File{slave}
		attr.Files = []*os.File{slave, slave, slave}
		sys.Setctty, sys.Ctty = true, 0
	default:
		var pipes [3][2]*os.File
		for i := range pipes {
			r, w, err := os.Pipe()
			if err != nil {
				closeAll(run.stdin, run.stdout, run.stderr)
				return nil, err
			}
			pipes[i] = [2]*os.File{r, w}
		}
		run.stdin, run.stdout, run.stderr = pipes[0][1], pipes[1][0], pipes[2][0]
		theirs = []*os.File{pipes[0][0], pipes[1][1], pipes[2][1]}
		if s.p.StdinTTY {
			master, slave, err := openTerminal(ent.Ptmx, rows, cols)
			if err != nil {
				closeAll(append(theirs, run.stdin, run.stdout, run.stderr)...)
				return nil, err
			}
			ownTerminal(slave, ent)
			pipes[0][0].Close()
			run.stdin.Close()
			run.stdin, run.master, run.stdinTTY = master, master, true
			theirs[0] = slave
			sys.Setctty, sys.Ctty = true, 0
		}
		attr.Files = append([]*os.File{}, theirs...)
	}
	fail := func(err error) (*running, error) {
		closeAll(theirs...)
		closeAll(run.master, run.stdin, run.stdout, run.stderr)
		return nil, err
	}
	specR, specW, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	theirs = append(theirs, specR)
	for len(attr.Files) < entrySpecFd {
		attr.Files = append(attr.Files, attr.Files[len(attr.Files)-1])
	}
	attr.Files = append(attr.Files, specR)
	if ent.Debug != nil {
		spec.Debug = ent.Debug
		attr.Files = append(attr.Files, ent.Extra...)
		theirs = append(theirs, ent.Extra...)
	}
	if ent.MountNS != nil {
		spec.Enter = true
		attr.Files = append(attr.Files, ent.MountNS, ent.CgroupNS)
		if ent.UserNS != nil {
			stage, err := os.Open(ent.Stage)
			if err != nil {
				specW.Close()
				return fail(fmt.Errorf("the isolated machine's stage: %w", err))
			}
			defer stage.Close()
			self, err := os.Open(m.entry)
			if err != nil {
				specW.Close()
				return fail(err)
			}
			defer self.Close()
			spec.EnterUser, spec.Isolate = true, ent.Isolate
			attr.Files = append(attr.Files, ent.UserNS, stage, self)
		}
	}
	entryPath := m.entry
	if ep, ok := ctx.(EntryPather); ok {
		entryPath = ep.EntryPath()
	}
	group := ""
	var groupFd *os.File
	if g, ok := ctx.(Grouped); ok {
		if group, err = m.newGroup(g.SessionGroups()); err != nil {
			specW.Close()
			return fail(err)
		}
		if groupFd, err = os.Open(group); err != nil {
			unix.Rmdir(group)
			specW.Close()
			return fail(err)
		}
		defer groupFd.Close()
	}
	err = ctx.In(func(cgroup int) error {
		if spec.Path == "" && !ent.Find {
			path, err := lookPath(spec.Argv[0], spec.Env)
			if err != nil {
				return err
			}
			spec.Path = path
		}
		if groupFd != nil {
			cgroup = int(groupFd.Fd())
		}
		if cgroup >= 0 {
			sys.UseCgroupFD, sys.CgroupFD = true, cgroup
		}
		b, _ := json.Marshal(spec)
		if ent.Started == nil {
			if _, err := specW.Write(b); err != nil {
				return err
			}
			specW.Close()
			run.child, err = m.reaper.Start(entryPath, []string{EntryName}, attr)
			return err
		}
		if run.child, err = m.reaper.Start(entryPath, []string{EntryName}, attr); err != nil {
			return err
		}
		ent.Started(run.child.Pid)
		if _, err := specW.Write(b); err != nil {
			m.reaper.Signal(run.child, unix.SIGKILL, true)
			return err
		}
		specW.Close()
		return nil
	})
	specW.Close()
	if err != nil {
		if group != "" {
			unix.Rmdir(group)
		}
		return fail(err)
	}
	closeAll(theirs...)
	run.group = group
	m.mu.Lock()
	s.child, s.master, s.group = run.child, run.master, group
	forgotten := s.forgotten
	m.mu.Unlock()
	if forgotten {
		m.reaper.Signal(run.child, unix.SIGKILL, true)
	}
	return run, nil
}

func (r *running) serve(m *Manager, conn net.Conn) unix.WaitStatus {
	defer conn.Close()
	var out sync.WaitGroup
	if r.master != nil && !r.stdinTTY {
		go func() {
			io.Copy(r.master, conn)
			r.master.Close()
		}()
		out.Add(1)
		go func() {
			defer out.Done()
			if _, err := io.Copy(conn, r.master); err != nil && !errors.Is(err, unix.EIO) && !errors.Is(err, os.ErrClosed) {
				r.master.Close()
			}
		}()
	} else {
		var write sync.Mutex
		go func() {
			buf := make([]byte, relay.MaxFrame)
			for {
				stream, n, err := relay.ReadFrame(conn, buf)
				if err != nil || stream != relay.Stdin {
					r.stdin.Close()
					m.reaper.Signal(r.child, unix.SIGKILL, true)
					return
				}
				if n == 0 {
					if r.stdinTTY {
						r.stdin.Write([]byte{4})
						continue
					}
					r.stdin.Close()
					continue
				}
				r.stdin.Write(buf[:n])
			}
		}()
		if r.stdinTTY {
			go io.Copy(io.Discard, r.master)
		}
		for stream, f := range map[byte]*os.File{relay.Stdout: r.stdout, relay.Stderr: r.stderr} {
			out.Add(1)
			go func() {
				defer out.Done()
				buf := make([]byte, 32<<10)
				for {
					n, err := f.Read(buf)
					if n > 0 {
						write.Lock()
						werr := relay.WriteFrame(conn, stream, buf[:n])
						write.Unlock()
						if werr != nil {
							m.reaper.Signal(r.child, unix.SIGKILL, true)
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}

	status := r.child.Wait()
	drained := make(chan struct{})
	go func() { out.Wait(); close(drained) }()
	timer := time.NewTimer(DrainTimeout)
	select {
	case <-drained:
		timer.Stop()
	case <-timer.C:
	}
	for _, f := range []*os.File{r.master, r.stdin, r.stdout, r.stderr} {
		if f != nil {
			f.Close()
		}
	}
	<-drained
	return status
}

func (m *Manager) get(id uint64) (*session, error) {
	s := m.sessions[id]
	if s == nil {
		return nil, fmt.Errorf("no session %d", id)
	}
	return s, nil
}

func (m *Manager) Resize(id uint64, rows, cols int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.get(id)
	if err != nil {
		return err
	}
	if !s.p.TTY && !s.p.StdinTTY {
		return fmt.Errorf("session %d has no terminal", id)
	}
	s.rows, s.cols = rows, cols
	if s.master == nil {
		return nil
	}
	conn, err := s.master.SyscallConn()
	if err != nil {
		return err
	}
	var ioerr error
	if err := conn.Control(func(fd uintptr) {
		ioerr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)})
	}); err != nil {
		return err
	}
	return ioerr
}

func (m *Manager) Signal(id uint64, sig int) error {
	m.mu.Lock()
	s, err := m.get(id)
	var child *reaper.Child
	if err == nil {
		child = s.child
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if child == nil || !m.reaper.Signal(child, unix.Signal(sig), true) {
		return fmt.Errorf("session %d is not running", id)
	}
	return nil
}

func (m *Manager) Close(id uint64) error {
	m.mu.Lock()
	s, err := m.get(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	s.forgotten = true
	delete(m.sessions, id)
	child := s.child
	waiting := false
	for token, t := range m.tokens {
		if t == s {
			delete(m.tokens, token)
			waiting = true
		}
	}
	m.mu.Unlock()
	if waiting {
		s.expire()
		close(s.done)
	}
	if child != nil {
		m.reaper.Signal(child, unix.SIGKILL, true)
	}
	return nil
}

func (m *Manager) Kill(context string) {
	m.mu.Lock()
	var children []*reaper.Child
	for _, s := range m.sessions {
		if s.context == context && s.child != nil {
			children = append(children, s.child)
		}
	}
	m.mu.Unlock()
	for _, c := range children {
		m.reaper.Signal(c, unix.SIGKILL, true)
	}
}

func (m *Manager) Shutdown(limit time.Duration) (left int) {
	m.mu.Lock()
	m.closed = true
	var all []*session
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		m.mu.Lock()
		child, group := s.child, s.group
		m.mu.Unlock()
		if group != "" {
			os.WriteFile(filepath.Join(group, "cgroup.kill"), []byte("1"), 0)
		}
		if child == nil || !m.reaper.Signal(child, unix.SIGKILL, true) {
			m.Close(s.id)
		}
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for i, s := range all {
		select {
		case <-s.done:
		case <-timer.C:
			return len(all) - i
		}
	}
	return 0
}

func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}
