package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"
	"fev/proto/debugstore"

	"fev/guest/debug/attach"
)

type Debug struct {
	Arch            attach.Arch
	Kernel          attach.KernelVersion
	SyscallNR       func(string) (uint32, bool)
	Dial            func(ctx context.Context) (net.Conn, error)
	EngineProc      func(fn func() error) (ok bool, err error)
	DefaultSeccomp  [][]attach.Insn
	ContainerMounts func(containerID string) (attach.GraphDriver, error)
	ImageView       func(diffIDs []string, session string) (lowers []string, release func(), err error)

	StoreDir         string
	DataRoot         string
	Stage            string
	Galley           string
	DenyCgroupParent string

	PrepareStore         func(cacheURL, indexURL string) error
	StoreReady           func() bool
	Serve                func(cacheURL, indexURL string) (socket string, err error)
	DumpLock             string
	SessionsDir, PinsDir string

	Logf func(string, ...any)
}

func (d *Debug) In(fn func(cgroup int) error) error { return fn(-1) }

func (d *Debug) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

const engineDataRoot = "/var/lib/docker"

var ErrToolsUnavailable = api.ErrDebugToolsUnavailable

func (d *Debug) Prepare(p api.ExecStartParams) error {
	if p.Debug == nil || d.PrepareStore == nil {
		return nil
	}
	if err := d.PrepareStore(p.Debug.CacheURL, p.Debug.IndexURL); err != nil {
		return fmt.Errorf("%w: %v", ErrToolsUnavailable, err)
	}
	return nil
}

