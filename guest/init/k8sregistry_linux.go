package main

import (
	"bufio"
	"errors"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"fev/guest/internal/logbuf"
	"fev/guest/internal/reaper"
)

type k8sRegistry struct {
	s *system

	mu    sync.Mutex
	l     *net.TCPListener
	file  *os.File
	child *reaper.Child
	stop  *os.File
	gen   int
}

func (r *k8sRegistry) arm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.l != nil {
		return
	}
	if st, err := os.Stat(registryProgram); err != nil || st.Mode()&0o111 == 0 {
		r.s.log.Logf(logbuf.Error, "k8s: no %s in the system image; pods find no image of Docker's", registryProgram)
		return
	}
	addr, _ := net.ResolveTCPAddr("tcp", registryAddr)
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		r.s.log.Logf(logbuf.Error, "k8s: fev's registry on %s: %v", registryAddr, err)
		return
	}
	f, err := l.File()
	if err != nil {
		l.Close()
		r.s.log.Logf(logbuf.Error, "k8s: fev's registry's listener: %v", err)
		return
	}
	stopR, stopW, err := os.Pipe()
	if err != nil {
		f.Close()
		l.Close()
		return
	}
	r.l, r.file, r.stop = l, f, stopW
	r.gen++
	go r.await(r.gen, f, stopR)
}

func (r *k8sRegistry) await(gen int, f, stop *os.File) {
	defer stop.Close()
	for {
		fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}, {Fd: int32(stop.Fd()), Events: unix.POLLIN}}
		_, err := unix.Poll(fds, -1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		r.mu.Lock()
		current := r.gen == gen && r.l != nil
		r.mu.Unlock()
		if !current {
			return
		}
		if err != nil || fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			r.s.log.Logf(logbuf.Error, "k8s: fev's registry's listener: %v", err)
			return
		}
		began := time.Now()
		child, err := r.start(f)
		if err != nil {
			r.s.log.Logf(logbuf.Error, "k8s: start fev's registry: %v", err)
			return
		}
		r.s.log.Logf(logbuf.Info, "k8s: fev's registry started at its first connection (pid %d)", child.Pid)
		st := child.Wait()
		r.mu.Lock()
		if r.child == child {
			r.child = nil
		}
		current = r.gen == gen && r.l != nil
		r.mu.Unlock()
		if !current {
			return
		}
		r.s.log.Logf(logbuf.Warning, "k8s: fev's registry ended (%v) after %v; started again at the next connection", st, time.Since(began).Round(time.Millisecond))
		if time.Since(began) < time.Second {
			time.Sleep(time.Second)
		}
	}
}

func (r *k8sRegistry) start(f *os.File) (*reaper.Child, error) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer null.Close()
	logR, logW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	child, err := r.s.reaper.Start(registryProgram, []string{"fev-registry", "serve",
		"-containerd", engineRun + "/docker/containerd/containerd.sock",
		"-content", engineData + "/containerd/daemon/io.containerd.content.v1.content",
		"-procs", engineCgroup + "/containerd/cgroup.procs",
	}, &os.ProcAttr{
		Dir:   "/",
		Env:   []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"},
		Files: []*os.File{null, logW, logW, f},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
	logW.Close()
	if err != nil {
		logR.Close()
		return nil, err
	}
	go func() {
		defer logR.Close()
		sc := bufio.NewScanner(logR)
		sc.Buffer(make([]byte, 16<<10), 16<<10)
		for sc.Scan() {
			r.s.log.Logf(logbuf.Info, "%s", sc.Text())
		}
	}()
	r.mu.Lock()
	r.child = child
	r.mu.Unlock()
	return child, nil
}

func (r *k8sRegistry) disarm() {
	r.mu.Lock()
	l, f, child, stop := r.l, r.file, r.child, r.stop
	r.l, r.file, r.child, r.stop = nil, nil, nil, nil
	r.gen++
	r.mu.Unlock()
	if child != nil {
		r.s.reaper.Signal(child, unix.SIGTERM, false)
	}
	if l != nil {
		stop.Write([]byte{1})
		stop.Close()
		l.Close()
		f.Close()
	}
}
