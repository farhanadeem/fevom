package vm

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"fev/host/daemon/home"
	"fev/host/netstack"
	"fev/host/storage"
	"fev/proto/agent"
	"fev/proto/vmmctl"
)

const (
	PortAgent  = agent.Port
	PortDocker = agent.DockerPort
	PortRelay  = 1026
	PortView   = agent.ViewVsockPort
)

const lockName = "vm.lock"

const maxLine = 4096

type Process struct {
	Bin            string
	Prefix         []string
	Kernel, Initrd string
	Cmdline        string
	DataImage      string
	SystemImage    string
	Dir            string
	Net            bool
	Share          *Share
	NoHostLoopback *atomic.Bool
	NetOptions     func(o *netstack.Options)
	OnStack        func(s *netstack.Stack)
	Control        bool
	Extra          []string
	BootWords      func() []string

	orphan      int
	orphanSince time.Time
	StandIn     bool
	Log         *slog.Logger
}

func (p *Process) Fake() bool { return p.StandIn }

const (
	ShareTag   = "mac"
	ShareMount = "/mnt/mac"
)

type Share struct {
	Root string
	Deny []string
}

func (s Share) Arg() (string, error) {
	arg := "tag=" + ShareTag + ",path=" + s.Root
	for _, path := range append([]string{s.Root}, s.Deny...) {
		if !filepath.IsAbs(path) || strings.ContainsAny(path, ",\x00") {
			return "", fmt.Errorf("the path %q cannot be given to the VMM's --share (it must be absolute and have no comma)", path)
		}
	}
	for _, d := range s.Deny {
		arg += ",deny=" + d
	}
	return arg, nil
}

func (p *Process) AgentSocket() string {
	return filepath.Join(p.Dir, fmt.Sprintf("host_%d.sock", agent.HostPort))
}

func (p *Process) netSocket() string { return filepath.Join(p.Dir, "net.sock") }

type process struct {
	p   *Process
	cmd *exec.Cmd

	control *vmmctl.Client

	mu       sync.Mutex
	lifeline *os.File
	stack    *netstack.Stack
}

var OrphanGrace = 2 * time.Second

const orphanWait = 5 * time.Second

func (p *Process) orphaned(pid int) bool {
	if pid <= 1 || pid == os.Getpid() {
		return false
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || info == nil || info.Proc.P_pid != int32(pid) {
		return false
	}
	name := unix.ByteSliceToString(info.Proc.P_comm[:])
	want := filepath.Base(p.Bin)
	if len(want) > len(info.Proc.P_comm)-1 {
		want = want[:len(info.Proc.P_comm)-1]
	}
	return info.Eproc.Ppid == 1 && name == want
}

func (p *Process) reapOrphan(lockPath string) (*os.File, error) {
	stillRunning := fmt.Errorf("the VMM of an earlier daemon is still running in %s (it holds %s)", p.Dir, lockName)
	data, _ := os.ReadFile(lockPath)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if !p.orphaned(pid) {
		p.orphan, p.orphanSince = 0, time.Time{}
		return nil, stillRunning
	}
	if p.orphan != pid {
		p.orphan, p.orphanSince = pid, time.Now()
		return nil, stillRunning
	}
	if time.Since(p.orphanSince) < OrphanGrace {
		return nil, stillRunning
	}
	p.Log.Warn("the VMM of an earlier daemon outlived it and ignored the end of its lifeline; killing it", "pid", pid, "waited", time.Since(p.orphanSince).Round(time.Millisecond))
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return nil, fmt.Errorf("%w; it is an orphan (pid %d) and could not be killed: %v", stillRunning, pid, err)
	}
	p.orphan, p.orphanSince = 0, time.Time{}
	type got struct {
		f   *os.File
		err error
	}
	done := make(chan got, 1)
	go func() {
		f, _, err := home.Lock(lockPath, true)
		done <- got{f, err}
	}()
	select {
	case g := <-done:
		if g.err != nil {
			return nil, g.err
		}
		return g.f, nil
	case <-time.After(orphanWait):
		go func() {
			if g := <-done; g.f != nil {
				g.f.Close()
			}
		}()
		return nil, fmt.Errorf("%w; it was killed (pid %d) and still holds the directory after %v", stillRunning, pid, orphanWait)
	}
}

