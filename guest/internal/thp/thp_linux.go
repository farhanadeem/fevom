package thp

import (
	"os"

	"golang.org/x/sys/unix"
)

func Off() error { return unix.Prctl(unix.PR_SET_THP_DISABLE, 1, 0, 0, 0) }

func Restore() error { return unix.Prctl(unix.PR_SET_THP_DISABLE, 0, 0, 0, 0) }

func IsOff() (bool, error) {
	n, err := unix.PrctlRetInt(unix.PR_GET_THP_DISABLE, 0, 0, 0, 0)
	return n&1 != 0, err
}

const SmallFoliosPath = "/sys/kernel/mm/transparent_hugepage/hugepages-16kB/enabled"

func SmallFolios() error {
	return os.WriteFile(SmallFoliosPath, []byte("always"), 0)
}
