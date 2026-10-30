package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/proto/version"

	"fev/guest/internal/logbuf"
	"fev/guest/internal/reaper"
	"fev/guest/internal/supervise"
	"fev/guest/machines"
)

const (
	engineImage     = systemMount + "/engine"
	engineRoot      = "/run/fev/engine/root"
	engineRun       = "/run/fev/engine/run"
	engineSocket    = engineRun + "/docker.sock"
	engineData      = "/data/@docker"
	engineMachines  = machinesData
	engineOSRelease = "/run/fev/engine/os-release"
	engineUpper     = engineData + "/.fev-root"
	engineCgroup    = cgroupRoot + "/fev.system/docker"
	notifySocket    = "@fev/notify/engine"
)

const (
	killWait      = 5 * time.Second
	pingWait      = 5 * time.Second
	rcuStartLimit = 5 * time.Second
)

var engineArgs = []string{
	"/usr/bin/dockerd",
	"--config-file", "/run/" + engineConfigName,
}

var engineEnv = []string{
	"PATH=" + runcByName + ":/usr/sbin:/usr/bin",
	"HOME=/root",
	"NOTIFY_SOCKET=" + notifySocket,
	"TMPDIR=/run/tmp",
}

const runcByName = "/usr/libexec/fev/byname"

const (
	hostServices = engineRun + "/host-services"
	sshAuthSock  = hostServices + "/ssh-auth.sock"
)

type engine struct {
	sys  *system
	out  *logbuf.Buffer
	sup  *supervise.Supervisor
	http *http.Client

	mu                             sync.Mutex
	cur                            *dockerdRun
	spawnedWith                    string
	ssh                            net.Listener
	badConfig, badWhy, lastFailure string

	cfgMu sync.Mutex
}

type dockerdRun struct {
	child    *reaper.Child
	dockerd  int
	ready    chan error
	exited   chan struct{}
	how      string
	said     chan struct{}
	saidOnce sync.Once
	ended    atomic.Bool
	release  func()

	ns  sync.RWMutex
	mnt *os.File
	pid *os.File

	agentMu                sync.Mutex
	agentAt                string
	agentKnown, agentBound bool

	k8sBound atomic.Bool
}

func newEngine(sys *system) *engine {
	e := &engine{sys: sys, out: logbuf.New(1024, monotonicMS, nil)}
	e.http = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", engineSocket)
		},
		DisableKeepAlives: true,
	}}
	e.sup = supervise.New(supervise.Hooks{
		Start: e.start,
		Stop:  e.stop,
		NowMS: monotonicMS,
		After: func(d time.Duration, f func()) func() {
			t := time.AfterFunc(d, f)
			return func() { t.Stop() }
		},
		Logf: func(format string, args ...any) { sys.log.Logf(logbuf.Warning, format, args...) },
	})
	return e
}

func (e *engine) listenNotify() error {
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: notifySocket, Net: "unixgram"})
	if err != nil {
		return err
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	raw.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PASSCRED, 1) })
	if serr != nil {
		return serr
	}
	go func() {
		buf := make([]byte, 4096)
		oob := make([]byte, unix.CmsgSpace(unix.SizeofUcred))
		for {
			n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
			if err != nil {
				e.sys.log.Logf(logbuf.Error, "engine: notify socket: %v", err)
				return
			}
			e.notified(buf[:n], senderPid(oob[:oobn]))
		}
	}()
	return nil
}

func senderPid(oob []byte) int {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return 0
	}
	for _, m := range msgs {
		if cred, err := unix.ParseUnixCredentials(&m); err == nil {
			return int(cred.Pid)
		}
	}
	return 0
}

func (e *engine) notified(data []byte, pid int) {
	e.mu.Lock()
	run := e.cur
	e.mu.Unlock()
	if run == nil || pid == 0 || pid != run.dockerd {
		e.sys.log.Logf(logbuf.Warning, "engine: ignored a notification from pid %d, which is not dockerd", pid)
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			e.sys.log.Logf(logbuf.Debug, "engine: notified %s", line)
		}
		if line == "READY=1" {
			run.saidOnce.Do(func() { close(run.said) })
		}
	}
}

func (e *engine) ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), pingWait)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://engine/_ping", nil)
	resp, err := e.http.Do(req)
	if err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ping: status %d", resp.StatusCode)
	}
	return nil
}