func (p *Process) Start(spec Spec, ev Events) (Instance, error) {
	if p.Log == nil {
		p.Log = slog.Default()
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(p.Dir, 0o700); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(p.Dir, lockName)
	lock, held, err := home.Lock(lockPath, false)
	if err != nil {
		return nil, err
	}
	if held {
		if lock, err = p.reapOrphan(lockPath); err != nil {
			return nil, err
		}
	}
	defer lock.Close()
	if entries, err := os.ReadDir(p.Dir); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".sock") {
				os.Remove(filepath.Join(p.Dir, e.Name()))
			}
		}
	}
	args := append([]string{}, p.Prefix...)
	if p.Kernel != "" {
		args = append(args, "--kernel", p.Kernel)
	}
	if p.Initrd != "" {
		args = append(args, "--initrd", p.Initrd)
	}
	var words []string
	if p.BootWords != nil {
		words = p.BootWords()
	}
	if !p.StandIn {
		args = append(args, rosettaArgs(words)...)
	}
	if spec.EngineOff {
		words = append(words, agent.EngineCmdlineParam+"=0")
	}
	if spec.K8s && !spec.EngineOff {
		words = append(words, agent.K8sCmdlineParam+"=1")
	}
	if spec.NodeName != "" && spec.NodeName != agent.DefaultNodeName && agent.CheckNodeName(spec.NodeName) == nil {
		words = append(words, agent.EngineNodeNameParam+"="+spec.NodeName)
	}
	if p.Cmdline != "" {
		cmdline := DataAllocated(p.Cmdline, p.DataImage)
		for _, w := range words {
			cmdline += " " + w
		}
		args = append(args, "--cmdline", Cmdline(cmdline, spec.Verbose))
	} else if p.StandIn && len(words) > 0 {
		args = append(args, "--cmdline", strings.Join(words, " "))
	}
	args = append(args, "--cpus", strconv.Itoa(spec.CPUs), "--mem", strconv.Itoa(spec.MemMiB))
	if p.DataImage != "" {
		args = append(args, "--disk", p.DataImage, "--disk-sync", "paced")
		if p.SystemImage != "" {
			args = append(args, "--disk", p.SystemImage+",ro")
		}
		swap := storage.SwapImagePath(p.DataImage)
		if st, err := os.Stat(swap); err == nil && st.Mode().IsRegular() {
			args = append(args, "--disk", swap+",swap")
		}
	}

	var listeners []net.Listener
	closeAll := func() {
		for _, l := range listeners {
			l.Close()
		}
	}
	agentListener, err := net.Listen("unix", p.AgentSocket())
	if err != nil {
		return nil, err
	}
	listeners = append(listeners, agentListener)
	var netListener net.Listener
	if p.Net {
		if netListener, err = net.Listen("unix", p.netSocket()); err != nil {
			closeAll()
			return nil, err
		}
		listeners = append(listeners, netListener)
		args = append(args, "--net", p.netSocket())
	}
	if spec.Share && p.Share != nil {
		share, err := p.Share.Arg()
		if err != nil {
			closeAll()
			return nil, err
		}
		for _, d := range p.Share.Deny {
			if err := os.MkdirAll(d, 0o700); err != nil {
				closeAll()
				return nil, err
			}
		}
		args = append(args, "--share", share)
	}
	args = append(args, "--vsock-dir", p.Dir)
	for _, port := range []uint32{PortAgent, PortDocker, PortRelay, PortView} {
		args = append(args, "--vsock-port", strconv.FormatUint(uint64(port), 10))
	}
	args = append(args, "--console", "stdio", "--stamp", "--no-reboot", "--verbose", "--trace-fd", "3", "--lifeline-fd", "4")
	args = append(args, p.Extra...)

	var pipes []*os.File
	closePipes := func() {
		for _, f := range pipes {
			f.Close()
		}
	}
	pipe := func() (r, w *os.File, err error) {
		if r, w, err = os.Pipe(); err == nil {
			pipes = append(pipes, r, w)
		}
		return
	}
	consoleR, consoleW, err := pipe()
	var logR, logW, traceR, traceW, lifeR, lifeW *os.File
	if err == nil {
		logR, logW, err = pipe()
	}
	if err == nil {
		traceR, traceW, err = pipe()
	}
	if err == nil {
		lifeR, lifeW, err = pipe()
	}
	if err != nil {
		closePipes()
		closeAll()
		return nil, err
	}

	var controlOurs, controlTheirs *os.File
	if p.Control {
		syscall.ForkLock.RLock()
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err == nil {
			unix.CloseOnExec(fds[0])
			unix.CloseOnExec(fds[1])
		}
		syscall.ForkLock.RUnlock()
		if err != nil {
			closePipes()
			closeAll()
			return nil, fmt.Errorf("cannot make the control socket: %w", err)
		}
		controlOurs, controlTheirs = os.NewFile(uintptr(fds[0]), "control"), os.NewFile(uintptr(fds[1]), "control-vmm")
		pipes = append(pipes, controlOurs, controlTheirs)
		args = append(args, vmmctl.Flag, "6")
	}

	cmd := exec.Command(p.Bin, args...)
	cmd.Stdout = consoleW
	cmd.Stderr = logW
	cmd.ExtraFiles = []*os.File{traceW, lifeR, lock}
	if controlTheirs != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, controlTheirs)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		closePipes()
		closeAll()
		return nil, fmt.Errorf("cannot start %s: %w", p.Bin, err)
	}
	consoleW.Close()
	logW.Close()
	traceW.Close()
	lifeR.Close()

	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		p.Log.Debug("cannot note the VMM's pid in its lock file", "err", err)
	}

	inst := &process{p: p, cmd: cmd, lifeline: lifeW}
	if controlTheirs != nil {
		controlTheirs.Close()
		inst.control = vmmctl.NewClient(controlOurs)
	}
	var readers sync.WaitGroup
	readers.Add(3)
	go func() {
		defer readers.Done()
		readLines(traceR, func(line string) {
			var m Mark
			if json.Unmarshal([]byte(line), &m) == nil && m.Name != "" {
				ev.Mark(m)
			}
		})
		traceR.Close()
	}()
	go func() {
		defer readers.Done()
		readLines(logR, ev.VMMLine)
		logR.Close()
	}()
	go func() {
		defer readers.Done()
		readLines(consoleR, ev.ConsoleLine)
		consoleR.Close()
	}()
	go func() {
		for {
			conn, err := agentListener.Accept()
			if err != nil {
				return
			}
			ev.AgentConn(conn)
		}
	}()
	if netListener != nil {
		go inst.serveNet(netListener)
	}
	go func() {
		readers.Wait()
		err := cmd.Wait()
		exit := Exit{At: time.Now()}
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			exit.Code, exit.Signal = -1, unix.SignalName(ws.Signal())
		} else if cmd.ProcessState != nil {
			exit.Code = cmd.ProcessState.ExitCode()
		} else if err != nil {
			exit.Code = -1
		}
		closeAll()
		if inst.control != nil {
			inst.control.Close()
		}
		inst.mu.Lock()
		stack := inst.stack
		inst.stack = nil
		if inst.lifeline != nil {
			inst.lifeline.Close()
			inst.lifeline = nil
		}
		inst.mu.Unlock()
		if stack != nil {
			stack.Close()
		}
		ev.Exited(exit)
	}()
	return inst, nil
}

