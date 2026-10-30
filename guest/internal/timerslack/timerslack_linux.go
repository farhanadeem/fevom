package timerslack

import (
	"runtime"

	"golang.org/x/sys/unix"
)

const (
	Cluster = 2_000_000
	Kernel  = 50_000
)

func Set(ns int) error { return unix.Prctl(unix.PR_SET_TIMERSLACK, uintptr(ns), 0, 0, 0) }

func Get() (int, error) { return unix.PrctlRetInt(unix.PR_GET_TIMERSLACK, 0, 0, 0, 0) }

func StartWith(ns int, start func() (int, error)) (int, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	was, err := Get()
	if err == nil && was != ns {
		if Set(ns) == nil {
			defer Set(was)
		}
	}
	return start()
}