func (e *engine) available() error {
	if _, err := os.Stat(engineImage + engineArgs[0]); err != nil {
		return errors.New("the system image has no engine")
	}
	vol, err := e.sys.volume()
	if err != nil {
		return err
	}
	if vol.Outcome().ReadOnly {
		return errors.New(api.DataReadOnly)
	}
	if st, err := os.Stat(engineData); err != nil || !st.IsDir() {
		return fmt.Errorf("the data filesystem has no %s", filepath.Base(engineData))
	}
	return nil
}

func populated(events *os.File) (bool, error) {
	buf := make([]byte, 256)
	n, err := events.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return false, err
	}
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		if v, ok := strings.CutPrefix(line, "populated "); ok {
			return v == "1", nil
		}
	}
	return false, errors.New("cgroup.events has no populated line")
}

func waitEmpty(cgroup string, limit time.Duration) bool {
	events, err := os.Open(filepath.Join(cgroup, "cgroup.events"))
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	defer events.Close()
	deadline := time.Now().Add(limit)
	for {
		if full, err := populated(events); err != nil || !full {
			return err == nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		fds := []unix.PollFd{{Fd: int32(events.Fd()), Events: unix.POLLPRI}}
		if _, err := unix.Poll(fds, int(left.Milliseconds())+1); err != nil && !errors.Is(err, unix.EINTR) {
			return false
		}
	}
}

func (e *engine) resetCgroup() error {
	if _, err := os.Stat(engineCgroup); err == nil {
		os.WriteFile(filepath.Join(engineCgroup, "cgroup.kill"), []byte("1"), 0)
		if !waitEmpty(engineCgroup, killWait) {
			return errors.New("processes of the last run would not die")
		}
		e.sys.engPorts.ended()
		if err := removeCgroups(engineCgroup); err != nil {
			return err
		}
	}
	parent := filepath.Dir(engineCgroup)
	controllers, err := os.ReadFile(filepath.Join(parent, "cgroup.controllers"))
	if err != nil {
		return err
	}
	if len(strings.Fields(string(controllers))) == 0 {
		return errors.New("no cgroup controllers are available to the engine")
	}
	enable := "+" + strings.Join(strings.Fields(string(controllers)), " +")
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte(enable), 0); err != nil {
		return fmt.Errorf("delegate controllers to %s: %w", filepath.Base(parent), err)
	}
	return os.Mkdir(engineCgroup, 0o755)
}

