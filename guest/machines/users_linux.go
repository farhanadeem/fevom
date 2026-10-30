package machines

import (
	"io"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

func readIn(root, name string) ([]byte, error) {
	dir, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: root, Err: err}
	}
	defer unix.Close(dir)
	rel := strings.TrimPrefix(path.Clean("/"+name), "/")
	if rel == "" {
		rel = "."
	}
	fd, err := unix.Openat2(dir, rel, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOCTTY | unix.O_NONBLOCK,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, &os.PathError{Op: "read", Path: name, Err: unix.EINVAL}
	}
	return io.ReadAll(f)
}
