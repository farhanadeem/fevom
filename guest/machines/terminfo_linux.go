package machines

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"
)

var terminfoDirs = []string{"etc/terminfo", "lib/terminfo", "usr/share/terminfo"}

const (
	terminfoTarget   = "etc/terminfo"
	terminfoFallback = "usr/share/terminfo"
)

func installTerminfo(root, home, name string, b []byte) (string, error) {
	if err := api.CheckTerminfo(name, b); err != nil {
		return "", fmt.Errorf("refused: %w", err)
	}
	rfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(rfd)
	sub := name[:1]
	dirs := terminfoDirs
	if rel := strings.Trim(path.Clean("/"+home), "/"); home != "" && rel != "" {
		dirs = append(append([]string{}, dirs...), path.Join(rel, ".terminfo"))
	}
	for _, d := range dirs {
		fd, err := unix.Openat2(rfd, path.Join(d, sub, name), &unix.OpenHow{Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: inRoot})
		if err == nil {
			unix.Close(fd)
			return "", nil
		}
		if !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENOTDIR) {
			return "", fmt.Errorf("/%s: %w", path.Join(d, sub, name), err)
		}
	}
	base := terminfoTarget
	if fd, err := unix.Openat2(rfd, base, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: inRoot}); err == nil {
		unix.Close(fd)
	} else {
		base = terminfoFallback
	}
	dir := path.Join(base, sub)
	if err := mkdirInRoot(rfd, dir); err != nil {
		return "", fmt.Errorf("/%s: %w", dir, err)
	}
	dfd, err := unix.Openat2(rfd, dir, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: inRoot})
	if err != nil {
		return "", fmt.Errorf("/%s: %w", dir, err)
	}
	defer unix.Close(dfd)
	var rnd [8]byte
	rand.Read(rnd[:])
	tmp := ".fev-terminfo-" + hex.EncodeToString(rnd[:])
	fd, err := unix.Openat(dfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return "", fmt.Errorf("/%s: %w", dir, err)
	}
	f := os.NewFile(uintptr(fd), tmp)
	_, err = f.Write(b)
	if err == nil {
		err = unix.Fchmod(fd, 0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = unix.Renameat2(dfd, tmp, dfd, name, unix.RENAME_NOREPLACE)
		if errors.Is(err, unix.EINVAL) {
			err = unix.Linkat(dfd, tmp, dfd, name, 0)
		}
	}
	unix.Unlinkat(dfd, tmp, 0)
	if errors.Is(err, unix.EEXIST) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("/%s: %w", path.Join(dir, name), err)
	}
	return base, nil
}
