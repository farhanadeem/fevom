package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	engineStageName = "fev-engine"
	engineStagePath = engineFevDir + "/" + engineStageName
	engineReportFD  = 3
)

func isEngineStage(args []string) bool { return len(args) > 0 && args[0] == engineStageName }

const (
	reportDockerd    = "dockerd"
	reportExit       = "exit"
	reportError      = "error"
	reportK8s        = "k8s"
	reportK8sDrained = "k8s-drained"
	reportK8sSysctl  = "k8s-sysctl"
)

var forwarded = map[os.Signal]bool{unix.SIGTERM: true, unix.SIGINT: true, unix.SIGHUP: true, unix.SIGUSR1: true}

func engineStageMain() {
	report := os.NewFile(engineReportFD, "report")
	unix.CloseOnExec(engineReportFD)
	fail := func(format string, args ...any) {
		fmt.Fprintf(report, reportError+" "+format+"\n", args...)
		os.Exit(127)
	}
	sig := make(chan os.Signal, 64)
	signal.Notify(sig, unix.SIGCHLD, unix.SIGTERM, unix.SIGINT, unix.SIGHUP, unix.SIGQUIT, unix.SIGUSR1, unix.SIGUSR2)
	if len(os.Args) < 2 {
		fail("no command to supervise")
	}
	leaves, err := engineStageSetup("/proc", "/sys/fs/cgroup")
	if err != nil {
		fail("%v", err)
	}
	st := &engineStage{report: report, leaves: leaves, dockerdArgv: os.Args[1:], env: os.Environ(), events: make(chan string, 64), ready: make(chan int, 4)}
	st.cmdline, _ = os.ReadFile("/proc/cmdline")
	if err := st.listenContainerd(); err != nil {
		fail("containerd's notify socket: %v", err)
	}
	st.readWant()
	st.started = time.Now()
	st.reconcile()
	if st.failed != "" {
		fail("%s", st.failed)
	}
	for {
		select {
		case s := <-sig:
			switch {
			case s == unix.SIGCHLD:
				st.reaped()
			case s == unix.SIGUSR2:
				st.readWant()
			case s == unix.SIGQUIT:
				buf := make([]byte, 1<<20)
				n := runtime.Stack(buf, true)
				st.logf("goroutines at SIGQUIT:\n%s", buf[:n])
			case s == unix.SIGTERM || s == unix.SIGINT:
				st.stopping = true
				if st.dockerd > 0 {
					unix.Kill(st.dockerd, s.(syscall.Signal))
				}
			case forwarded[s]:
				if st.dockerd > 0 {
					unix.Kill(st.dockerd, s.(syscall.Signal))
				}
			}
		case pid := <-st.ready:
			if pid == st.ctr && st.ctr > 0 {
				st.ctrReady = true
				st.logf("containerd ready (pid %d, cri %v) %d ms after its start", st.ctr, st.ctrCRI, time.Since(st.ctrAt).Milliseconds())
				if st.ctrLost {
					st.ctrLost = false
					st.forwardExits()
				}
			}
		case ev := <-st.events:
			st.event(ev)
		}
		st.reconcile()
		if st.failed != "" {
			fail("%s", st.failed)
		}
	}
}

const containerdNotify = "@fev/notify/containerd"

type engineStage struct {
	report      *os.File
	leaves      map[string]*os.File
	dockerdArgv []string
	env         []string
	cmdline     []byte
	events      chan string
	ready       chan int
	started     time.Time

	ctr                           int
	ctrCRI, ctrReady, ctrStopping bool
	ctrAt                         time.Time
	ctrWaited                     bool
	ctrCrashes                    []time.Time
	ctrHeld                       bool
	ctrLost                       bool

	dockerd       int
	dockerdDone   bool
	dockerdStatus unix.WaitStatus

	stopping bool
	failed   string

	k8s k8sStage
}

func (st *engineStage) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fev-engine: "+format+"\n", args...)
}

func (st *engineStage) after(d time.Duration, ev string) {
	time.AfterFunc(d, func() { st.events <- ev })
}

func (st *engineStage) listenContainerd() error {
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: containerdNotify, Net: "unixgram"})
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
				return
			}
			for _, line := range strings.Split(string(buf[:n]), "\n") {
				if line == "READY=1" {
					st.ready <- senderPid(oob[:oobn])
				}
			}
		}
	}()
	return nil
}

