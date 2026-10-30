package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"fev/guest/internal/logbuf"
	"fev/guest/internal/reaper"
	"fev/guest/internal/supervise"
	"fev/guest/internal/webproxy/webctl"
	api "fev/proto/agent"
	"fev/proto/netplan"
)

const (
	webRun      = "/run/fev/webproxy"
	issueSocket = webRun + "/tls.sock"
	engineCA    = engineRun + "/fev/https/ca.pem"
)

type webFront struct {
	s *system

	mu      sync.Mutex
	sup     *supervise.Supervisor
	set     *api.DomainsParams
	held    *webctl.Held
	waiting bool
	ctrl    *os.File
	writeMu sync.Mutex
	issue   net.Listener
	child   *reaper.Child
}

func newWebFront(s *system) *webFront {
	w := &webFront{s: s}
	var mu sync.Mutex
	was := ""
	s.engine.sup.Subscribe(func(st api.EngineStatus) {
		mu.Lock()
		became := st.State == api.EngineReady && was != api.EngineReady
		was = st.State
		mu.Unlock()
		if became {
			go w.send(webctl.Control{NetNSRoot: w.engineRoot()})
		}
	})
	return w
}

func (w *webFront) domains(p api.DomainsParams) {
	w.mu.Lock()
	w.set = &p
	w.mu.Unlock()
	w.writeCA()
	if webProxyOn(w.s.cmdline) {
		return
	}
	w.mu.Lock()
	if w.sup == nil {
		w.sup = supervise.New(supervise.Hooks{
			Start: w.start, Stop: w.stop, NowMS: monotonicMS,
			After: func(d time.Duration, f func()) func() {
				t := time.AfterFunc(d, f)
				return func() { t.Stop() }
			},
			Logf: func(format string, args ...any) {
				w.s.log.Logf(logbuf.Warning, "web proxy: "+strings.TrimPrefix(format, "engine: "), args...)
			},
		})
		was := ""
		w.sup.Subscribe(func(st api.EngineStatus) {
			if st.State == api.EngineFailed && was != api.EngineFailed {
				w.gaveUp()
			}
			was = st.State
		})
	}
	w.mu.Unlock()
	w.arm()
	w.send(webctl.Control{Domains: &p})
}

func (w *webFront) arm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.held != nil {
		return
	}
	if st, err := os.Stat(webctl.Path); err != nil || !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
		w.s.log.Logf(logbuf.Error, "web proxy: no program at %s (the system image's); the web ports stay the targets' own", webctl.Path)
		return
	}
	h, err := webctl.Hold(netip.IPv4Unspecified(), netplan.WebHTTPPort, netplan.WebHTTPSPort)
	if err != nil {
		w.s.log.Logf(logbuf.Error, "web proxy: the web ports' listeners: %v", err)
		return
	}
	if w.s.ok6 {
		if err := h.Also(netip.IPv6Unspecified(), netplan.WebHTTPPort, netplan.WebHTTPSPort); err != nil {
			w.s.log.Logf(logbuf.Error, "web proxy: the web ports' IPv6 listeners: %v", err)
		}
	}
	w.held, w.waiting = h, true
	w.s.direct.setWeb(true)
	go w.activate(h)
}

func (w *webFront) activate(h *webctl.Held) {
	err := h.Wait()
	w.mu.Lock()
	w.waiting = false
	sup, current := w.sup, w.held == h
	w.mu.Unlock()
	if !current {
		return
	}
	if err != nil {
		w.s.log.Logf(logbuf.Error, "web proxy: waiting for the first connection: %v", err)
		w.disarm(h)
		return
	}
	w.s.log.Logf(logbuf.Info, "web proxy: the first connection; starting it")
	sup.Start()
}

func (w *webFront) gaveUp() {
	w.mu.Lock()
	h := w.held
	w.mu.Unlock()
	if h != nil {
		w.disarm(h)
	}
}

func (w *webFront) disarm(h *webctl.Held) {
	w.mu.Lock()
	if w.held != h || w.waiting {
		w.mu.Unlock()
		return
	}
	w.held = nil
	w.mu.Unlock()
	w.s.direct.setWeb(false)
	h.Close()
}

func (w *webFront) writeCA() {
	w.mu.Lock()
	ca := ""
	if w.set != nil && w.set.HTTPS {
		ca = w.set.CA
	}
	w.mu.Unlock()
	if _, err := os.Stat(engineRun); err != nil {
		return
	}
	if ca == "" {
		os.Remove(engineCA)
		return
	}
	if err := os.MkdirAll(filepath.Dir(engineCA), 0o755); err != nil {
		w.s.log.Logf(logbuf.Warning, "web proxy: fev's root for containers: %v", err)
		return
	}
	tmp := engineCA + ".new"
	if err := os.WriteFile(tmp, []byte(ca), 0o644); err == nil {
		err = os.Rename(tmp, engineCA)
		if err != nil {
			w.s.log.Logf(logbuf.Warning, "web proxy: fev's root for containers: %v", err)
		}
	}
}

func (w *webFront) engineStarted() { w.writeCA() }

func (w *webFront) engineRoot() string {
	e := w.s.engine
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cur == nil || e.cur.child == nil {
		return ""
	}
	return fmt.Sprintf("/proc/%d/root", e.cur.child.Pid)
}

func (w *webFront) machines(gone netip.Addr) {
	if gone.IsValid() {
		w.s.direct.machineGone(gone)
	}
	w.send(webctl.Control{Machines: w.s.net.NetNSPaths()})
}

