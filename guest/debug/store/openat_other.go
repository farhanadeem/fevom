//go:build !linux

package store

import "golang.org/x/sys/unix"

func openatBeneath(dirfd int, name string, flags int, mode uint32) (int, error) {
	return unix.Openat(dirfd, name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
}
