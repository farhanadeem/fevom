package daemon

import (
	"errors"

	"golang.org/x/sys/unix"
)

const (
	vqLowDisk     = 0x0004
	vqVeryLowDisk = 0x0200
	vqNearLowDisk = 0x2000
	vqDesiredDisk = 0x4000
	vqSpace       = vqLowDisk | vqVeryLowDisk | vqNearLowDisk | vqDesiredDisk
)

type lowDiskWatch struct {
	kq   int
	done chan struct{}
}

func watchLowDisk(notify func()) (*lowDiskWatch, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(kq)
	changes := []unix.Kevent_t{
		{Ident: 1, Filter: unix.EVFILT_FS, Flags: unix.EV_ADD | unix.EV_CLEAR, Fflags: vqSpace},
		{Ident: 2, Filter: unix.EVFILT_USER, Flags: unix.EV_ADD | unix.EV_CLEAR},
	}
	if _, err := unix.Kevent(kq, changes, nil, nil); err != nil {
		unix.Close(kq)
		return nil, err
	}
	w := &lowDiskWatch{kq: kq, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		events := make([]unix.Kevent_t, 4)
		for {
			n, err := unix.Kevent(kq, nil, events, nil)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return
			}
			space := false
			for _, e := range events[:n] {
				if e.Filter == unix.EVFILT_USER {
					return
				}
				if e.Filter == unix.EVFILT_FS && e.Fflags&vqSpace != 0 {
					space = true
				}
			}
			if space {
				notify()
			}
		}
	}()
	return w, nil
}

func (w *lowDiskWatch) stop() {
	trigger := []unix.Kevent_t{{Ident: 2, Filter: unix.EVFILT_USER, Fflags: unix.NOTE_TRIGGER}}
	if _, err := unix.Kevent(w.kq, trigger, nil, nil); err == nil {
		<-w.done
	}
	unix.Close(w.kq)
}