func removeCgroups(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if err := removeCgroups(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return unix.Rmdir(dir)
}

func (e *engine) start() (*supervise.Run, error) {
	if err := e.available(); err != nil {
		return nil, err
	}
	release := func() {}
	if rcuForEngine(e.sys.cmdline) {
		release = e.sys.rcu.hold()
	}
	started := false
	defer func() {
		if !started {
			release()
		}
	}()

	if err := e.resetCgroup(); err != nil {
		return nil, fmt.Errorf("reset the engine's cgroup: %w", err)
	}
	e.sys.engPorts.started()
	e.mu.Lock()
	if e.ssh != nil {
		e.ssh.Close()
		e.ssh = nil
	}
	e.mu.Unlock()
	if err := newEngineRun(engineRun, e.sys.web.engineStarted); err != nil {
		return nil, err
	}
	if err := e.sys.cluster().prepareRun(); err != nil {
		return nil, fmt.Errorf("write the cluster's files: %w", err)
	}
	if e.sys.emu != nil {
		if err := e.sys.emu.writeConfig(engineRun, engineCgroup); err != nil {
			return nil, err
		}
	}
	for _, dir := range []string{engineRoot, hostServices} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	spawnedWith, err := e.writeConfig()
	if err != nil {
		return nil, fmt.Errorf("write the engine's configuration: %w", err)
	}
	if err := e.listenSSH(); err != nil {
		e.sys.log.Logf(logbuf.Warning, "engine: no SSH agent socket: %v", err)
	}
	dns := e.sys.uplink().DNS
	resolv := fmt.Sprintf("# written by fev-init\nnameserver %s\n", dns)
	if err := os.WriteFile(filepath.Join(engineRun, "resolv.conf"), []byte(resolv), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(engineOSRelease, []byte(osRelease(version.Build)), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(engineRun, filepath.Base(engineFevDir), engineStageName), nil, 0o755); err != nil {
		return nil, err
	}

	cgroup, err := os.Open(engineCgroup)
	if err != nil {
		return nil, err
	}
	defer cgroup.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer null.Close()
	logR, logW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	reportR, reportW, err := os.Pipe()
	if err != nil {
		logR.Close()
		logW.Close()
		return nil, err
	}

	run := &dockerdRun{ready: make(chan error, 1), exited: make(chan struct{}), said: make(chan struct{})}
	spawned := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		spawned <- e.spawn(run, cgroup, null, logW, reportW)
	}()
	err = <-spawned
	logW.Close()
	reportW.Close()
	if err != nil {
		logR.Close()
		reportR.Close()
		return nil, err
	}
	go e.pump(logR)
	report, br, err := e.supervised(run, reportR)
	if err != nil {
		reportR.Close()
		run.ended.Store(true)
		e.sys.reaper.Signal(run.child, unix.SIGKILL, false)
		run.child.Wait()
		e.closeNamespaces(run)
		return nil, err
	}

	e.mu.Lock()
	e.cur, e.spawnedWith = run, spawnedWith
	e.mu.Unlock()
	want := e.sys.agentPath()
	run.agentMu.Lock()
	moved := run.agentAt != want
	run.agentMu.Unlock()
	if moved {
		e.followAgent(run)
	}

	var once sync.Once
	run.release = func() { once.Do(release) }
	timer := time.AfterFunc(rcuStartLimit, run.release)
	reported := make(chan struct{})
	go func() {
		defer close(reported)
		for {
			ok, err := readReport(br, report, e.sys.cluster().reported)
			if !ok {
				break
			}
			if err != nil {
				e.sys.log.Logf(logbuf.Warning, "engine: %v", err)
			}
		}
		reportR.Close()
	}()
	go func() {
		status := run.child.Wait()
		<-reported
		if report.exited {
			status = report.status
		}
		switch {
		case status.Signaled():
			run.how = fmt.Sprintf("signal %d", status.Signal())
		default:
			run.how = fmt.Sprintf("exit %d", status.ExitStatus())
		}
		timer.Stop()
		run.release()
		e.closeNamespaces(run)
		select {
		case <-run.said:
		default:
			if !run.ended.Load() {
				e.ended(spawnedWith, false)
			}
		}
		e.sys.viewEngineEnded()
		close(run.exited)
	}()
	go func() {
		select {
		case <-run.said:
			err := e.ping()
			timer.Stop()
			run.release()
			if err == nil {
				e.ended(spawnedWith, true)
			}
			run.ready <- err
		case <-run.exited:
		}
	}()

	started = true
	return &supervise.Run{PID: run.dockerd, Ready: run.ready, Exited: run.exited, How: func() string { return run.how }}, nil
}

const supervisorWait = 5 * time.Second

func (e *engine) supervised(run *dockerdRun, reportR *os.File) (*supervisorReport, *bufio.Reader, error) {
	report := &supervisorReport{}
	reportR.SetReadDeadline(time.Now().Add(supervisorWait))
	br := bufio.NewReader(reportR)
	var (
		ok  bool
		err error
	)
	for report.dockerd == 0 && report.err == nil && !report.exited {
		ok, err = readReport(br, report, e.sys.cluster().reported)
		if !ok || err != nil {
			break
		}
	}
	reportR.SetReadDeadline(time.Time{})
	switch {
	case report.err != nil:
		return nil, nil, fmt.Errorf("the engine's supervisor: %w", report.err)
	case err != nil && ok:
		return nil, nil, err
	case report.dockerd == 0:
		return nil, nil, fmt.Errorf("the engine's supervisor said nothing: %v", err)
	}
	run.dockerd = outerPid("/proc", run.child.Pid, report.dockerd)
	if run.dockerd == 0 && !report.exited {
		e.sys.log.Logf(logbuf.Warning, "engine: dockerd (pid %d in its namespace) is not a child of its supervisor", report.dockerd)
	}
	pidns, err := os.Open(fmt.Sprintf("/proc/%d/ns/pid", run.child.Pid))
	if err != nil {
		return nil, nil, fmt.Errorf("open the engine's PID namespace: %w", err)
	}
	run.ns.Lock()
	run.pid = pidns
	run.ns.Unlock()
	return report, br, nil
}

func (e *engine) closeNamespaces(run *dockerdRun) {
	run.ns.Lock()
	defer run.ns.Unlock()
	for _, f := range []**os.File{&run.mnt, &run.pid} {
		if *f != nil {
			(*f).Close()
			*f = nil
		}
	}
}

func osRelease(build string) string {
	return fmt.Sprintf("NAME=\"fev\"\nID=fev\nVERSION_ID=%s\nPRETTY_NAME=\"fev\"\n", build)
}

func (e *engine) spawn(run *dockerdRun, cgroup, stdin, output, report *os.File) error {
	view := false
	if st, err := os.Stat(engineMachines); err == nil && st.IsDir() {
		if err := machines.EnsureView(machinesView, engineVolumes()); err != nil {
			e.sys.log.Logf(logbuf.Warning, "engine: %v; the engine has no /mnt/machines", err)
		} else {
			view = true
		}
	}
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("unshare mount namespace: %w", err)
	}
	if err := unix.Unshare(unix.CLONE_NEWUTS); err != nil {
		return fmt.Errorf("unshare UTS namespace: %w", err)
	}
	if err := unix.Sethostname([]byte(engineNodeName(e.sys.cmdline))); err != nil {
		return fmt.Errorf("set the engine's host name: %w", err)
	}
	mnt, err := os.Open("/proc/thread-self/ns/mnt")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			mnt.Close()
		}
	}()
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		return fmt.Errorf("make / a slave: %w", err)
	}
	const quiet = unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC
	steps := []struct {
		source, target, fstype string
		flags                  uintptr
		data                   string
	}{
		{"overlay", "", "overlay", 0, "lowerdir=" + engineImage + ",upperdir=" + engineUpper + "/upper,workdir=" + engineUpper + "/work"},
		{"sysfs", "/sys", "sysfs", quiet, ""},
		{engineCgroup, "/sys/fs/cgroup", "", unix.MS_BIND, ""},
		{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755"},
		{"devpts", "/dev/pts", "devpts", unix.MS_NOSUID | unix.MS_NOEXEC, "gid=5,mode=0620,ptmxmode=0666"},
		{"tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		{"mqueue", "/dev/mqueue", "mqueue", quiet, ""},
		{engineRun, "/run", "", unix.MS_BIND, ""},
		{engineData, "/var/lib/docker", "", unix.MS_BIND, ""},
		{"", "/var/lib/docker", "", unix.MS_BIND | unix.MS_REMOUNT | unix.MS_NOATIME, ""},
		{engineImage + containerdName, containerdReal, "", unix.MS_BIND, ""},
		{initPath(), containerdName, "", unix.MS_BIND, ""},
		{"", containerdName, "", unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV, ""},
		{initPath(), engineStagePath, "", unix.MS_BIND, ""},
		{"", engineStagePath, "", unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV, ""},
		{engineOSRelease, "/etc/os-release", "", unix.MS_BIND, ""},
		{"", "/etc/os-release", "", unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | quiet, ""},
	}
	if err := engineRootLayer(engineNodeName(e.sys.cmdline)); err != nil {
		return err
	}
	if binds := e.sys.cluster().k8sStartSteps(); len(binds) > 0 {
		for _, b := range binds {
			os.MkdirAll(engineUpper+"/upper"+b.target, 0o755)
			steps = append(steps, struct {
				source, target, fstype string
				flags                  uintptr
				data                   string
			}{b.source, b.target, "", unix.MS_BIND, ""})
			if b.ro {
				steps = append(steps, struct {
					source, target, fstype string
					flags                  uintptr
					data                   string
				}{"", b.target, "", unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY, ""})
			}
		}
		run.k8sBound.Store(true)
		e.sys.cluster().reg.arm()
	}
	os.Mkdir("/dev/mqueue", 0o755)
	for _, m := range steps {
		if err := unix.Mount(m.source, engineRoot+m.target, m.fstype, m.flags, m.data); err != nil {
			return fmt.Errorf("mount %s on the engine's %s: %w", m.source, filepath.Join("/", m.target), err)
		}
	}
	if view {
		if err := unix.Mount(machinesView, engineRoot+"/mnt/machines", "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("mount %s on the engine's /mnt/machines: %w", machinesView, err)
		}
	}
	if !e.sys.share.bind(engineRoot) {
		if err := unix.Mount("tmpfs", engineRoot+"/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777"); err != nil {
			return fmt.Errorf("mount tmpfs on the engine's /tmp: %w", err)
		}
	}
	e.sys.share.refuseUnmapped(engineRoot)
	bindMacZoneinfo(engineRoot, shareMount)
	run.agentAt = e.sys.agentPath()
	if err := bindMacAgent(engineRoot, sshAuthSock, run.agentAt); err != nil {
		e.sys.log.Logf(logbuf.Info, "engine: the SSH agent is not at the Mac's SSH_AUTH_SOCK path: %v", err)
	}
	if err := unix.Chdir(engineRoot); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", "."); err == nil {
		if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
			return fmt.Errorf("detach init's root: %w", err)
		}
	} else {
		e.sys.log.Logf(logbuf.Warning, "engine: pivot_root failed (%v); moving the root instead", err)
		if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("move the engine's root: %w", err)
		}
		if err := unix.Chroot("."); err != nil {
			return err
		}
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SHARED, ""); err != nil {
		return fmt.Errorf("make the engine's root shared: %w", err)
	}
	args := append(append([]string{engineStageName}, engineArgs...), extraEngineArgs(e.sys.cmdline)...)
	child, err := e.sys.reaper.Start(engineStagePath, args, &os.ProcAttr{
		Dir:   "/",
		Env:   e.sys.engineEnvironment(),
		Files: []*os.File{stdin, output, output, report},
		Sys: &syscall.SysProcAttr{
			Setsid:       true,
			Cloneflags:   unix.CLONE_NEWPID,
			UseCgroupFD:  true,
			CgroupFD:     int(cgroup.Fd()),
			Unshareflags: unix.CLONE_NEWCGROUP,
		},
	})
	if err != nil {
		return fmt.Errorf("start the engine's supervisor: %w", err)
	}
	ok = true
	run.child, run.mnt = child, mnt
	return nil
}

