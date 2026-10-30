package machines

import (
	"golang.org/x/sys/unix"
)

func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	flags := ifr.Uint16()
	if flags&unix.IFF_UP != 0 {
		return nil
	}
	ifr.SetUint16(flags | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}