const AllocatedParam = "fev.data_allocated"

func DataAllocated(cmdline, image string) string {
	if image == "" {
		return cmdline
	}
	n, err := storage.Allocated(image)
	if err != nil {
		return cmdline
	}
	return cmdline + " " + AllocatedParam + "=" + strconv.FormatInt(n, 10)
}

func (i *process) serveNet(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		i.mu.Lock()
		busy := i.stack != nil
		i.mu.Unlock()
		if busy {
			conn.Close()
			continue
		}
		opts := netstack.Options{Host: netstack.MacHost{}, Log: i.p.Log, NoHostLoopback: i.p.NoHostLoopback}
		if i.p.NetOptions != nil {
			i.p.NetOptions(&opts)
		}
		stack, err := netstack.New(conn, opts)
		if err != nil {
			i.p.Log.Error("cannot start the network stack for the guest", "err", err)
			conn.Close()
			continue
		}
		i.mu.Lock()
		i.stack = stack
		i.mu.Unlock()
		if i.p.OnStack != nil {
			i.p.OnStack(stack)
		}
		go func() {
			<-stack.Done()
			i.mu.Lock()
			if i.stack == stack {
				i.stack = nil
			}
			i.mu.Unlock()
		}()
	}
}

func readLines(r io.Reader, emit func(string)) {
	br := bufio.NewReaderSize(r, maxLine)
	for {
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			emit(strings.ToValidUTF8(strings.TrimRight(string(line), "\r\n"), "�"))
		}
		if err != nil && err != bufio.ErrBufferFull {
			return
		}
	}
}

func (i *process) PID() int { return i.cmd.Process.Pid }

func (i *process) VsockPath(port uint32) string {
	return filepath.Join(i.p.Dir, fmt.Sprintf("vsock_%d.sock", port))
}

func (i *process) Stop() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.lifeline != nil {
		i.lifeline.Close()
		i.lifeline = nil
	}
}

func (i *process) Kill() { i.cmd.Process.Kill() }

func (i *process) Control() *vmmctl.Client { return i.control }

func rosettaArgs(words []string) []string {
	off := slices.Contains(words, agent.EmulationCmdlineParam+"=0")
	switch {
	case slices.Contains(words, agent.EmulationEngineParam+"="+agent.EmulationEngineRosetta) && !off:
		return []string{"--rosetta"}
	case slices.Contains(words, agent.EmulationEngineParam+"="+agent.EmulationEngineRosetta),
		slices.Contains(words, agent.EmulationEngineParam+"="+agent.EmulationEngineAuto):
		return []string{"--rosetta-optional"}
	}
	return nil
}