func (d *Debug) Entry(p api.ExecStartParams) (*Entry, error) {
	if p.Debug == nil {
		return nil, fmt.Errorf("debug: no target")
	}
	if d.StoreReady != nil && !d.StoreReady() {
		return nil, fmt.Errorf("debug: %w: the tool store was reset after this session was started", ErrToolsUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r, err := attach.Resolve(ctx, d.Dial, p.Debug.Target, p.Debug.Kind)
	if err != nil {
		return nil, err
	}
	switch r.Kind {
	case attach.DebugKindRunning:
		return d.running(p, r)
	case attach.DebugKindStopped:
		return d.stopped(p, r)
	case attach.DebugKindImage:
		return d.image(p, r)
	default:
		return nil, fmt.Errorf("debug: unknown target kind %q", r.Kind)
	}
}

func (d *Debug) Resolve(p api.ExecStartParams) (*api.DebugStarted, error) {
	if p.Debug == nil {
		return nil, fmt.Errorf("debug: no target")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r, err := attach.Resolve(ctx, d.Dial, p.Debug.Target, p.Debug.Kind)
	if err != nil {
		return nil, err
	}
	if r.Kind == attach.DebugKindRunning && r.Paused {
		return nil, fmt.Errorf("debug: %s is paused; unpause it first (docker unpause %s)", r.Name, r.Name)
	}
	readOnly := p.Debug.ReadOnly || r.Kind == attach.DebugKindImage
	ds := &api.DebugStarted{
		Kind:     r.Kind,
		TargetID: r.ID,
		Name:     r.Name,
		Image:    r.Image,
		ReadOnly: readOnly,
	}
	if n := attach.PtraceNote(r); n != "" {
		ds.Notes = append(ds.Notes, n)
	}
	if r.Kind == attach.DebugKindStopped {
		if n := attach.SeccompNote(r); n != "" {
			ds.Notes = append(ds.Notes, n)
		}
		if readOnly {
			ds.Notes = append(ds.Notes, "the container's root is read-only in this session (--read-only); changes are not kept")
		}
	}
	return ds, nil
}

type fdAssign struct {
	files []*os.File
	next  int
}

func newFdAssign() *fdAssign { return &fdAssign{next: DebugExtraBaseFd} }

func (a *fdAssign) add(f *os.File) int {
	if f == nil {
		return -1
	}
	fd := a.next
	a.files = append(a.files, f)
	a.next++
	return fd
}

func (a *fdAssign) closeAll() {
	for _, f := range a.files {
		f.Close()
	}
}

func (d *Debug) openStoreTrees(a *fdAssign, e *attach.DebugEntry) error {
	store, err := openTree(filepath.Join(d.StoreDir, "store"), true)
	if err != nil {
		return fmt.Errorf("debug: the tool store: %w", err)
	}
	e.StoreTreeFd = a.add(store)
	if prof, err := openTree(filepath.Join(d.StoreDir, "profiles", "default"), true); err == nil {
		e.ProfileTreeFd = a.add(prof)
	} else {
		e.ProfileTreeFd = -1
	}
	if etc, err := openTree(filepath.Join(d.StoreDir, "etc"), true); err == nil {
		e.EtcTreeFd = a.add(etc)
	} else {
		e.EtcTreeFd = -1
	}
	return nil
}

var sessionIDs atomic.Uint64

type sess struct {
	id    string
	home  string
	ended []func(*api.ExecExit)
}

func (d *Debug) base(p api.ExecStartParams, r *attach.Resolved, a *fdAssign) (*attach.DebugEntry, *sess, error) {
	e := &attach.DebugEntry{
		Kind:    r.Kind,
		Name:    "fev debug " + r.Name,
		MountFd: -1, NetFd: -1, IPCFd: -1, UTSFd: -1, CgroupNSFd: -1, PIDFd: -1,
		UserFd: -1, CgroupProcsFd: -1, DevCgroupProcsFd: -1, DevPtsFd: -1,
		GalleyTreeFd: -1, SocketTreeFd: -1, HomeTreeFd: -1,
	}
	ss := &sess{id: strconv.FormatUint(sessionIDs.Add(1), 10) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)}
	stage, err := os.Open(d.Stage)
	if err != nil {
		return nil, nil, fmt.Errorf("debug: the stage: %w", err)
	}
	e.StageFd = a.add(stage)
	if err := d.openStoreTrees(a, e); err != nil {
		a.closeAll()
		return nil, nil, err
	}
	if d.Galley != "" {
		g, err := openTree(d.Galley, true)
		if err != nil {
			a.closeAll()
			return nil, nil, fmt.Errorf("debug: galley: %w", err)
		}
		e.GalleyTreeFd = a.add(g)
	}
	if d.Serve != nil {
		sock, err := d.Serve(p.Debug.CacheURL, p.Debug.IndexURL)
		if err != nil {
			a.closeAll()
			return nil, nil, fmt.Errorf("debug: the store's server: %w", err)
		}
		t, err := openTree(sock, true)
		if err != nil {
			a.closeAll()
			return nil, nil, fmt.Errorf("debug: the store's socket: %w", err)
		}
		e.SocketTreeFd = a.add(t)
	}
	ss.home = d.homeDir(r)
	fileTerminfo(ss.home, p)
	if home, err := openTree(ss.home, false); err == nil {
		e.HomeTreeFd = a.add(home)
	}
	e.Env = attach.BuildEnv(r.Env, envValue(p.Env, "TERM"), r.Name)
	e.Env = macEnv(e.Env, p.Env)
	e.Dir = p.Dir
	if e.Dir == "" {
		e.Dir = r.Cwd
	}
	e.Home = attach.HomeDir
	argv := p.Argv
	if len(argv) == 0 {
		argv = []string{debugstore.ProfileDir + "/bin/zsh", "-i"}
	}
	e.Spec = attach.Spec{Argv: argv, UID: 0, GID: 0}
	return e, ss, nil
}

func macEnv(env, mac []string) []string {
	var over []string
	for _, kv := range mac {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch {
		case k == "LANG", strings.HasPrefix(k, "LC_"), k == "PATH", k == "HOME", k == "TERM",
			k == "ZDOTDIR", k == "TERMINFO_DIRS", k == "SSL_CERT_FILE", k == "NIX_SSL_CERT_FILE", k == "FEV_PROFILE":
			continue
		}
		over = append(over, kv)
	}
	return mergeEnv(env, over)
}

func fileTerminfo(home string, p api.ExecStartParams) {
	name := api.TermOf(p.Env)
	if len(p.Terminfo) == 0 || name == "" || len(name) > 64 || strings.ContainsAny(name, "/\x00") || name[0] == '.' {
		return
	}
	dir := filepath.Join(home, ".terminfo", name[:1])
	file := filepath.Join(dir, name)
	if b, err := os.ReadFile(file); err == nil && bytes.Equal(b, p.Terminfo) {
		return
	}
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	tmp := file + ".tmp"
	if os.WriteFile(tmp, p.Terminfo, 0o644) == nil {
		os.Rename(tmp, file)
	}
}

func (d *Debug) homeDir(r *attach.Resolved) string {
	key := r.ID
	if key == "" {
		key = r.Name
	}
	home := filepath.Join(d.StoreDir, "homes", sanitizeKey(key))
	os.MkdirAll(home, 0o700)
	return home
}

func (d *Debug) register(ss *sess) func(pid int) {
	return func(pid int) {
		if d.SessionsDir != "" {
			writeAtomic(filepath.Join(d.SessionsDir, ss.id), []byte(strconv.Itoa(pid)+"\n"))
		}
		if d.PinsDir != "" {
			m, _ := os.ReadFile(filepath.Join(d.StoreDir, "profiles", "default", "manifest"))
			writeAtomic(filepath.Join(d.PinsDir, ss.id), m)
		}
	}
}

func (d *Debug) unregister(ss *sess) {
	if d.SessionsDir != "" {
		os.Remove(filepath.Join(d.SessionsDir, ss.id))
	}
	if d.PinsDir != "" {
		os.Remove(filepath.Join(d.PinsDir, ss.id))
	}
}

func writeAtomic(p string, b []byte) {
	os.MkdirAll(filepath.Dir(p), 0o700)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, p)
	}
}

