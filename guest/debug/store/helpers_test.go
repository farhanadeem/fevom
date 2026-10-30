package store

import (
	"crypto/sha256"

	"golang.org/x/sys/unix"
)

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }

func openDir(path string) (int, error) {
	return unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}

func closeFd(fd int) { unix.Close(fd) }
