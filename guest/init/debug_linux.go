package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

	"fev/guest/debug/attach"
	"fev/guest/internal/logbuf"
	"fev/guest/internal/reclaim"
	"fev/guest/internal/session"
	"fev/guest/nfsview"
)

const (
	debugStore       = "/data/@debug"
	debugDenyCgroups = cgroupRoot + "/fev.init/debug"
	debugCgroup      = cgroupRoot + "/fev.debug"
	debugStage       = "/system/bin/fev-dbgstage"
	debugLock        = "/system/etc/fev-debug/defaultset.lock"
	debugStampFile   = "/system/etc/fev-debug/stamp"
	debugRun         = "/run/fev/debug"
	debugSocket      = debugRun + "/store.sock"
	debugSessions    = debugRun + "/sessions"
	debugPins        = debugRun + "/pins"
)

const debugBin = "/system/bin/fev-debug"

var debugStamp = sync.OnceValue(func() string {
	b, err := os.ReadFile(debugStampFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
})

func debugReady() bool {
	st := debugStamp()
	if st == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(debugStore, "profiles", "default", "stamp"))
	if err != nil || strings.TrimSpace(string(b)) != st {
		return false
	}
	_, err = os.Lstat(filepath.Join(debugStore, "profiles", "default", "bin", "zsh"))
	return err == nil
}

var debugSweep sync.Once

func (s *system) debugPrepare(cacheURL, indexURL string) error {
	if _, err := os.Stat(debugStore); err == nil {
		debugSweep.Do(func() {
			ents, _ := os.ReadDir(filepath.Join(debugStore, "tmp"))
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), ".dbg-") {
					os.RemoveAll(filepath.Join(debugStore, "tmp", e.Name()))
				}
			}
		})
	}
	if debugReady() {
		return nil
	}
	vol, err := s.volume()
	if err != nil {
		return err
	}
	if _, _, err := vol.EnsureDebugSubvolume(); err != nil {
		return err
	}
	s.log.Logf(logbuf.Info, "debug: making the tool store whole for this release (fev-debug fetch)")
	args := []string{"fetch", "-q", "-store", debugStore, "-lock", debugLock}
	if cacheURL != "" {
		args = append(args, "-cache", cacheURL)
	}
	_, stderr, err := s.debugRun(10*time.Minute, args...)
	if err != nil {
		return fmt.Errorf("fev-debug fetch: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	if !debugReady() {
		return errors.New("fev-debug fetch ended, yet the store is not whole")
	}
	return nil
}

func debugGroup() (*os.File, error) {
	if err := os.MkdirAll(debugCgroup, 0o755); err != nil {
		return nil, err
	}
	return os.Open(debugCgroup)
}

func debugTrim() {
	b, err := os.ReadFile(filepath.Join(debugCgroup, "cgroup.events"))
	if err != nil || strings.Contains(string(b), "populated 1") {
		return
	}
	cur, err := os.ReadFile(filepath.Join(debugCgroup, "memory.current"))
	if err != nil {
		return
	}
	if n, err := strconv.ParseUint(strings.TrimSpace(string(cur)), 10, 64); err == nil && n > 0 {
		reclaim.Trim(debugCgroup, n)
	}
}

func (s *system) debugRun(limit time.Duration, args ...string) (stdout, stderr []byte, err error) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, nil, err
	}
	defer null.Close()
	or, ow, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	defer or.Close()
	er, ew, err := os.Pipe()
	if err != nil {
		ow.Close()
		return nil, nil, err
	}
	defer er.Close()
	sys := &syscall.SysProcAttr{}
	if g, gerr := debugGroup(); gerr == nil {
		defer g.Close()
		sys.UseCgroupFD, sys.CgroupFD = true, int(g.Fd())
	}
	child, err := s.reaper.Start(debugBin, append([]string{debugBin}, args...), &os.ProcAttr{
		Files: []*os.File{null, ow, ew},
		Env:   []string{"PATH=/system/bin"},
		Sys:   sys,
	})
	ow.Close()
	ew.Close()
	if err != nil {
		return nil, nil, err
	}
	var killed atomic.Bool
	timer := time.AfterFunc(limit, func() {
		if s.reaper.Signal(child, unix.SIGKILL, false) {
			killed.Store(true)
		}
	})
	var out, errb bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); errb.ReadFrom(er) }()
	out.ReadFrom(or)
	wg.Wait()
	status := child.Wait()
	timer.Stop()
	debugTrim()
	switch {
	case killed.Load():
		return out.Bytes(), errb.Bytes(), fmt.Errorf("killed after %v", limit)
	case status.Signaled():
		return out.Bytes(), errb.Bytes(), fmt.Errorf("killed by signal %d", status.Signal())
	case status.ExitStatus() != 0:
		return out.Bytes(), errb.Bytes(), fmt.Errorf("exit status %d", status.ExitStatus())
	}
	return out.Bytes(), errb.Bytes(), nil
}