func (d *Debug) running(p api.ExecStartParams, r *attach.Resolved) (*Entry, error) {
	if r.Paused {
		return nil, fmt.Errorf("debug: %s is paused; unpause it first (docker unpause %s)", r.Name, r.Name)
	}
	a := newFdAssign()
	e, ss, err := d.base(p, r, a)
	if err != nil {
		return nil, err
	}
	var capbnd uint64
	var cwd string
	var rootUID, rootGID int = -1, -1
	var initUser unix.Stat_t
	haveInitUser := unix.Stat("/proc/self/ns/user", &initUser) == nil
	cgRoot, err := os.Open("/sys/fs/cgroup")
	if err != nil {
		a.closeAll()
		return nil, fmt.Errorf("debug: the cgroup root: %w", err)
	}
	defer cgRoot.Close()
	ok, err := d.EngineProc(func() error {
		base := "/proc/" + strconv.Itoa(r.Pid)
		for _, ns := range []struct {
			name string
			dst  *int
		}{
			{"mnt", &e.MountFd}, {"net", &e.NetFd}, {"ipc", &e.IPCFd},
			{"uts", &e.UTSFd}, {"cgroup", &e.CgroupNSFd}, {"pid", &e.PIDFd},
		} {
			f, err := os.Open(base + "/ns/" + ns.name)
			if err != nil {
				return fmt.Errorf("the target's %s namespace: %w", ns.name, err)
			}
			if ns.name == "pid" {
				var st unix.Stat_t
				if err := unix.Fstat(int(f.Fd()), &st); err != nil {
					f.Close()
					return fmt.Errorf("the target's pid namespace: %w", err)
				}
				e.TargetPidNS = st.Ino
			}
			*ns.dst = a.add(f)
		}
		status, err := os.ReadFile(base + "/status")
		if err != nil {
			return fmt.Errorf("the target's status: %w", err)
		}
		if e.TargetNSPid, err = attach.LastNSpid(status); err != nil {
			return err
		}
		stat, err := os.ReadFile(base + "/stat")
		if err != nil {
			return fmt.Errorf("the target's stat: %w", err)
		}
		if e.TargetStart, err = attach.StartTime(stat); err != nil {
			return err
		}
		if haveInitUser {
			if own, uf := ownUserNS(base, initUser); own {
				e.UserFd = a.add(uf)
				rootUID = mappedRoot(base + "/uid_map")
				rootGID = mappedRoot(base + "/gid_map")
			}
		}
		procs, err := openCgroupProcs(base, cgRoot)
		if err != nil {
			return fmt.Errorf("the target's cgroup: %w", err)
		}
		e.CgroupProcsFd = a.add(procs)
		capbnd = readCapBnd(base)
		if l, err := os.Readlink(base + "/cwd"); err == nil {
			cwd = l
		}
		return nil
	})
	if err != nil {
		a.closeAll()
		return nil, fmt.Errorf("debug: %w", err)
	}
	if !ok {
		a.closeAll()
		return nil, fmt.Errorf("debug: the engine is not running")
	}
	if rootUID >= 0 && rootGID >= 0 {
		os.Lchown(ss.home, rootUID, rootGID)
		filepath.WalkDir(ss.home, func(p string, _ fs.DirEntry, err error) error {
			if err == nil {
				os.Lchown(p, rootUID, rootGID)
			}
			return nil
		})
	}
	if e.Dir == "" {
		e.Dir = cwd
	}
	e.Spec.CapKeep = attach.RunningCaps(capbnd)
	e.Prefix = attach.BuildPrefix(d.Arch, true)
	e.DumpLock = d.DumpLock
	ss.ended = append(ss.ended, d.targetGone(r))
	return d.finish(e, a, ss), nil
}

