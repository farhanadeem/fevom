package files

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

type waiter struct {
	dirs []string

	mu     sync.Mutex
	kq     int
	closed bool
}

func isDir(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.IsDir()
}

func above(root, path string) string {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if dir != root && !strings.HasPrefix(dir, strings.TrimSuffix(root, "/")+"/") {
			return ""
		}
		if isDir(dir) {
			return dir
		}
		if dir == root || dir == "/" {
			return ""
		}
	}
}

func waitFor(root string, missing []string, changed func(w *waiter)) *waiter {
	seen := map[string]bool{}
	w := &waiter{}
	for _, path := range missing {
		if dir := above(root, path); dir != "" && !seen[dir] {
			seen[dir] = true
			w.dirs = append(w.dirs, dir)
		}
	}
	if len(w.dirs) == 0 {
		return nil
	}
	sort.Strings(w.dirs)
	kq, err := unix.Kqueue()
	if err != nil {
		return nil
	}
	unix.CloseOnExec(kq)
	w.kq = kq
	changes := []unix.Kevent_t{{Ident: 0, Filter: unix.EVFILT_USER, Flags: unix.EV_ADD | unix.EV_CLEAR}}
	var fds []int
	for _, dir := range w.dirs {
		fd, err := unix.Open(dir, unix.O_EVTONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		fds = append(fds, fd)
		changes = append(changes, unix.Kevent_t{
			Ident: uint64(fd), Filter: unix.EVFILT_VNODE, Flags: unix.EV_ADD | unix.EV_CLEAR,
			Fflags: unix.NOTE_WRITE | unix.NOTE_EXTEND | unix.NOTE_LINK | unix.NOTE_RENAME | unix.NOTE_DELETE,
		})
	}
	end := func() {
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
		for _, fd := range fds {
			unix.Close(fd)
		}
		unix.Close(kq)
	}
	if _, err := unix.Kevent(kq, changes, nil, nil); err != nil || len(fds) == 0 {
		end()
		return nil
	}
	already := false
	for _, path := range missing {
		already = already || isDir(path) || !seen[above(root, path)]
	}
	go func() {
		stopped := false
		if !already {
			events := make([]unix.Kevent_t, 4)
			for {
				n, err := unix.Kevent(kq, nil, events, nil)
				if err == unix.EINTR {
					continue
				}
				for i := 0; i < n; i++ {
					stopped = stopped || events[i].Filter == unix.EVFILT_USER
				}
				break
			}
		}
		end()
		if !stopped {
			changed(w)
		}
	}()
	return w
}

func (w *waiter) stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		unix.Kevent(w.kq, []unix.Kevent_t{{Ident: 0, Filter: unix.EVFILT_USER, Fflags: unix.NOTE_TRIGGER}}, nil, nil)
	}
}
