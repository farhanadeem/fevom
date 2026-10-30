package home

import (
	"os"
	"syscall"
)

func Lock(path string, wait bool) (f *os.File, held bool, err error) {
	f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	for {
		err = syscall.Flock(int(f.Fd()), how)
		if err != syscall.EINTR {
			break
		}
	}
	if err == syscall.EWOULDBLOCK && !wait {
		f.Close()
		return nil, true, nil
	}
	if err != nil {
		f.Close()
		return nil, false, err
	}
	return f, false, nil
}