var targetGoneWindow = 3 * time.Second

func (d *Debug) targetGone(r *attach.Resolved) func(*api.ExecExit) {
	return func(x *api.ExecExit) {
		if x.Signal != int(unix.SIGKILL) && x.ExitCode != 128+int(unix.SIGKILL) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), targetGoneWindow+2*time.Second)
		defer cancel()
		deadline := time.Now().Add(targetGoneWindow)
		for {
			now, err := attach.Resolve(ctx, d.Dial, r.ID, attach.DebugRequestContainer)
			var gone *attach.NotHereError
			if errors.As(err, &gone) || (err == nil && now.Kind != attach.DebugKindRunning) {
				x.Reason = api.ExecReasonTargetGone
				return
			}
			if time.Now().After(deadline) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(150 * time.Millisecond):
			}
		}
	}
}

func mappedRoot(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == "0" {
			if n, err := strconv.Atoi(f[1]); err == nil {
				return n
			}
		}
	}
	return -1
}

func (d *Debug) stopped(p api.ExecStartParams, r *attach.Resolved) (*Entry, error) {
	a := newFdAssign()
	e, ss, err := d.base(p, r, a)
	if err != nil {
		return nil, err
	}
	readOnly := p.Debug.ReadOnly

	gd := r.Graph
	if (gd.Data == nil || (gd.Data["LowerDir"] == "" && gd.Data["UpperDir"] == "")) && d.ContainerMounts != nil {
		m, err := d.ContainerMounts(r.ID)
		if err != nil {
			a.closeAll()
			return nil, fmt.Errorf("debug: the container's layers: %w", err)
		}
		gd = m
	}
	lowers, err := attach.OverlayLowers(gd)
	if err != nil {
		a.closeAll()
		return nil, fmt.Errorf("debug: %w", err)
	}
	e.Lowers = d.translateLayers(lowers)

	if !readOnly {
		root, upper, work, err := d.sessionUpper(r.ID)
		if err != nil {
			a.closeAll()
			return nil, fmt.Errorf("debug: %w", err)
		}
		e.Upper, e.Work = upper, work
		containerUpper := d.translate(attach.ContainerUpper(gd))
		ss.ended = append(ss.ended, d.commitOnExit(r, root, upper, containerUpper))
	}
	if err := d.freshNS(r.ID, e, a, ss); err != nil {
		a.closeAll()
		return nil, err
	}
	e.Spec.CapKeep = attach.StoppedImageCaps(r.Caps, r.HasCaps)
	seccomp, err := d.stoppedSeccomp(r)
	if err != nil {
		d.end(ss, &api.ExecExit{ExitCode: -1})
		a.closeAll()
		return nil, err
	}
	e.Spec.Seccomp = seccomp
	return d.finish(e, a, ss), nil
}

func (d *Debug) stoppedSeccomp(r *attach.Resolved) ([][]attach.Insn, error) {
	switch r.Seccomp {
	case "unconfined":
		return nil, nil
	case "custom":
		if d.SyscallNR == nil {
			return nil, fmt.Errorf("debug: the seccomp compiler is not wired (init must set SyscallNR)")
		}
		bounding := r.Caps
		if !r.HasCaps {
			bounding = attach.DockerDefaultCaps
		}
		layers, err := attach.CompileContainerProfile(r.SeccompProfile, bounding, d.Arch, d.Kernel, d.SyscallNR)
		if err != nil {
			return nil, fmt.Errorf("debug: the container's seccomp profile cannot be applied to this session: %w", err)
		}
		return layers, nil
	default:
		if d.DefaultSeccomp == nil {
			return nil, fmt.Errorf("debug: the default seccomp program is not wired (init must set DefaultSeccomp)")
		}
		return d.DefaultSeccomp, nil
	}
}