type debugServer struct {
	s *system

	mu           sync.Mutex
	ln           *net.UnixListener
	file         *os.File
	cache, index string
}

func (d *debugServer) socket(cache, index string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cache, d.index = cache, index
	if d.ln != nil {
		return debugSocket, nil
	}
	for _, dir := range []string{debugRun, debugSessions, debugPins} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
	}
	os.Remove(debugSocket)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: debugSocket, Net: "unix"})
	if err != nil {
		return "", err
	}
	ln.SetUnlinkOnClose(false)
	os.Chmod(debugSocket, 0o666)
	f, err := ln.File()
	if err != nil {
		ln.Close()
		return "", err
	}
	d.ln, d.file = ln, f
	go d.activate()
	return debugSocket, nil
}

func (d *debugServer) activate() {
	fd := int32(d.file.Fd())
	for {
		pfd := []unix.PollFd{{Fd: fd, Events: unix.POLLIN}}
		if _, err := unix.Poll(pfd, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			d.s.log.Logf(logbuf.Error, "debug: the store's listener: %v", err)
			return
		}
		d.mu.Lock()
		args := []string{debugBin, "serve", "-store", debugStore, "-listen-fd", "3",
			"-sessions", debugSessions, "-pins", debugPins}
		if d.cache != "" {
			args = append(args, "-cache", d.cache)
		}
		if d.index != "" {
			args = append(args, "-index-url", d.index)
		}
		d.mu.Unlock()
		null, _ := os.Open(os.DevNull)
		sys := &syscall.SysProcAttr{}
		g, gerr := debugGroup()
		if gerr == nil {
			sys.UseCgroupFD, sys.CgroupFD = true, int(g.Fd())
		}
		t0 := time.Now()
		child, err := d.s.reaper.Start(debugBin, args, &os.ProcAttr{
			Files: []*os.File{null, null, null, d.file},
			Env:   []string{"PATH=/system/bin"},
			Sys:   sys,
		})
		null.Close()
		if g != nil {
			g.Close()
		}
		if err != nil {
			d.s.log.Logf(logbuf.Error, "debug: starting the store's server: %v", err)
			time.Sleep(time.Second)
			continue
		}
		st := child.Wait()
		debugTrim()
		if st.ExitStatus() != 0 || st.Signaled() {
			d.s.log.Logf(logbuf.Warning, "debug: the store's server ended: %v", st)
			if time.Since(t0) < time.Second {
				time.Sleep(time.Second)
			}
		}
	}
}

const debugImageLeasePrefix = "fev-debug-"

func (s *system) newDebug() *session.Debug {
	logf := func(format string, args ...any) { s.log.Logf(logbuf.Debug, "debug: "+format, args...) }
	s.debugSrv = &debugServer{s: s}
	ctr := &nfsview.Containerd{Dial: dialUnix(engineContainerd), Namespace: "moby", Snapshotter: "overlayfs"}
	iv := &debugImageViews{ctr: ctr, active: map[string]bool{}}
	return &session.Debug{
		Arch:             attach.ArchARM64,
		Kernel:           guestKernelVersion(),
		SyscallNR:        attach.ARM64SyscallNR,
		Dial:             dialUnix(engineSocket),
		EngineProc:       s.engineProc,
		ContainerMounts:  func(id string) (attach.GraphDriver, error) { return containerGraph(ctr, id) },
		ImageView:        iv.open,
		StoreDir:         debugStore,
		DataRoot:         engineData,
		Stage:            debugStage,
		Galley:           debugBin,
		DenyCgroupParent: debugDenyCgroups,
		PrepareStore:     s.debugPrepare,
		StoreReady:       debugReady,
		Serve:            s.debugSrv.socket,
		SessionsDir:      debugSessions,
		DumpLock:         debugRun + "/dump.lock",
		PinsDir:          debugPins,
		Logf:             logf,
		DefaultSeccomp:   attach.DefaultSeccomp(attach.ArchARM64),
	}
}

