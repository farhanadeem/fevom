package nfsview

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

type Keeper struct {
	dir string
	log *slog.Logger
	kq  int

	mu      sync.Mutex
	held    map[string]*os.File
	opening map[string]bool
	closed  bool
	scans   int
	done    chan struct{}
	opens   sync.WaitGroup
}

var (
	mountTable = func() ([]unix.Statfs_t, error) {
		for {
			n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
			if err != nil {
				return nil, err
			}
			buf := make([]unix.Statfs_t, n+4)
			got, err := unix.Getfsstat(buf, unix.MNT_NOWAIT)
			if err != nil {
				return nil, err
			}
			if got < len(buf) {
				return buf[:got], nil
			}
		}
	}
	holdMount = func(path string) (*os.File, error) {
		return os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	}
)

const wakeIdent = 1

func Keep(dir string, log *slog.Logger) (*Keeper, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(filepath.Clean(dir)))
	if err != nil {
		return nil, err
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(kq)
	changes := []unix.Kevent_t{
		{Ident: 0, Filter: unix.EVFILT_FS, Flags: unix.EV_ADD | unix.EV_CLEAR},
		{Ident: wakeIdent, Filter: unix.EVFILT_USER, Flags: unix.EV_ADD | unix.EV_CLEAR},
	}
	if _, err := unix.Kevent(kq, changes, nil, nil); err != nil {
		unix.Close(kq)
		return nil, err
	}
	k := &Keeper{
		dir:     filepath.Join(parent, filepath.Base(filepath.Clean(dir))),
		log:     log,
		kq:      kq,
		held:    map[string]*os.File{},
		opening: map[string]bool{},
		done:    make(chan struct{}),
	}
	k.scan()
	go k.wait()
	return k, nil
}

func (k *Keeper) wait() {
	defer close(k.done)
	events := make([]unix.Kevent_t, 8)
	for {
		n, err := unix.Kevent(k.kq, nil, events, nil)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			k.log.Warn("view: the mount events ended", "err", err)
			return
		}
		rescan := false
		for _, ev := range events[:n] {
			switch ev.Filter {
			case unix.EVFILT_USER:
				return
			case unix.EVFILT_FS:
				if ev.Fflags&(0x8|0x10) != 0 {
					rescan = true
				}
			}
		}
		if rescan {
			k.scan()
		}
	}
}

func (k *Keeper) mirror(s *unix.Statfs_t) (string, bool) { return isMirror(s, k.dir) }

func isMirror(s *unix.Statfs_t, view string) (string, bool) {
	on := unix.ByteSliceToString(s.Mntonname[:])
	return on, unix.ByteSliceToString(s.Fstypename[:]) == "nfs" &&
		s.Flags&unix.MNT_AUTOMOUNTED != 0 &&
		s.Owner == uint32(os.Getuid()) &&
		strings.HasPrefix(on, view+"/")
}

func (k *Keeper) scan() {
	table, err := mountTable()
	k.mu.Lock()
	defer k.mu.Unlock()
	k.scans++
	if k.closed {
		return
	}
	if err != nil {
		k.log.Warn("view: the mount table", "err", err)
		return
	}
	now := map[string]bool{}
	for i := range table {
		on, ok := k.mirror(&table[i])
		if !ok {
			continue
		}
		now[on] = true
		if _, held := k.held[on]; held || k.opening[on] {
			continue
		}
		k.opening[on] = true
		k.opens.Add(1)
		go k.hold(on)
	}
	for on, f := range k.held {
		if !now[on] {
			f.Close()
			delete(k.held, on)
			k.log.Debug("view: a held mirror mount went", "path", on)
		}
	}
}

func (k *Keeper) hold(on string) {
	defer k.opens.Done()
	f, err := holdMount(on)
	k.mu.Lock()
	defer k.mu.Unlock()
	wanted := k.opening[on]
	delete(k.opening, on)
	if err != nil {
		k.log.Debug("view: a mirror mount not held", "path", on, "err", err)
		return
	}
	if k.closed || !wanted {
		f.Close()
		return
	}
	k.held[on] = f
	k.log.Debug("view: holding a mirror mount", "path", on)
}

func (k *Keeper) settle() { k.opens.Wait() }

func (k *Keeper) Held() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []string
	for on := range k.held {
		out = append(out, on)
	}
	return out
}

func (k *Keeper) Forget(path string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	was := k.opening[path]
	delete(k.opening, path)
	if f, ok := k.held[path]; ok {
		f.Close()
		delete(k.held, path)
		was = true
	}
	return was
}

func (k *Keeper) ForgetUnder(rel string) (path string, was bool) {
	path = filepath.Join(k.dir, filepath.Clean("/"+rel))
	return path, k.Forget(path)
}

func (k *Keeper) Close() {
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return
	}
	k.closed = true
	for on, f := range k.held {
		f.Close()
		delete(k.held, on)
	}
	clear(k.opening)
	k.mu.Unlock()
	unix.Kevent(k.kq, []unix.Kevent_t{{Ident: wakeIdent, Filter: unix.EVFILT_USER, Fflags: unix.NOTE_TRIGGER}}, nil, nil)
	<-k.done
	unix.Close(k.kq)
}
