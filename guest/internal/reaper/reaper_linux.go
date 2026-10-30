package reaper

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

type Reaper struct {
	mu      sync.Mutex
	waiting map[int]chan unix.WaitStatus
}

func New() *Reaper {
	return &Reaper{waiting: map[int]chan unix.WaitStatus{}}
}

type Child struct {
	Pid  int
	done chan unix.WaitStatus
}

func (c *Child) Wait() unix.WaitStatus { return <-c.done }

func (r *Reaper) Start(path string, argv []string, attr *os.ProcAttr) (*Child, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := os.StartProcess(path, argv, attr)
	if err != nil {
		return nil, err
	}
	c := &Child{Pid: p.Pid, done: make(chan unix.WaitStatus, 1)}
	r.waiting[p.Pid] = c.done
	p.Release()
	return c, nil
}

func (r *Reaper) Signal(c *Child, sig unix.Signal, group bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, running := r.waiting[c.Pid]; !running {
		return false
	}
	pid := c.Pid
	if group {
		pid = -pid
	}
	return unix.Kill(pid, sig) == nil
}

func (r *Reaper) Reap() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if pid <= 0 || err != nil {
			return n
		}
		n++
		if done, ok := r.waiting[pid]; ok {
			delete(r.waiting, pid)
			done <- status
		}
	}
}

func (r *Reaper) Run(limit time.Duration, path string, args ...string) ([]byte, error) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer null.Close()
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer pr.Close()
	child, err := r.Start(path, append([]string{path}, args...), &os.ProcAttr{
		Files: []*os.File{null, pw, pw},
		Env:   []string{"PATH=/system/bin"},
	})
	pw.Close()
	if err != nil {
		return nil, err
	}
	var killed atomic.Bool
	timer := time.AfterFunc(limit, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, running := r.waiting[child.Pid]; running {
			killed.Store(true)
			unix.Kill(child.Pid, unix.SIGKILL)
		}
	})
	var out bytes.Buffer
	out.ReadFrom(pr)
	status := child.Wait()
	timer.Stop()
	switch {
	case killed.Load():
		return out.Bytes(), fmt.Errorf("%s: killed after %v", path, limit)
	case status.Signaled():
		return out.Bytes(), fmt.Errorf("%s: killed by signal %d", path, status.Signal())
	case status.ExitStatus() != 0:
		return out.Bytes(), &ExitError{Path: path, Status: status.ExitStatus()}
	}
	return out.Bytes(), nil
}

type ExitError struct {
	Path   string
	Status int
}

func (e *ExitError) Error() string { return fmt.Sprintf("%s: exit status %d", e.Path, e.Status) }

func (e *ExitError) ExitStatus() int { return e.Status }
