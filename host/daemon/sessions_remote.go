package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	api "fev/proto/daemon"
	"fev/proto/relay"
)

type remoteRun struct {
	appEnd    *net.UnixConn
	resize    chan [2]int
	cancel    context.CancelFunc
	closeOnce sync.Once
	appOnce   sync.Once
}

func (r *remoteRun) setSize(rows, cols int) {
	select {
	case r.resize <- [2]int{rows, cols}:
	default:
	}
}

func (r *remoteRun) close() {
	r.closeOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
	})
	r.closeApp()
}

func (r *remoteRun) closeApp() {
	r.appOnce.Do(func() {
		if r.appEnd != nil {
			r.appEnd.Close()
		}
	})
}

func socketPair() (local, peer *net.UnixConn, err error) {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	mk := func(fd int, name string) (*net.UnixConn, error) {
		f := os.NewFile(uintptr(fd), name)
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		return c.(*net.UnixConn), nil
	}
	local, err = mk(fds[0], "debug-remote-daemon")
	if err != nil {
		syscall.Close(fds[1])
		return nil, nil, err
	}
	peer, err = mk(fds[1], "debug-remote-app")
	if err != nil {
		local.Close()
		return nil, nil, err
	}
	return local, peer, nil
}

func newSessionToken() string {
	var b [relay.TokenLen / 2]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (t *sessionTable) armRemote(s *session, token string) {
	s.mu.Lock()
	s.token = token
	s.timer = time.AfterFunc(attachBound, func() {
		s.mu.Lock()
		attached := s.attached
		s.mu.Unlock()
		if !attached {
			t.kill(s)
			s.exit(api.SessionExit{ExitCode: -1, Error: "the session's stream was not attached within 10s"})
		}
	})
	s.mu.Unlock()
	t.mu.Lock()
	t.byToken[token] = s
	t.mu.Unlock()
	s.caller.OnGone(func() {
		if s.finish() {
			t.kill(s)
		}
	})
}
