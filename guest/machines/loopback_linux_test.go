package machines

import (
	"errors"
	"os"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func dialLoopback(family int, sa unix.Sockaddr) error {
	l, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(l)
	c, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(c)
	if err := unix.Bind(l, sa); err != nil {
		if errors.Is(err, unix.EADDRNOTAVAIL) {
			return unix.Connect(c, sa)
		}
		return err
	}
	if err := unix.Listen(l, 1); err != nil {
		return err
	}
	got, err := unix.Getsockname(l)
	if err != nil {
		return err
	}
	return unix.Connect(c, got)
}

func TestAMachinesLoopbackIsUpBeforeItsInitRuns(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for a network namespace (tests/run unit guest runs this in a privileged container)")
	}
	done := make(chan error, 1)
	var before4, before6, after4, after6 error
	go func() {
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			done <- err
			return
		}
		v4 := &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}
		v6 := &unix.SockaddrInet6{Addr: [16]byte{15: 1}}
		before4, before6 = dialLoopback(unix.AF_INET, v4), dialLoopback(unix.AF_INET6, v6)
		if err := loopbackUp(); err != nil {
			done <- err
			return
		}
		if err := loopbackUp(); err != nil {
			done <- err
			return
		}
		after4, after6 = dialLoopback(unix.AF_INET, v4), dialLoopback(unix.AF_INET6, v6)
		done <- nil
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(before4, unix.ENETUNREACH) {
		t.Errorf("127.0.0.1 in a new namespace: %v; want network is unreachable (lo down)", before4)
	}
	if before6 == nil {
		t.Error("::1 in a new namespace was reached with lo down")
	}
	if after4 != nil {
		t.Errorf("127.0.0.1 after loopbackUp: %v", after4)
	}
	if _, err := os.Stat("/proc/sys/net/ipv6"); err == nil && after6 != nil {
		t.Errorf("::1 after loopbackUp: %v", after6)
	}
}
