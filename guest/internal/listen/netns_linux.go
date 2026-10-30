package listen

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

func netnsCookie(nsfd int) (uint64, error) {
	runtime.LockOSThread()
	own, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		runtime.UnlockOSThread()
		return 0, err
	}
	defer unix.Close(own)
	if err := unix.Setns(nsfd, unix.CLONE_NEWNET); err != nil {
		runtime.UnlockOSThread()
		return 0, fmt.Errorf("enter the namespace: %w", err)
	}
	fd, serr := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err := unix.Setns(own, unix.CLONE_NEWNET); err != nil {
		if serr == nil {
			unix.Close(fd)
		}
		return 0, fmt.Errorf("leave the namespace: %w", err)
	}
	runtime.UnlockOSThread()
	if serr != nil {
		return 0, serr
	}
	defer unix.Close(fd)
	v, err := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
	if err != nil {
		return 0, fmt.Errorf("SO_NETNS_COOKIE: %w", err)
	}
	return v, nil
}

func SocketCookie(fd int) (uint64, error) {
	return unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_COOKIE)
}