func (s *system) DebugStatus() (api.DebugStatus, error) {
	exists := false
	if fi, err := os.Stat(debugStore); err == nil && fi.IsDir() {
		if _, err := os.Stat(filepath.Join(debugStore, "store")); err == nil {
			exists = true
		}
	}
	if !exists {
		return api.DebugStatus{Exists: false}, nil
	}
	out, stderr, err := s.debugRun(20*time.Second, "status", "-store", debugStore,
		"-exists", "true", "-default-set", debugLock)
	if err != nil {
		return api.DebugStatus{}, fmt.Errorf("fev-debug status: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	var st api.DebugStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return api.DebugStatus{}, fmt.Errorf("fev-debug status: %w", err)
	}
	st.Exists = true
	return st, nil
}

func (s *system) DebugReset(force bool) (api.DebugReset, error) {
	var freed int64
	if _, err := os.Stat(debugStore); err == nil {
		out, _, rerr := s.debugRun(30*time.Second, "reset", "-store", debugStore)
		if rerr == nil {
			var r api.DebugReset
			if json.Unmarshal(out, &r) == nil {
				freed = r.FreedBytes
			}
		}
	}
	if vol, err := s.volume(); err == nil {
		if rerr := vol.RemoveDebugSubvolume(); rerr != nil {
			s.log.Logf(logbuf.Debug, "debug: removing @debug: %v", rerr)
		}
	}
	return api.DebugReset{FreedBytes: freed}, nil
}

func containerGraph(ctr *nfsview.Containerd, id string) (attach.GraphDriver, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ms, err := ctr.Mounts(ctx, id)
	if err != nil {
		return attach.GraphDriver{}, err
	}
	if len(ms) != 1 {
		return attach.GraphDriver{}, fmt.Errorf("the snapshot has %d mounts", len(ms))
	}
	return attach.GraphFromMount(ms[0].Type, ms[0].Source, ms[0].Options)
}

type debugImageViews struct {
	ctr    *nfsview.Containerd
	mu     sync.Mutex
	active map[string]bool
}

func (v *debugImageViews) open(diffIDs []string, session string) ([]string, func(), error) {
	chain := nfsview.ChainID(diffIDs)
	if chain == "" {
		return nil, nil, fmt.Errorf("the image has no layers")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	v.sweep(ctx, session)
	if err := v.ctr.CreateLease(ctx, session); err != nil {
		return nil, nil, err
	}
	ms, err := v.ctr.View(ctx, session, session, chain)
	if err != nil {
		v.ctr.DeleteLease(context.Background(), session)
		return nil, nil, err
	}
	lowers, err := nfsview.ViewLayers(ms, engineDataName, engineData)
	if err != nil {
		v.ctr.Remove(context.Background(), session)
		v.ctr.DeleteLease(context.Background(), session)
		return nil, nil, err
	}
	v.mu.Lock()
	v.active[session] = true
	v.mu.Unlock()
	release := func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		v.ctr.Remove(c, session)
		v.ctr.DeleteLease(c, session)
		v.mu.Lock()
		delete(v.active, session)
		v.mu.Unlock()
	}
	return lowers, release, nil
}

func (v *debugImageViews) sweep(ctx context.Context, keep string) {
	ls, err := v.ctr.Leases(ctx)
	if err != nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, l := range ls {
		if l == keep || !strings.HasPrefix(l, debugImageLeasePrefix) || v.active[l] {
			continue
		}
		v.ctr.Remove(ctx, l)
		v.ctr.DeleteLease(ctx, l)
	}
}

func (s *system) engineProc(fn func() error) (bool, error) {
	path, done, ok := s.engineMountNS()
	if !ok {
		return false, nil
	}
	defer done()
	type result struct{ err error }
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			ch <- result{err}
			return
		}
		f, err := os.Open(path)
		if err != nil {
			ch <- result{err}
			return
		}
		defer f.Close()
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNS); err != nil {
			ch <- result{err}
			return
		}
		ch <- result{fn()}
	}()
	r := <-ch
	return true, r.err
}

func guestKernelVersion() attach.KernelVersion {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return attach.KernelVersion{}
	}
	rel := unix.ByteSliceToString(u.Release[:])
	k, rest, ok := leadingUint(rel)
	if !ok {
		return attach.KernelVersion{}
	}
	m := uint64(0)
	if r, ok := strings.CutPrefix(rest, "."); ok {
		m, _, _ = leadingUint(r)
	}
	return attach.KernelVersion{Kernel: k, Major: m}
}

func leadingUint(s string) (uint64, string, bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, s, false
	}
	v, err := strconv.ParseUint(s[:i], 10, 64)
	if err != nil {
		return 0, s, false
	}
	return v, s[i:], true
}