func (d *Debug) image(p api.ExecStartParams, r *attach.Resolved) (*Entry, error) {
	a := newFdAssign()
	e, ss, err := d.base(p, r, a)
	if err != nil {
		return nil, err
	}
	if d.ImageView == nil {
		a.closeAll()
		return nil, fmt.Errorf("debug: image sessions need the guest file view's image snapshots, which init has not wired")
	}
	sessionName := "fev-debug-" + sanitizeKey(r.ID) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	lowers, release, err := d.ImageView(r.DiffIDs, sessionName)
	if err != nil {
		a.closeAll()
		return nil, fmt.Errorf("debug: the image's view: %w", err)
	}
	ss.ended = append(ss.ended, func(*api.ExecExit) { release() })
	e.Lowers = lowers
	root, upper, work, err := d.sessionUpper(r.ID)
	if err != nil {
		d.end(ss, &api.ExecExit{ExitCode: -1})
		a.closeAll()
		return nil, fmt.Errorf("debug: %w", err)
	}
	e.Upper, e.Work = upper, work
	ss.ended = append(ss.ended, func(*api.ExecExit) { removeAll(root) })
	if err := d.freshNS(r.ID, e, a, ss); err != nil {
		d.end(ss, &api.ExecExit{ExitCode: -1})
		a.closeAll()
		return nil, err
	}
	e.Spec.CapKeep = attach.StoppedImageCaps(0, false)
	if d.DefaultSeccomp == nil {
		d.end(ss, &api.ExecExit{ExitCode: -1})
		a.closeAll()
		return nil, fmt.Errorf("debug: the default seccomp program is not wired (init must set DefaultSeccomp)")
	}
	e.Spec.Seccomp = d.DefaultSeccomp
	return d.finish(e, a, ss), nil
}

func (d *Debug) end(ss *sess, x *api.ExecExit) {
	for _, f := range ss.ended {
		f(x)
	}
	ss.ended = nil
	d.unregister(ss)
}

func (d *Debug) finish(e *attach.DebugEntry, a *fdAssign, ss *sess) *Entry {
	ent := &Entry{
		Argv:    e.Spec.Argv,
		Env:     e.Env,
		Find:    true,
		Home:    attach.HomeDir,
		Debug:   e,
		Extra:   a.files,
		Name:    "fev debug",
		Started: d.register(ss),
		OnExit:  func(x *api.ExecExit) { d.end(ss, x) },
	}
	if e.DevPtsFd >= 0 {
		ent.Ptmx = fmt.Sprintf("/proc/self/fd/%d/ptmx", a.files[e.DevPtsFd-DebugExtraBaseFd].Fd())
	}
	return ent
}

func (d *Debug) commitOnExit(r *attach.Resolved, root, sessionUpper, containerUpper string) func(*api.ExecExit) {
	return func(x *api.ExecExit) {
		if containerUpper == "" {
			removeAll(root)
			return
		}
		keep := func(why string) {
			kept := filepath.Join(d.StoreDir, "kept", sanitizeKey(r.Name)+"-"+time.Now().UTC().Format("20060102T150405Z"))
			os.MkdirAll(filepath.Dir(kept), 0o700)
			if err := os.Rename(sessionUpper, kept); err != nil {
				kept = sessionUpper
			} else {
				removeAll(root)
			}
			note := fmt.Sprintf("%s %s; the session's changes were not committed and are kept in the VM at %s", r.Name, why, kept)
			addNote(x, note)
			d.logf("debug: %s", note)
		}
		state, err := d.containerState(r.ID)
		switch {
		case err != nil:
			keep(fmt.Sprintf("could not be looked at (%v)", err))
			return
		case state == stateGone:
			keep("was removed during the session")
			return
		case state == stateRunning:
			keep("was started during the session")
			return
		}
		if err := mergeTree(sessionUpper, containerUpper); err != nil {
			keep(fmt.Sprintf("could not take the session's changes (%v)", err))
			return
		}
		if state, err := d.containerState(r.ID); err == nil && state == stateRunning {
			addNote(x, fmt.Sprintf("%s was started while the session's changes were committed; it may have started with only part of them", r.Name))
		}
		removeAll(root)
	}
}

func addNote(x *api.ExecExit, line string) {
	if x.Note != "" {
		x.Note += "\n"
	}
	x.Note += line
}

const (
	stateStopped = iota
	stateRunning
	stateGone
)

