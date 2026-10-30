package machines

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/internal/logbuf"
	"fev/guest/internal/reaper"
)

func WriteCloudSeed(root int, s CloudSeed) error {
	files, err := cloudFiles(s, func(p string) bool { return existsIn(root, p) })
	if err != nil {
		return err
	}
	if err := removeIn(root, cloudDisabled); err != nil {
		return fmt.Errorf("%s: %w", cloudDisabled, err)
	}
	for _, f := range files {
		if cloudKeep(f.path) && existsIn(root, f.path) {
			continue
		}
		if err := writeIn(root, f); err != nil {
			return fmt.Errorf("%s: %w", f.path, err)
		}
	}
	return nil
}

func removeIn(root int, p string) error {
	rel := strings.TrimPrefix(path.Clean("/"+p), "/")
	dir, base := path.Split(rel)
	if dir == "" {
		dir = "."
	}
	dfd, err := unix.Openat2(root, dir, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	})
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(dfd)
	if err := unix.Unlinkat(dfd, base, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

func existsIn(root int, p string) bool {
	fd, err := unix.Openat2(root, strings.TrimPrefix(path.Clean("/"+p), "/"), &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFREG
}

func writeIn(root int, f setupFile) error {
	rel := strings.TrimPrefix(path.Clean("/"+f.path), "/")
	dir, base := path.Split(rel)
	dfd, err := mkdirIn(root, strings.TrimSuffix(dir, "/"))
	if err != nil {
		return err
	}
	defer unix.Close(dfd)
	if err := unix.Unlinkat(dfd, base, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	fd, err := unix.Openat(dfd, base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, f.mode)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), base)
	defer file.Close()
	if _, err := file.WriteString(f.content); err != nil {
		return err
	}
	if err := unix.Fchown(fd, 0, 0); err != nil {
		return err
	}
	return unix.Fchmod(fd, f.mode)
}

func mkdirIn(root int, rel string) (int, error) {
	open := func(p string) (int, error) {
		if p == "" {
			p = "."
		}
		return unix.Openat2(root, p, &unix.OpenHow{
			Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
		})
	}
	if fd, err := open(rel); err == nil || !errors.Is(err, unix.ENOENT) {
		return fd, err
	}
	parts := strings.Split(rel, "/")
	cur := ""
	for _, p := range parts {
		next := path.Join(cur, p)
		fd, err := open(next)
		if err == nil {
			unix.Close(fd)
			cur = next
			continue
		}
		if !errors.Is(err, unix.ENOENT) {
			return -1, err
		}
		pfd, err := open(cur)
		if err != nil {
			return -1, err
		}
		err = unix.Mkdirat(pfd, p, 0o755)
		unix.Close(pfd)
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, err
		}
		cur = next
	}
	return open(rel)
}

const firstBootBound = 30 * time.Minute

type firstBoot struct {
	done  chan struct{}
	cloud *api.CloudInit
	err   error
}

func (r *run) firstStep() *firstBoot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.first
}

func (r *run) startFirstBoot(b *boot) {
	f := &firstBoot{done: make(chan struct{})}
	r.mu.Lock()
	r.first = f
	r.mu.Unlock()
	go func() {
		defer close(f.done)
		f.cloud, f.err = r.online(b)
		m := r.m
		if f.err != nil {
			m.runtimeLogf(logbuf.Error, "the first boot's online step: %v (it is tried again at the next start)", f.err)
			return
		}
		rec := *r.rec
		rec.Online = false
		if err := WriteRecord(m.rt.o.DataDir, &rec); err != nil {
			m.runtimeLogf(logbuf.Error, "the first boot's online step is done and cannot be recorded: %v", err)
			return
		}
		r.rec.Online = false
	}()
}

func (m *machine) runtimeLogf(level int, format string, args ...any) {
	m.rt.o.Log.Logf(level, "%s%s", RuntimeLinePrefix(m.Name()), fmt.Sprintf(format, args...))
}

