package nfsview

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type fdConn struct {
	fd     int
	local  net.Addr
	remote net.Addr
	closed atomic.Bool
	once   sync.Once
}

var errClosed = errors.New("the view's connection is closed")

func newFDConn(c net.Conn) *fdConn {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return nil
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return nil
	}
	fd := -1
	var derr error
	if err := rc.Control(func(s uintptr) {
		fd, derr = dupCloexec(int(s))
	}); err != nil || derr != nil {
		return nil
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		syscall.Close(fd)
		return nil
	}
	f := &fdConn{fd: fd, local: c.LocalAddr(), remote: c.RemoteAddr()}
	c.Close()
	return f
}

func dupCloexec(fd int) (int, error) {
	r, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_DUPFD_CLOEXEC, 0)
	if e != 0 {
		return -1, e
	}
	return int(r), nil
}

func (f *fdConn) Read(b []byte) (int, error) {
	if f.closed.Load() {
		return 0, errClosed
	}
	for {
		n, err := syscall.Read(f.fd, b)
		switch {
		case err == syscall.EINTR:
			continue
		case err != nil:
			if f.closed.Load() {
				return 0, errClosed
			}
			return 0, err
		case n == 0 && len(b) > 0:
			if f.closed.Load() {
				return 0, errClosed
			}
			return 0, io.EOF
		}
		return n, nil
	}
}

func (f *fdConn) Write(b []byte) (int, error) {
	done := 0
	for done < len(b) {
		if f.closed.Load() {
			return done, errClosed
		}
		n, err := syscall.Write(f.fd, b[done:])
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return done, err
		}
		done += n
	}
	return done, nil
}

func (f *fdConn) CloseWrite() error { return syscall.Shutdown(f.fd, syscall.SHUT_WR) }

func (f *fdConn) Close() error {
	if f.closed.Swap(true) {
		return nil
	}
	syscall.Shutdown(f.fd, syscall.SHUT_RDWR)
	return nil
}

func (f *fdConn) release() {
	f.once.Do(func() {
		f.Close()
		syscall.Close(f.fd)
	})
}

func (f *fdConn) LocalAddr() net.Addr              { return f.local }
func (f *fdConn) RemoteAddr() net.Addr             { return f.remote }
func (f *fdConn) SetDeadline(time.Time) error      { return errors.ErrUnsupported }
func (f *fdConn) SetReadDeadline(time.Time) error  { return errors.ErrUnsupported }
func (f *fdConn) SetWriteDeadline(time.Time) error { return errors.ErrUnsupported }