func (e *engine) listenSSH() error {
	l, err := net.Listen("unix", sshAuthSock)
	if err != nil {
		return err
	}
	if err := os.Chmod(sshAuthSock, 0o666); err != nil {
		l.Close()
		return err
	}
	e.mu.Lock()
	e.ssh = l
	e.mu.Unlock()
	go e.sys.requests.Serve(l, api.StreamSSHAgent)
	return nil
}

func (e *engine) pump(r *os.File) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for {
		for sc.Scan() {
			line := sc.Text()
			level := logbuf.Info
			switch {
			case strings.Contains(line, "level=error"), strings.Contains(line, "level=fatal"):
				level = logbuf.Error
			case strings.Contains(line, "level=warning"):
				level = logbuf.Warning
			case strings.Contains(line, "level=debug"):
				level = logbuf.Debug
			}
			e.out.Logf(level, "%s", line)
			if why, ok := strings.CutPrefix(line, "failed to start daemon: "); ok {
				e.mu.Lock()
				e.lastFailure = clipText(strings.TrimSpace(why), 400)
				e.mu.Unlock()
			}
		}
		if !errors.Is(sc.Err(), bufio.ErrTooLong) {
			return
		}
		e.out.Logf(logbuf.Warning, "(a line over %d bytes was dropped)", 64<<10)
		sc = bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 64<<10)
	}
}