func (m *machine) openRoot() (int, error) {
	open := func(name string) (int, error) {
		return unix.Open(filepath.Join(m.rt.o.DataDir, name), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	}
	fd, err := open(m.Name())
	if !errors.Is(err, unix.ENOENT) {
		return fd, err
	}
	m.opMu.Lock()
	name := m.Name()
	m.opMu.Unlock()
	return open(name)
}

func rootPath(fd int) string { return fmt.Sprintf("/proc/self/fd/%d", fd) }

func (r *run) online(b *boot) (*api.CloudInit, error) {
	m := r.m
	rfd, err := m.openRoot()
	if err != nil {
		return nil, fmt.Errorf("its root: %w", err)
	}
	defer unix.Close(rfd)
	root := rootPath(rfd)
	has := func(p string) bool {
		q, err := resolveIn(root, p)
		if err != nil {
			return false
		}
		_, err = os.Stat(filepath.Join(root, filepath.FromSlash(q)))
		return err == nil
	}
	plan, perr := planOnline(has)
	deadline := time.Now().Add(firstBootBound)
	r.mu.RLock()
	leased, netErr := b.net.Leased, b.netErr
	r.mu.RUnlock()
	if netErr != nil && (plan.cloud || len(plan.install) > 0) {
		what := "cloud-init's run is not waited for"
		if len(plan.install) > 0 {
			what = strings.Join(plan.packages, " and ") + " cannot be added"
			if plan.cloud {
				what = "cloud-init's run is not waited for and " + what
			}
		}
		return nil, fmt.Errorf("%s has no network (%v): %s", m.Name(), netErr, what)
	}
	var cloud *api.CloudInit
	if plan.cloud {
		var err error
		if cloud, err = r.cloudRun(b, root, deadline); err != nil {
			return nil, err
		}
	}
	if len(plan.install) > 0 {
		if leased != nil {
			select {
			case <-leased:
			case <-b.ended:
				return nil, fmt.Errorf("%s stopped before it had its address", m.Name())
			case <-time.After(time.Until(deadline)):
				return nil, fmt.Errorf("%s was given no address in %v", m.Name(), firstBootBound)
			}
		}
		if err := waitResolver(root, deadline, b.ended); err != nil {
			return nil, err
		}
		t0 := time.Now()
		out, ws, err := r.runIn(b, root, plan.install, deadline)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", strings.Join(plan.install, " "), err)
		}
		if ws.Signaled() || ws.ExitStatus() != 0 {
			return nil, fmt.Errorf("%s: %s: %s", strings.Join(plan.install, " "), describe(ws), lastLines(string(out), 12))
		}
		m.runtimeLogf(logbuf.Info, "added %s (%s, %d ms)", strings.Join(plan.packages, " and "), strings.Join(plan.install, " "), time.Since(t0).Milliseconds())
	}
	if perr != nil {
		return cloud, perr
	}
	return cloud, nil
}

func (r *run) cloudRun(b *boot, root string, deadline time.Time) (*api.CloudInit, error) {
	t0 := time.Now()
	out, _, err := r.runIn(b, root, []string{"cloud-init", "status", "--long"}, deadline)
	if err != nil {
		return nil, fmt.Errorf("cloud-init status --long: %w", err)
	}
	if st, _, _ := cloudStatus(string(out)); !strings.HasPrefix(st, "disabled") && !cloudOff(string(out)) {
		if _, _, err := r.runIn(b, root, []string{"cloud-init", "status", "--wait"}, deadline); err != nil {
			return nil, fmt.Errorf("cloud-init status --wait: %w", err)
		}
		if out, _, err = r.runIn(b, root, []string{"cloud-init", "status", "--long"}, deadline); err != nil {
			return nil, fmt.Errorf("cloud-init status --long: %w", err)
		}
	}
	result, ws, err := r.runIn(b, root, []string{"cat", cloudResult}, deadline)
	if err != nil || ws.ExitStatus() != 0 {
		result = nil
	}
	log, _ := readIn(root, CloudOutputLog)
	c := cloudAccount(string(out), result, log)
	level := logbuf.Info
	if c.Status != "done" || len(c.RecoverableErrors) > 0 {
		level = logbuf.Warning
	}
	r.m.runtimeLogf(level, "cloud-init: %s after %d ms%s%s", c.Status, time.Since(t0).Milliseconds(), errorList(c.Errors), recoverableList(c.RecoverableErrors))
	return &c, nil
}

func recoverableList(errs []string) string {
	if len(errs) == 0 {
		return ""
	}
	return "; recoverable: " + strings.Join(errs, "; ")
}

func errorList(errs []string) string {
	if len(errs) == 0 {
		return ""
	}
	return "; " + strings.Join(errs, "; ")
}