func (st *engineStage) spawn(leaf string, argv, env []string) (int, error) {
	attr := &syscall.SysProcAttr{Setsid: true, UseCgroupFD: true, CgroupFD: int(st.leaves[leaf].Fd())}
	return syscall.ForkExec(argv[0], argv, &syscall.ProcAttr{Dir: "/", Env: env, Files: []uintptr{0, 1, 2}, Sys: attr})
}

func (st *engineStage) envWith(sock string) []string {
	var out []string
	for _, e := range st.env {
		if !strings.HasPrefix(e, "NOTIFY_SOCKET=") && !strings.HasPrefix(e, "PATH=") {
			out = append(out, e)
		}
	}
	out = append(out, "PATH="+runcByName+":/usr/sbin:/usr/bin:/sbin:/bin")
	if sock != "" {
		out = append(out, "NOTIFY_SOCKET="+sock)
	}
	return out
}

func (st *engineStage) startContainerd() {
	cri := st.wantCRI()
	config := containerdConfig
	if cri {
		config = containerdConfigCRI
	}
	pid, err := st.spawn("containerd", []string{containerdReal, "--config", config}, st.envWith(containerdNotify))
	if err != nil {
		st.failed = fmt.Sprintf("start containerd: %v", err)
		return
	}
	st.ctr, st.ctrCRI, st.ctrReady, st.ctrAt, st.ctrWaited = pid, cri, false, time.Now(), false
	st.logf("containerd started (pid %d, cri %v)", pid, cri)
	st.after(4*time.Second, fmt.Sprintf("ctr-wait %d", pid))
}

func (st *engineStage) startDockerd() {
	pid, err := st.spawn("engine", st.dockerdArgv, st.env)
	if err != nil {
		st.failed = fmt.Sprintf("start %s: %v", st.dockerdArgv[0], err)
		return
	}
	st.dockerd = pid
	st.logf("dockerd started %d ms after the supervisor", time.Since(st.started).Milliseconds())
	fmt.Fprintf(st.report, "%s %d\n", reportDockerd, pid)
}

func (st *engineStage) forwardExits() {
	go func() {
		for _, d := range []time.Duration{time.Second, 4 * time.Second} {
			time.Sleep(d)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			n, all, err := forwardLostExits(ctx, containerdSocket, "/run/docker.sock")
			cancel()
			if err != nil {
				st.logf("the exits containerd's outage lost: %v", err)
				continue
			}
			if n > 0 {
				st.logf("after containerd's outage: %d of the %d containers dockerd held running had ended; their exits forwarded", n, all)
			}
		}
	}()
}

func (st *engineStage) restartContainerd() {
	st.logf("restarting containerd for cri %v", st.wantCRI())
	unix.Kill(st.ctr, unix.SIGTERM)
	st.ctrStopping = true
	st.after(10*time.Second, fmt.Sprintf("ctr-kill %d", st.ctr))
}

func (st *engineStage) reconcile() {
	defer st.k8s.tell(st)
	if st.stopping {
		st.k8sStopping()
		if st.dockerd == 0 && !st.dockerdDone {
			st.dockerdDone = true
		}
		if st.dockerdDone && st.k8s.idle() {
			if st.ctr > 0 && !st.ctrStopping {
				unix.Kill(st.ctr, unix.SIGTERM)
				st.ctrStopping = true
				st.after(10*time.Second, fmt.Sprintf("ctr-kill %d", st.ctr))
			}
			if st.ctr == 0 {
				st.k8s.tell(st)
				fmt.Fprintf(st.report, "%s %d\n", reportExit, uint32(st.dockerdStatus))
				os.Exit(exitCode(st.dockerdStatus))
			}
		}
		return
	}
	if st.ctr == 0 && !st.ctrHeld {
		st.startContainerd()
		return
	}
	if st.ctr > 0 && st.ctrCRI != st.wantCRI() && !st.ctrStopping && st.k8s.mayRestartContainerd() {
		st.restartContainerd()
		return
	}
	if st.dockerd == 0 && !st.dockerdDone && (st.ctrReady || st.ctrWaited) {
		st.startDockerd()
	}
	st.k8sReconcile()
}

func (st *engineStage) event(ev string) {
	word, arg, _ := strings.Cut(ev, " ")
	pid, _ := strconv.Atoi(arg)
	switch word {
	case "ctr-wait":
		if pid == st.ctr && !st.ctrReady {
			st.logf("containerd (pid %d) has not said it is ready after 4 s", pid)
			st.ctrWaited = true
		}
	case "ctr-kill":
		if pid == st.ctr && st.ctrStopping {
			unix.Kill(pid, unix.SIGKILL)
		}
	case "ctr-release":
		st.ctrHeld = false
	default:
		st.k8sEvent(word, arg, pid)
	}
}

