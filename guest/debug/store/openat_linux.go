package store

import "golang.org/x/sys/unix"

func openatBeneath(dirfd int, name string, flags int, mode uint32) (int, error) {
	return unix.Openat2(dirfd, name, &unix.OpenHow{
		Flags:   uint64(flags) | unix.O_CLOEXEC,
		Mode:    uint64(mode),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	})
}