func (d *Debug) containerState(id string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := attach.Resolve(ctx, d.Dial, id, attach.DebugRequestContainer)
	var gone *attach.NotHereError
	switch {
	case errors.As(err, &gone):
		return stateGone, nil
	case err != nil:
		return 0, err
	case r.Kind == attach.DebugKindStopped:
		return stateStopped, nil
	}
	return stateRunning, nil
}

func (d *Debug) sessionUpper(id string) (root, upper, work string, err error) {
	root = filepath.Join(d.StoreDir, "tmp", ".dbg-"+sanitizeKey(id)+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	upper = filepath.Join(root, "u")
	work = filepath.Join(root, "w")
	for _, p := range []string{upper, work} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return "", "", "", err
		}
	}
	return root, upper, work, nil
}

func (d *Debug) freshNS(id string, e *attach.DebugEntry, a *fdAssign, ss *sess) error {
	name := "dbg-" + sanitizeKey(id) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	dir, procs, err := attach.DenyDeviceCgroup(d.DenyCgroupParent, name)
	if err != nil {
		return fmt.Errorf("debug: %w", err)
	}
	ss.ended = append([]func(*api.ExecExit){func(*api.ExecExit) {
		killGroup(dir, killWait)
		os.Remove(dir)
	}}, ss.ended...)
	e.DevCgroupProcsFd = a.add(procs)
	pts, err := newDevpts()
	if err != nil {
		return fmt.Errorf("debug: the session's devpts: %w", err)
	}
	e.DevPtsFd = a.add(pts)
	return nil
}

func newDevpts() (*os.File, error) {
	fs, err := unix.Fsopen("devpts", unix.FSOPEN_CLOEXEC)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fs)
	for k, v := range map[string]string{"ptmxmode": "0666", "mode": "0620", "gid": "5"} {
		if err := unix.FsconfigSetString(fs, k, v); err != nil {
			return nil, fmt.Errorf("devpts %s: %w", k, err)
		}
	}
	if err := unix.FsconfigCreate(fs); err != nil {
		return nil, err
	}
	m, err := unix.Fsmount(fs, unix.FSMOUNT_CLOEXEC, unix.MOUNT_ATTR_NOSUID|unix.MOUNT_ATTR_NOEXEC)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(m), "devpts"), nil
}

func (d *Debug) translateLayers(ls []string) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = d.translate(l)
	}
	return out
}

func (d *Debug) translate(p string) string {
	if d.DataRoot == "" {
		return p
	}
	if rest, ok := strings.CutPrefix(p, engineDataRoot); ok {
		return d.DataRoot + rest
	}
	return p
}

func openTree(path string, readOnly bool) (*os.File, error) {
	fd, err := unix.OpenTree(unix.AT_FDCWD, path, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|uint(unix.AT_RECURSIVE))
	if err != nil {
		return nil, err
	}
	if readOnly {
		if err := unix.MountSetattr(fd, "", uint(unix.AT_EMPTY_PATH|unix.AT_RECURSIVE), &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NODEV}); err != nil {
			unix.Close(fd)
			return nil, err
		}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func ownUserNS(procBase string, initUser unix.Stat_t) (bool, *os.File) {
	f, err := os.Open(procBase + "/ns/user")
	if err != nil {
		return false, nil
	}
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) == nil && st.Ino == initUser.Ino && st.Dev == initUser.Dev {
		f.Close()
		return false, nil
	}
	return true, f
}

func openCgroupProcs(procBase string, root *os.File) (*os.File, error) {
	b, err := os.ReadFile(procBase + "/cgroup")
	if err != nil {
		return nil, err
	}
	var rel string
	for _, line := range strings.Split(string(b), "\n") {
		if r, ok := strings.CutPrefix(line, "0::"); ok {
			rel = strings.TrimSpace(r)
		}
	}
	if rel == "" {
		return nil, fmt.Errorf("no cgroup v2 line")
	}
	if strings.Contains(rel, "..") {
		return nil, fmt.Errorf("the cgroup %q is outside the reader's namespace", rel)
	}
	name := filepath.Join(strings.TrimPrefix(rel, "/"), "cgroup.procs")
	fd, err := unix.Openat(int(root.Fd()), name, unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: filepath.Join(root.Name(), name), Err: err}
	}
	return os.NewFile(uintptr(fd), filepath.Join(root.Name(), name)), nil
}

func readCapBnd(procBase string) uint64 {
	b, err := os.ReadFile(procBase + "/status")
	if err != nil {
		return ^uint64(0)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "CapBnd:"); ok {
			if n, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64); err == nil {
				return n
			}
		}
	}
	return ^uint64(0)
}

