package daemon

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openPTY() (master, tty *os.File, err error) {
	m, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	fail := func(what string, err error) (*os.File, *os.File, error) {
		unix.Close(m)
		return nil, nil, fmt.Errorf("%s: %w", what, err)
	}
	if err := unix.IoctlSetInt(m, unix.TIOCPTYGRANT, 0); err != nil {
		return fail("grantpt", err)
	}
	if err := unix.IoctlSetInt(m, unix.TIOCPTYUNLK, 0); err != nil {
		return fail("unlockpt", err)
	}
	var name [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(m), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		return fail("ptsname", errno)
	}
	path := string(name[:bytes.IndexByte(name[:], 0)])
	s, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail("open "+path, err)
	}
	return os.NewFile(uintptr(m), "/dev/ptmx"), os.NewFile(uintptr(s), path), nil
}

func setSize(f *os.File, rows, cols int) error {
	if rows <= 0 || cols <= 0 {
		return nil
	}
	return unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)})
}