func (st *engineStage) reaped() {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || pid <= 0 {
			return
		}
		switch pid {
		case st.dockerd:
			st.dockerd, st.dockerdDone, st.dockerdStatus = 0, true, ws
			st.stopping = true
			st.logf("dockerd ended (%v)", ws)
		case st.ctr:
			planned := st.ctrStopping
			st.ctr, st.ctrReady, st.ctrStopping = 0, false, false
			if !planned && !st.stopping {
				st.ctrLost = true
				st.logf("containerd ended by itself (%v); starting it again", ws)
				now := time.Now()
				st.ctrCrashes = append(st.ctrCrashes, now)
				recent := 0
				for _, t := range st.ctrCrashes {
					if now.Sub(t) < 30*time.Second {
						recent++
					}
				}
				if recent >= 3 {
					st.ctrHeld = true
					st.after(5*time.Second, "ctr-release")
				}
			}
		default:
			st.k8sReaped(pid, ws)
		}
	}
}

func engineStageSetup(proc, cgroup string) (map[string]*os.File, error) {
	if err := unix.Mount("proc", proc, "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return nil, fmt.Errorf("mount the engine's /proc: %w", err)
	}
	for _, leaf := range engineTree.Own {
		if err := os.Mkdir(filepath.Join(cgroup, leaf), 0o755); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(cgroup, engineSupervisorLeaf, "cgroup.procs"), []byte("0"), 0); err != nil {
		return nil, fmt.Errorf("move into the supervisor's cgroup: %w", err)
	}
	controllers, err := os.ReadFile(filepath.Join(cgroup, "cgroup.controllers"))
	if err != nil {
		return nil, err
	}
	if fields := strings.Fields(string(controllers)); len(fields) > 0 {
		enable := "+" + strings.Join(fields, " +")
		if err := os.WriteFile(filepath.Join(cgroup, "cgroup.subtree_control"), []byte(enable), 0); err != nil {
			return nil, fmt.Errorf("delegate controllers to the engine: %w", err)
		}
	}
	leaves := map[string]*os.File{}
	for _, name := range []string{"engine", "containerd", "k3s"} {
		leaf, err := os.Open(filepath.Join(cgroup, name))
		if err != nil {
			return nil, fmt.Errorf("open the engine's cgroup %s: %w", name, err)
		}
		leaves[name] = leaf
	}
	return leaves, nil
}

func reapAll(dockerd int) (unix.WaitStatus, bool) {
	var (
		dws  unix.WaitStatus
		done bool
	)
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || pid <= 0 {
			return dws, done
		}
		if pid == dockerd {
			dws, done = ws, true
		}
	}
}

func exitCode(ws unix.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

type supervisorReport struct {
	dockerd int
	exited  bool
	status  unix.WaitStatus
	err     error
}

func readReport(br *bufio.Reader, r *supervisorReport, k func(word, rest string)) (bool, error) {
	line, err := br.ReadString('\n')
	if line == "" {
		return false, err
	}
	word, rest, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
	switch word {
	case reportDockerd:
		n, err := strconv.Atoi(rest)
		if err != nil || n <= 0 {
			return true, fmt.Errorf("the engine's supervisor reported %q", line)
		}
		r.dockerd = n
	case reportExit:
		n, err := strconv.ParseUint(rest, 10, 32)
		if err != nil {
			return true, fmt.Errorf("the engine's supervisor reported %q", line)
		}
		r.exited, r.status = true, unix.WaitStatus(n)
	case reportError:
		r.err = errors.New(rest)
	case reportK8s, reportK8sDrained, reportK8sSysctl:
		if k != nil {
			k(word, rest)
		}
	default:
		return true, fmt.Errorf("the engine's supervisor reported %q", line)
	}
	return true, nil
}

func outerPid(proc string, sup, inner int) int {
	tasks, err := os.ReadDir(filepath.Join(proc, strconv.Itoa(sup), "task"))
	if err != nil {
		return 0
	}
	for _, t := range tasks {
		data, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(sup), "task", t.Name(), "children"))
		if err != nil {
			continue
		}
		for _, c := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(c)
			if err != nil {
				continue
			}
			if nsPidOf(proc, pid) == inner {
				return pid
			}
		}
	}
	return 0
}

func nsPidOf(proc string, pid int) int {
	data, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "NSpid:"); ok {
			f := strings.Fields(rest)
			if len(f) == 0 {
				return 0
			}
			n, _ := strconv.Atoi(f[len(f)-1])
			return n
		}
	}
	return 0
}