func sanitizeKey(k string) string {
	k = strings.TrimPrefix(k, "sha256:")
	var b strings.Builder
	for _, c := range k {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteRune(c)
		}
	}
	s := b.String()
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		s = "x"
	}
	return s
}

func envValue(env []string, key string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			return v
		}
	}
	return ""
}

func removeAll(p string) {
	filepath.WalkDir(p, func(q string, de fs.DirEntry, err error) error {
		if err == nil && de.IsDir() {
			os.Chmod(q, 0o755)
		}
		return nil
	})
	os.RemoveAll(p)
}

func mergeTree(src, dst string) error {
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, de := range ents {
		s := filepath.Join(src, de.Name())
		t := filepath.Join(dst, de.Name())
		var st unix.Stat_t
		if err := unix.Lstat(s, &st); err != nil {
			return err
		}
		var tst unix.Stat_t
		tErr := unix.Lstat(t, &tst)
		tExists := tErr == nil
		tDir := tExists && tst.Mode&unix.S_IFMT == unix.S_IFDIR
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			opaque := xattr(s, "trusted.overlay.opaque") == "y"
			if tExists && (!tDir || opaque) {
				removeAll(t)
				tExists = false
			}
			if !tExists {
				if err := os.Mkdir(t, 0o700); err != nil {
					return err
				}
			}
			if err := mergeTree(s, t); err != nil {
				return err
			}
			if err := copyMeta(s, t, &st); err != nil {
				return err
			}
			if opaque {
				if err := unix.Lsetxattr(t, "trusted.overlay.opaque", []byte("y"), 0); err != nil {
					return fmt.Errorf("mark %s opaque: %w", t, err)
				}
			}
		case unix.S_IFREG:
			tmp := filepath.Join(dst, ".fev-commit-"+de.Name())
			if err := copyFile(s, tmp); err != nil {
				os.Remove(tmp)
				return err
			}
			if err := copyMeta(s, tmp, &st); err != nil {
				os.Remove(tmp)
				return err
			}
			if tDir {
				removeAll(t)
			}
			if err := os.Rename(tmp, t); err != nil {
				os.Remove(tmp)
				return err
			}
		case unix.S_IFLNK:
			link, err := os.Readlink(s)
			if err != nil {
				return err
			}
			if tExists {
				removeAll(t)
			}
			if err := os.Symlink(link, t); err != nil {
				return err
			}
			if err := copyMeta(s, t, &st); err != nil {
				return err
			}
		default:
			if tExists {
				removeAll(t)
			}
			if err := unix.Mknod(t, st.Mode, int(st.Rdev)); err != nil {
				return fmt.Errorf("mknod %s: %w", t, err)
			}
			if err := copyMeta(s, t, &st); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyMeta(s, t string, st *unix.Stat_t) error {
	if err := unix.Lchown(t, int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("chown %s: %w", t, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		if err := unix.Chmod(t, st.Mode&07777); err != nil {
			return fmt.Errorf("chmod %s: %w", t, err)
		}
	}
	if err := copyXattrs(s, t); err != nil {
		return err
	}
	ts := []unix.Timespec{st.Atim, st.Mtim}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, t, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("times of %s: %w", t, err)
	}
	return nil
}

func copyXattrs(s, t string) error {
	sz, err := unix.Llistxattr(s, nil)
	if err != nil || sz <= 0 {
		return nil
	}
	buf := make([]byte, sz)
	sz, err = unix.Llistxattr(s, buf)
	if err != nil {
		return nil
	}
	for _, name := range strings.Split(string(buf[:sz]), "\x00") {
		if name == "" || strings.HasPrefix(name, "trusted.overlay.") {
			continue
		}
		v := xattr(s, name)
		if err := unix.Lsetxattr(t, name, []byte(v), 0); err != nil && !errors.Is(err, unix.ENOTSUP) {
			return fmt.Errorf("xattr %s of %s: %w", name, t, err)
		}
	}
	return nil
}

func xattr(p, name string) string {
	sz, err := unix.Lgetxattr(p, name, nil)
	if err != nil || sz <= 0 {
		return ""
	}
	buf := make([]byte, sz)
	sz, err = unix.Lgetxattr(p, name, buf)
	if err != nil {
		return ""
	}
	return string(buf[:sz])
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