func (e *engine) stop(run *supervise.Run, grace time.Duration) {
	if rcuForEngine(e.sys.cmdline) {
		defer e.sys.rcu.hold()()
	}
	e.mu.Lock()
	cur := e.cur
	e.mu.Unlock()
	if cur == nil || cur.dockerd != run.PID {
		return
	}
	cur.ended.Store(true)
	e.sys.sessions.Kill(api.ExecEngine)
	began := time.Now()
	if each := grace - engineExit; each >= time.Second {
		drained := e.sys.cluster().drainForStop(each)
		if err := e.stopContainers(each); err != nil {
			e.sys.log.Logf(logbuf.Warning, "engine: stopping containers: %v", err)
		}
		if drained != nil {
			select {
			case <-drained:
			case <-time.After(max(each-time.Since(began), 0) + k3sStopWait):
				e.sys.log.Logf(logbuf.Warning, "engine: the cluster's pods were not stopped in %v", each)
			}
		}
		e.sys.cluster().reg.disarm()
	}
	e.sys.reaper.Signal(cur.child, unix.SIGTERM, false)
	if waitEmpty(engineCgroup, max(grace-time.Since(began), 0)) {
		e.sys.log.Logf(logbuf.Info, "engine: stopped in %d ms", time.Since(began).Milliseconds())
	} else {
		e.sys.log.Logf(logbuf.Warning, "engine: still running %v after being asked to stop; killing what is left", grace)
	}
	os.WriteFile(filepath.Join(engineCgroup, "cgroup.kill"), []byte("1"), 0)
	if !waitEmpty(engineCgroup, killWait) {
		e.sys.log.Logf(logbuf.Error, "engine: processes are left in its cgroup after the kill")
	}
	select {
	case <-run.Exited:
	case <-time.After(killWait):
		e.sys.reaper.Signal(cur.child, unix.SIGKILL, false)
	}
}

