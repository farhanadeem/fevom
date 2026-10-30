package session

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/debug/attach"
)

type Entered interface {
	Context
	Entry(p api.ExecStartParams) (*Entry, error)
}

type Entry struct {
	Argv              []string
	Path              string
	Find              bool
	Env               []string
	Home              string
	Cred              *Cred
	MountNS, CgroupNS *os.File
	UserNS            *os.File
	Stage             string
	IDBase            uint32
	Isolate           bool
	Copy              string
	RuntimeDir        string
	Name              string
	Debug             *attach.DebugEntry
	Extra             []*os.File
	OnExit            func(*api.ExecExit)
	Started           func(pid int)
	Ptmx              string
	Release           func()
}

func (e *Entry) release() {
	if e.Release != nil {
		e.Release()
	}
}

type EntryPather interface {
	EntryPath() string
}

type Grouped interface {
	SessionGroups() string
}

var groupIDs atomic.Uint64

func (m *Manager) newGroup(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	g := filepath.Join(dir, strconv.FormatUint(groupIDs.Add(1), 10))
	if err := os.Mkdir(g, 0o755); err != nil {
		return "", err
	}
	return g, nil
}

const killWait = 5 * time.Second

func killGroup(group string, limit time.Duration) bool {
	if err := os.WriteFile(filepath.Join(group, "cgroup.kill"), []byte("1"), 0); err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	events, err := os.Open(filepath.Join(group, "cgroup.events"))
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	defer events.Close()
	deadline := time.Now().Add(limit)
	buf := make([]byte, 256)
	for {
		n, err := events.ReadAt(buf, 0)
		if err != nil && err != io.EOF {
			return false
		}
		if strings.Contains(string(buf[:n]), "populated 0") {
			return true
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

func mergeEnv(base, over []string) []string {
	out := append([]string{}, base...)
	index := map[string]int{}
	for i, e := range out {
		if k, _, ok := strings.Cut(e, "="); ok {
			index[k] = i
		}
	}
	for _, e := range over {
		k, _, _ := strings.Cut(e, "=")
		if i, ok := index[k]; ok {
			out[i] = e
			continue
		}
		index[k] = len(out)
		out = append(out, e)
	}
	return out
}

type Groups struct{ Dir string }

func (Groups) In(fn func(cgroup int) error) error { return fn(-1) }
func (g Groups) SessionGroups() string            { return g.Dir }
