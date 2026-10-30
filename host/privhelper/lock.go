package privhelper

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	api "fev/proto/privhelper"
)

var LockPath = filepath.Join(filepath.Dir(api.RecordPath), "localnames.lock")

type namesLock struct {
	f     *os.File
	added time.Time
}

func (s *Server) lockNames() (*namesLock, error) {
	path := s.path(LockPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("the names' lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("the names' lock: %w", err)
	}
	fail := func(err error) (*namesLock, error) {
		f.Close()
		return nil, fmt.Errorf("the names' lock: %w", err)
	}
	if fi, err := f.Stat(); err != nil {
		return fail(err)
	} else if !fi.Mode().IsRegular() {
		return fail(fmt.Errorf("%s is not a regular file", path))
	} else if fi.Mode().Perm() != 0o600 {
		if err := f.Chmod(0o600); err != nil {
			return fail(err)
		}
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		return fail(err)
	}
	b, err := io.ReadAll(io.LimitReader(f, 128))
	if err != nil {
		return fail(err)
	}
	l := &namesLock{f: f}
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b))); err == nil {
		l.added = t
	}
	return l, nil
}

func (l *namesLock) stamp(now time.Time) error {
	line := now.UTC().Format(time.RFC3339Nano) + "\n"
	if err := l.f.Truncate(0); err != nil {
		return fmt.Errorf("the names' lock: %w", err)
	}
	if _, err := l.f.WriteAt([]byte(line), 0); err != nil {
		return fmt.Errorf("the names' lock: %w", err)
	}
	l.added = now
	return nil
}

func (l *namesLock) Close() { l.f.Close() }