const engineExit = 3 * time.Second

func (e *engine) stopContainers(each time.Duration) error {
	if _, err := os.Stat(engineSocket); err != nil {
		return nil
	}
	seconds := int(each / time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), each+2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://engine/containers/json", nil)
	resp, err := e.http.Do(req)
	if err != nil {
		return err
	}
	var containers []struct{ Id string }
	err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&containers)
	resp.Body.Close()
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	for _, c := range containers {
		if c.Id == "" || strings.ContainsAny(c.Id, "/?#") {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
				"http://engine/containers/"+c.Id+"/stop?t="+strconv.Itoa(seconds), nil)
			if resp, err := e.http.Do(req); err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	if len(containers) > 0 {
		e.sys.log.Logf(logbuf.Info, "engine: stopped %d containers", len(containers))
	}
	return ctx.Err()
}

func (e *engine) In(fn func(cgroup int) error) error {
	e.mu.Lock()
	run := e.cur
	e.mu.Unlock()
	if run == nil || e.sup.Status().State != api.EngineReady {
		return errors.New("the engine is not ready")
	}
	cgroup, err := os.Open(filepath.Join(engineCgroup, "exec"))
	if err != nil {
		return err
	}
	defer cgroup.Close()
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		run.ns.RLock()
		defer run.ns.RUnlock()
		if run.mnt == nil || run.pid == nil {
			result <- errors.New("the engine has stopped")
			return
		}
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			result <- fmt.Errorf("unshare: %w", err)
			return
		}
		if err := unix.Setns(int(run.mnt.Fd()), unix.CLONE_NEWNS); err != nil {
			result <- fmt.Errorf("enter the engine's mount namespace: %w", err)
			return
		}
		if err := unix.Setns(int(run.pid.Fd()), unix.CLONE_NEWPID); err != nil {
			result <- fmt.Errorf("enter the engine's PID namespace: %w", err)
			return
		}
		result <- fn(int(cgroup.Fd()))
	}()
	return <-result
}

func engineRootLayer(hostname string) error {
	upper, work := engineUpper+"/upper", engineUpper+"/work"
	for _, d := range []string{upper + "/etc", work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("the engine root's writable layer: %w", err)
		}
	}
	dropImageCopies(upper, engineImage)
	dropShadowedLinks(upper, engineImage)
	for _, f := range []string{"/etc/localtime", "/etc/timezone", "/etc/hostname"} {
		if st, err := os.Lstat(upper + f); err == nil && st.IsDir() {
			os.RemoveAll(upper + f)
		}
	}
	zone, data, ok := macTimeZone(shareMount)
	if !ok {
		zone, data = "Etc/UTC", utcTZif
	}
	os.Remove(upper + "/etc/localtime")
	os.WriteFile(upper+"/etc/localtime", data, 0o644)
	os.WriteFile(upper+"/etc/timezone", []byte(zone+"\n"), 0o644)
	os.WriteFile(upper+"/etc/hostname", []byte(hostname+"\n"), 0o644)
	if st, err := os.Lstat(upper + "/etc/machine-id"); err == nil && st.IsDir() {
		os.RemoveAll(upper + "/etc/machine-id")
	}
	if _, err := os.Stat(upper + "/etc/machine-id"); err != nil {
		var id [16]byte
		if _, err := rand.Read(id[:]); err == nil {
			os.WriteFile(upper+"/etc/machine-id", []byte(hex.EncodeToString(id[:])+"\n"), 0o444)
		}
	}
	for _, f := range hostEtc {
		path := upper + f.path
		if _, err := os.Lstat(engineImage + f.path); err == nil {
			continue
		}
		os.MkdirAll(filepath.Dir(path), 0o755)
		if st, err := os.Lstat(path); err == nil && st.IsDir() {
			os.RemoveAll(path)
		}
		if f.link != "" {
			if t, err := os.Readlink(path); err != nil || t != f.link {
				os.Remove(path)
				os.Symlink(f.link, path)
			}
			continue
		}
		os.WriteFile(path, []byte(f.text), 0o644)
	}
	for _, d := range []string{"/srv", "/etc/opt"} {
		if _, err := os.Lstat(engineImage + d); err != nil {
			os.MkdirAll(upper+d, 0o755)
		}
	}
	var u unix.Utsname
	if lib, ok := inShare(engineImage, "/lib"); ok && unix.Uname(&u) == nil {
		os.MkdirAll(upper+strings.TrimPrefix(lib, engineImage)+"/modules/"+unix.ByteSliceToString(u.Release[:]), 0o755)
	}
	return nil
}