func waitResolver(root string, deadline time.Time, ended <-chan struct{}) error {
	rel, err := resolveIn(root, "/etc/resolv.conf")
	if err != nil {
		return nil
	}
	if strings.HasPrefix(rel, "/run/") || rel == "/run" {
		return nil
	}
	file := filepath.Join(root, filepath.FromSlash(rel))
	ready := func() bool {
		b, err := os.ReadFile(file)
		return err == nil && hasNameserver(string(b))
	}
	if ready() {
		return nil
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return err
	}
	in := os.NewFile(uintptr(fd), "inotify")
	defer in.Close()
	if _, err := unix.InotifyAddWatch(fd, filepath.Dir(file), unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO|unix.IN_CREATE|unix.IN_MODIFY); err != nil {
		return err
	}
	changed := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := in.Read(buf); err != nil {
				return
			}
			select {
			case changed <- struct{}{}:
			default:
			}
		}
	}()
	if ready() {
		return nil
	}
	for {
		select {
		case <-changed:
			if ready() {
				return nil
			}
		case <-ended:
			return errors.New("the machine stopped before it had a resolver")
		case <-time.After(time.Until(deadline)):
			return fmt.Errorf("the machine's /etc/resolv.conf named no server in %v", firstBootBound)
		}
	}
}

const runOutputMax = 64 << 10

func (r *run) runIn(b *boot, root string, argv []string, deadline time.Time) ([]byte, unix.WaitStatus, error) {
	rt := r.m.rt
	prog := argv[0]
	if !strings.Contains(prog, "/") {
		prog = ""
		for _, dir := range strings.Split(machinePath, ":") {
			p := path.Join(dir, argv[0])
			if q, err := resolveIn(root, p); err == nil {
				if st, err := os.Stat(filepath.Join(root, filepath.FromSlash(q))); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
					prog = p
					break
				}
			}
		}
		if prog == "" {
			return nil, 0, fmt.Errorf("the machine has no %s", argv[0])
		}
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, 0, err
	}
	defer pr.Close()
	var child *reaper.Child
	err = r.in(b, func(cgroup int) error {
		if r.cfg.Isolated {
			null, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
			if err != nil {
				return err
			}
			defer null.Close()
			child, err = b.startIsolated(rt.o.Reaper, rt.o.Self, prog, argv, []string{"PATH=" + machinePath, "HOME=/root", "LANG=C.UTF-8", "TERM=dumb"},
				[3]*os.File{null, pw, pw}, &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: cgroup, Setsid: true})
			return err
		}
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			return err
		}
		if err := unix.Setns(int(b.ns["mnt"].Fd()), unix.CLONE_NEWNS); err != nil {
			return fmt.Errorf("enter the machine's mount namespace: %w", err)
		}
		null, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer null.Close()
		child, err = rt.o.Reaper.Start(prog, argv, &os.ProcAttr{
			Dir:   "/",
			Env:   []string{"PATH=" + machinePath, "HOME=/root", "LANG=C.UTF-8", "TERM=dumb"},
			Files: []*os.File{null, pw, pw},
			Sys:   &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: cgroup, Setsid: true},
		})
		return err
	})
	pw.Close()
	if err != nil {
		return nil, 0, err
	}
	out := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		chunk := make([]byte, 32<<10)
		for {
			n, err := pr.Read(chunk)
			buf.Write(chunk[:n])
			if buf.Len() > runOutputMax {
				buf.Next(buf.Len() - runOutputMax)
			}
			if err != nil {
				if err != io.EOF {
					buf.WriteString("\n(" + err.Error() + ")")
				}
				out <- buf.Bytes()
				return
			}
		}
	}()
	done := make(chan unix.WaitStatus, 1)
	go func() { done <- child.Wait() }()
	select {
	case ws := <-done:
		return <-out, ws, nil
	case <-b.ended:
		rt.o.Reaper.Signal(child, unix.SIGKILL, false)
		<-done
		<-out
		return nil, 0, fmt.Errorf("%s stopped", r.m.Name())
	case <-time.After(time.Until(deadline)):
		rt.o.Reaper.Signal(child, unix.SIGKILL, false)
		<-done
		got := <-out
		return got, 0, fmt.Errorf("%s did not end in time: %s", strings.Join(argv, " "), lastLines(string(got), 3))
	}
}

func (rt *Runtime) FirstBoot(name string) (*api.CloudInit, error) {
	m := rt.get(name)
	m.mu.Lock()
	r := m.cur
	m.mu.Unlock()
	if r == nil {
		return nil, nil
	}
	f := r.firstStep()
	if f == nil {
		return nil, nil
	}
	select {
	case <-f.done:
		return f.cloud, f.err
	default:
		return nil, fmt.Errorf("the first boot of %s is still being set up", name)
	}
}
