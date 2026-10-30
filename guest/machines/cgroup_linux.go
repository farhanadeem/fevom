package machines

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const sessionsGroup = ".fev"

func delegate(group string) error {
	available, err := os.ReadFile(filepath.Join(group, "cgroup.controllers"))
	if err != nil {
		return err
	}
	cs := strings.Fields(string(available))
	if len(cs) == 0 {
		return fmt.Errorf("%s has no controllers to delegate", group)
	}
	return os.WriteFile(filepath.Join(group, "cgroup.subtree_control"), []byte("+"+strings.Join(cs, " +")), 0)
}

func removeTree(group string) error {
	var dirs []string
	err := filepath.WalkDir(group, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := unix.Rmdir(dirs[i]); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("remove %s: %w", dirs[i], err)
		}
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

func waitEmpty(group string, limit time.Duration) bool {
	events, err := os.Open(filepath.Join(group, "cgroup.events"))
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

func killGroup(group string, limit time.Duration) bool {
	if err := os.WriteFile(filepath.Join(group, "cgroup.kill"), []byte("1"), 0); err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	return waitEmpty(group, limit)
}

func isPopulated(group string) bool {
	events, err := os.Open(filepath.Join(group, "cgroup.events"))
	if err != nil {
		return false
	}
	defer events.Close()
	full, _ := populated(events)
	return full
}

func sweepSessions(payload string) {
	entries, err := os.ReadDir(filepath.Join(payload, sessionsGroup))
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			unix.Rmdir(filepath.Join(payload, sessionsGroup, e.Name()))
		}
	}
}

const freezeWait = 5 * time.Second

func freeze(group string) (func(), error) {
	file := filepath.Join(group, "cgroup.freeze")
	if err := os.WriteFile(file, []byte("1"), 0); err != nil {
		return nil, err
	}
	thaw := func() { os.WriteFile(file, []byte("0"), 0) }
	events, err := os.Open(filepath.Join(group, "cgroup.events"))
	if err != nil {
		thaw()
		return nil, err
	}
	defer events.Close()
	deadline := time.Now().Add(freezeWait)
	for {
		if frozen(events) {
			return thaw, nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			thaw()
			return nil, fmt.Errorf("its processes were not all frozen within %v", freezeWait)
		}
		fds := []unix.PollFd{{Fd: int32(events.Fd()), Events: unix.POLLPRI}}
		if _, err := unix.Poll(fds, int(left.Milliseconds())+1); err != nil && !errors.Is(err, unix.EINTR) {
			thaw()
			return nil, err
		}
	}
}

func frozen(events *os.File) bool {
	buf := make([]byte, 256)
	n, _ := events.ReadAt(buf, 0)
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		if v, ok := strings.CutPrefix(line, "frozen "); ok {
			return v == "1"
		}
	}
	return false
}