func dropShadowedLinks(upper, image string) {
	entries, _ := os.ReadDir(upper)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if st, err := os.Lstat(image + "/" + e.Name()); err == nil && st.Mode()&os.ModeSymlink != 0 {
			os.RemoveAll(upper + "/" + e.Name())
		}
	}
}

var utcTZif = append(append([]byte("TZif"), make([]byte, 16+4*4)...),
	0, 0, 0, 1,
	0, 0, 0, 4,
	0, 0, 0, 0, 0, 0,
	'U', 'T', 'C', 0)

var hostEtc = []struct{ path, link, text string }{
	{path: "/etc/mtab", link: "../proc/mounts"},
	{path: "/etc/docker/daemon.json", link: "../../run/" + engineConfigName},
	{path: "/etc/fstab", text: "# fev's engine: its filesystems are mounted by fev-init, none from\n# this file. The mounts in force are in /proc/mounts (/etc/mtab).\n"},
	{path: "/etc/environment", text: "# Read by pam_env: KEY=VALUE lines. fev sets nothing here.\n"},
	{path: "/etc/sysctl.conf", text: "# fev's engine: no settings of its own here; fev-init sets the\n# kernel's at boot.\n"},
	{path: "/etc/modules", text: "# fev's kernel has the features a Docker host loads as modules built\n# in: nothing to load at boot.\n"},
}

func dropImageCopies(upper, image string) {
	filepath.WalkDir(upper, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path, upper)
		switch rel {
		case "/etc/localtime", "/etc/timezone", "/etc/hostname", "/etc/machine-id":
			return nil
		}
		for _, f := range hostEtc {
			if rel == f.path {
				return nil
			}
		}
		if _, err := os.Lstat(image + rel); err == nil {
			os.Remove(path)
		}
		return nil
	})
}

func bindMacZoneinfo(root, share string) {
	src, ok := inShare(share, "/usr/share/zoneinfo")
	if !ok {
		return
	}
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return
	}
	target := root + "/usr/share/zoneinfo"
	if os.MkdirAll(target, 0o755) != nil {
		return
	}
	if unix.Mount(src, target, "", unix.MS_BIND, "") == nil {
		unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, "")
	}
}

func macTimeZone(share string) (string, []byte, bool) {
	target, err := os.Readlink(share + "/private/etc/localtime")
	if err != nil {
		return "", nil, false
	}
	i := strings.Index(target, "zoneinfo/")
	if i < 0 {
		return "", nil, false
	}
	zone := target[i+len("zoneinfo/"):]
	file, ok := inShare(share, "/private/etc/localtime")
	if !ok {
		return "", nil, false
	}
	data, err := os.ReadFile(file)
	if err != nil || len(data) < 4 || string(data[:4]) != "TZif" || len(data) > 1<<20 {
		return "", nil, false
	}
	return zone, data, true
}

func inShare(share, path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	cur := ""
	for links := 0; len(parts) > 0; {
		p := parts[0]
		parts = parts[1:]
		switch p {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			if cur == "." || cur == "/" {
				cur = ""
			}
			continue
		}
		next := cur + "/" + p
		st, err := os.Lstat(share + next)
		if err != nil {
			return "", false
		}
		if st.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if links++; links > 40 {
			return "", false
		}
		t, err := os.Readlink(share + next)
		if err != nil {
			return "", false
		}
		if strings.HasPrefix(t, "/") {
			cur = ""
		}
		parts = append(strings.Split(strings.Trim(t, "/"), "/"), parts...)
	}
	return share + cur, true
}
