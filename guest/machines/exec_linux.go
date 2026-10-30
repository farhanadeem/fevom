package machines

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/internal/logbuf"
	"fev/guest/internal/session"
)

type Sessions struct{ rt *Runtime }

func (rt *Runtime) Sessions() Sessions { return Sessions{rt} }

func (Sessions) In(func(int) error) error {
	return errors.New("a session in a machine needs the machine's name")
}

func (s Sessions) Select(p api.ExecStartParams) (session.Context, error) {
	if err := s.rt.available(); err != nil {
		return nil, err
	}
	if _, err := ReadRecord(s.rt.o.DataDir, p.Machine); err != nil {
		return nil, err
	}
	m := s.rt.get(p.Machine)
	m.mu.Lock()
	r, state := m.cur, m.state
	m.mu.Unlock()
	if r == nil || state != api.MachineRunning {
		return nil, fmt.Errorf("%s is not running (%s)", p.Machine, state)
	}
	return inMachine{r}, nil
}

type inMachine struct{ r *run }

func (c inMachine) In(fn func(cgroup int) error) error {
	b := c.r.boot()
	if b == nil {
		return fmt.Errorf("%s is between boots", c.r.m.Name())
	}
	return c.r.in(b, fn)
}

func (c inMachine) Entry(p api.ExecStartParams) (*session.Entry, error) {
	b := c.r.boot()
	if b == nil {
		return nil, fmt.Errorf("%s is between boots", c.r.m.Name())
	}
	name := c.r.m.Name()
	if p.Copy != "" && !c.r.cfg.Isolated {
		return nil, fmt.Errorf("a copy over the session is an isolated machine's; %s copies through /mnt/mac", name)
	}
	root := filepath.Join(c.r.m.rt.o.DataDir, name)
	u, err := lookupUser(root, c.r.userFor(p.User))
	if err != nil {
		return nil, err
	}
	if len(p.Terminfo) > 0 {
		c.r.terminfo(root, u.Home, p)
	}
	e := sessionEntry(root, name, u, p)
	e.Ptmx = fmt.Sprintf("/proc/%d/root/dev/pts/ptmx", b.child.Pid)
	mnt, cg, err := b.nsFiles()
	if err != nil {
		return nil, err
	}
	e.MountNS, e.CgroupNS = mnt, cg
	e.Release = func() { mnt.Close(); cg.Close() }
	if c.r.cfg.Isolated {
		user, err := b.nsFile("user")
		if err != nil {
			e.Release()
			return nil, err
		}
		e.UserNS, e.Stage, e.IDBase, e.Isolate, e.Copy = user, Stage, isoBase, true, p.Copy
		e.Release = func() { mnt.Close(); cg.Close(); user.Close() }
	}
	return e, nil
}

func (r *run) in(b *boot, fn func(cgroup int) error) error {
	b.nsMu.RLock()
	defer b.nsMu.RUnlock()
	if b.ns == nil {
		return fmt.Errorf("%s has stopped", r.m.Name())
	}
	r.sessMu.Lock()
	defer r.sessMu.Unlock()
	sweepSessions(r.payload)
	r.m.rt.mu.Lock()
	r.m.rt.nextID++
	id := r.m.rt.nextID
	r.m.rt.mu.Unlock()
	group := filepath.Join(r.payload, sessionsGroup, strconv.FormatUint(id, 10))
	if err := os.MkdirAll(group, 0o755); err != nil {
		return err
	}
	cg, err := os.Open(group)
	if err != nil {
		return err
	}
	defer cg.Close()
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		for _, k := range []struct {
			name string
			flag int
		}{{"ipc", unix.CLONE_NEWIPC}, {"uts", unix.CLONE_NEWUTS}, {"net", unix.CLONE_NEWNET}, {"pid", unix.CLONE_NEWPID}} {
			if err := unix.Setns(int(b.ns[k.name].Fd()), k.flag); err != nil {
				result <- fmt.Errorf("enter the machine's %s namespace: %w", k.name, err)
				return
			}
		}
		result <- fn(int(cg.Fd()))
	}()
	err = <-result
	if err != nil {
		unix.Rmdir(group)
	}
	return err
}

func (r *run) terminfo(root, home string, p api.ExecStartParams) {
	name := api.TermOf(p.Env)
	dir, err := installTerminfo(root, home, name, p.Terminfo)
	switch {
	case err != nil:
		r.m.runtimeLogf(logbuf.Warning, "the Mac's terminal entry for TERM %q: %v; the session starts without it", name, err)
	case dir != "":
		r.m.runtimeLogf(logbuf.Info, "the Mac's terminal entry for %q is now %q (%d bytes): the machine had none", name, "/"+filepath.Join(dir, name[:1], name), len(p.Terminfo))
	}
}

func (b *boot) nsFiles() (mnt, cgroup *os.File, err error) {
	b.nsMu.RLock()
	defer b.nsMu.RUnlock()
	if b.ns == nil {
		return nil, nil, errors.New("the machine has stopped")
	}
	dup := func(f *os.File) (*os.File, error) {
		fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), f.Name()), nil
	}
	if mnt, err = dup(b.ns["mnt"]); err != nil {
		return nil, nil, err
	}
	if cgroup, err = dup(b.ns["cgroup"]); err != nil {
		mnt.Close()
		return nil, nil, err
	}
	return mnt, cgroup, nil
}

func (b *boot) nsFile(kind string) (*os.File, error) {
	b.nsMu.RLock()
	defer b.nsMu.RUnlock()
	if b.ns == nil || b.ns[kind] == nil {
		return nil, fmt.Errorf("the machine has no %s namespace of its own, or has stopped", kind)
	}
	fd, err := unix.FcntlInt(b.ns[kind].Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), b.ns[kind].Name()), nil
}

func (r *run) userFor(name string) string {
	if name != "" {
		return name
	}
	if r.cfg.User != "" {
		return r.cfg.User
	}
	return r.rec.User.Name
}

func sessionEntry(root, machine string, u *User, p api.ExecStartParams) *session.Entry {
	argv, file := sessionArgv(u, p)
	return &session.Entry{
		Argv: argv, Path: file, Find: file == "",
		Env:        sessionEnv(root, u),
		Home:       u.Home,
		Cred:       &session.Cred{UID: u.UID, GID: u.GID, Groups: u.Groups},
		RuntimeDir: fmt.Sprintf("/run/user/%d", u.UID),
		Name:       "fev",
	}
}

func (rt *Runtime) SessionUser(machine, user string) error {
	m := rt.get(machine)
	m.mu.Lock()
	r := m.cur
	m.mu.Unlock()
	if r == nil {
		return fmt.Errorf("%s is not running", machine)
	}
	_, err := lookupUser(filepath.Join(rt.o.DataDir, machine), r.userFor(user))
	if err != nil {
		return fmt.Errorf("%s: %w", machine, err)
	}
	return nil
}