func (w *webFront) send(c webctl.Control) {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	w.mu.Lock()
	ctrl := w.ctrl
	w.mu.Unlock()
	if ctrl == nil {
		return
	}
	w.writeLocked(ctrl, c)
}

func (w *webFront) writeLocked(ctrl *os.File, c webctl.Control) {
	if c.Domains != nil {
		c.Domains = engineNetNS(c.Domains)
	}
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	if _, err := ctrl.Write(append(b, '\n')); err != nil {
		w.s.log.Logf(logbuf.Debug, "web proxy: a control line: %v", err)
	}
}

func engineNetNS(p *api.DomainsParams) *api.DomainsParams {
	out := *p
	out.Targets = make([]api.DomainTarget, len(p.Targets))
	for i, t := range p.Targets {
		if rest, ok := strings.CutPrefix(t.NetNS, "/var/run/"); ok {
			t.NetNS = "/run/" + rest
		}
		out.Targets[i] = t
	}
	return &out
}

func (w *webFront) listenIssue() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.issue != nil {
		return nil
	}
	if err := os.MkdirAll(webRun, 0o700); err != nil {
		return err
	}
	os.Remove(issueSocket)
	l, err := net.Listen("unix", issueSocket)
	if err != nil {
		return err
	}
	if err := os.Chmod(issueSocket, 0o600); err != nil {
		l.Close()
		return err
	}
	w.issue = l
	go func() {
		err := w.s.requests.Serve(l, api.StreamTLSIssue)
		w.s.log.Logf(logbuf.Error, "web proxy: the tls.issue socket stopped: %v", err)
	}()
	return nil
}

func (w *webFront) start() (*supervise.Run, error) {
	if err := w.listenIssue(); err != nil {
		return nil, fmt.Errorf("the tls.issue socket: %w", err)
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	ctrl, theirs := os.NewFile(uintptr(fds[0]), "webproxy-control"), os.NewFile(uintptr(fds[1]), "webproxy-control")
	defer theirs.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		ctrl.Close()
		return nil, err
	}
	defer null.Close()
	w.mu.Lock()
	held := w.held
	w.mu.Unlock()
	if held == nil {
		ctrl.Close()
		return nil, errors.New("the web ports' listeners are closed")
	}
	logR, logW, err := os.Pipe()
	if err != nil {
		ctrl.Close()
		return nil, err
	}
	child, err := w.s.reaper.Start(webctl.Path, []string{webctl.StageName, issueSocket}, &os.ProcAttr{
		Dir:   "/",
		Env:   []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", webctl.ListenFDsEnv + "=" + held.FDs(4)},
		Files: append([]*os.File{null, logW, logW, theirs}, held.Files()...),
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
	logW.Close()
	if err != nil {
		ctrl.Close()
		logR.Close()
		return nil, err
	}
	go w.pump(logR)
	w.writeMu.Lock()
	w.mu.Lock()
	w.ctrl, w.child = ctrl, child
	set := w.set
	w.mu.Unlock()
	c := webctl.Control{NetNSRoot: w.engineRoot(), Machines: w.s.net.NetNSPaths(), Domains: set}
	if w.s.ok6 {
		c.Names6 = w.s.plan6.Names6().String()
	}
	if c.Machines == nil {
		c.Machines = map[string]string{}
	}
	w.writeLocked(ctrl, c)
	w.writeMu.Unlock()
	ready := make(chan error, 1)
	exited := make(chan struct{})
	var how string
	go w.reports(ctrl, ready)
	go func() {
		st := child.Wait()
		if st.Signaled() {
			how = fmt.Sprintf("signal %d", st.Signal())
		} else {
			how = fmt.Sprintf("exit %d", st.ExitStatus())
		}
		w.mu.Lock()
		if w.ctrl == ctrl {
			w.ctrl = nil
		}
		w.mu.Unlock()
		ctrl.Close()
		close(exited)
	}()
	return &supervise.Run{PID: child.Pid, Ready: ready, Exited: exited, How: func() string { return how }}, nil
}

func (w *webFront) reports(ctrl *os.File, ready chan<- error) {
	said := false
	sc := bufio.NewScanner(ctrl)
	for sc.Scan() {
		var r webctl.Report
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			w.s.log.Logf(logbuf.Warning, "web proxy: a report line: %v", err)
			continue
		}
		switch {
		case r.Ready && !said:
			said = true
			ready <- nil
		case r.Unredirect != "":
			if ap, err := netip.ParseAddrPort(r.Unredirect); err == nil {
				w.s.direct.unredirect(ap)
			}
		case r.Error != "":
			w.s.log.Logf(logbuf.Error, "web proxy: %s", r.Error)
		}
	}
}

func (w *webFront) stop(run *supervise.Run, grace time.Duration) {
	w.mu.Lock()
	child := w.child
	w.mu.Unlock()
	if child == nil || child.Pid != run.PID {
		<-run.Exited
		return
	}
	w.s.reaper.Signal(child, unix.SIGTERM, false)
	select {
	case <-run.Exited:
	case <-time.After(grace):
		w.s.reaper.Signal(child, unix.SIGKILL, false)
		<-run.Exited
	}
}

func (w *webFront) pump(r *os.File) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 16<<10), 16<<10)
	for sc.Scan() {
		w.s.log.Logf(logbuf.Info, "%s", sc.Text())
	}
}
