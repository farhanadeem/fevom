package machines

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"
)

func applyTemplates(root string, all []api.ImageTemplate, trigger string, ctx TemplateContext) ([]string, error) {
	ts := TemplatesFor(all, trigger)
	if len(ts) == 0 {
		return nil, nil
	}
	rfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rfd)
	var written []string
	for _, t := range ts {
		text, err := RenderTemplate(t, trigger, ctx)
		if err != nil {
			return written, err
		}
		ok, err := writeInRoot(rfd, t.Path, []byte(text), t.CreateOnly)
		if err != nil {
			return written, fmt.Errorf("%s: %w", t.Path, err)
		}
		if ok {
			written = append(written, t.Path)
		}
	}
	return written, nil
}

const inRoot = unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS

func mkdirInRoot(rfd int, dir string) error {
	rel := strings.Trim(path.Clean("/"+dir), "/")
	if rel == "" {
		return nil
	}
	cur := ""
	for _, c := range strings.Split(rel, "/") {
		parent := cur
		if parent == "" {
			parent = "."
		}
		cur = path.Join(cur, c)
		fd, err := unix.Openat2(rfd, cur, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: inRoot})
		if err == nil {
			unix.Close(fd)
			continue
		}
		if !errors.Is(err, unix.ENOENT) {
			return err
		}
		pfd, err := unix.Openat2(rfd, parent, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: inRoot})
		if err != nil {
			return err
		}
		err = unix.Mkdirat(pfd, c, 0o755)
		if err == nil {
			unix.Fchmodat(pfd, c, 0o755, 0)
		}
		unix.Close(pfd)
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
	}
	return nil
}

func writeInRoot(rfd int, p string, data []byte, createOnly bool) (bool, error) {
	rel := strings.TrimPrefix(path.Clean("/"+p), "/")
	if rel == "" {
		return false, errors.New("not a file")
	}
	if err := mkdirInRoot(rfd, path.Dir(rel)); err != nil {
		return false, err
	}
	fd, err := unix.Openat2(rfd, rel, &unix.OpenHow{Flags: unix.O_WRONLY | unix.O_CLOEXEC, Resolve: inRoot})
	made := false
	switch {
	case err == nil && createOnly:
		unix.Close(fd)
		return false, nil
	case errors.Is(err, unix.ENOENT):
		fd, err = unix.Openat2(rfd, rel, &unix.OpenHow{Flags: unix.O_WRONLY | unix.O_CLOEXEC | unix.O_CREAT, Mode: 0o644, Resolve: inRoot})
		made = true
	}
	if err != nil {
		return false, err
	}
	f := os.NewFile(uintptr(fd), rel)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return false, fmt.Errorf("not a regular file")
	}
	if made {
		if err := unix.Fchmod(fd, 0o644); err != nil {
			return false, err
		}
	}
	if err := f.Truncate(0); err != nil {
		return false, err
	}
	if _, err := f.Write(data); err != nil {
		return false, err
	}
	return true, f.Close()
}

func readInRoot(rfd int, p string, limit int64) ([]byte, error) {
	rel := strings.TrimPrefix(path.Clean("/"+p), "/")
	fd, err := unix.Openat2(rfd, rel, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC, Resolve: inRoot})
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), rel)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	if st.Size > limit {
		return nil, fmt.Errorf("%s is %d bytes, more than %d", p, st.Size, limit)
	}
	b := make([]byte, st.Size)
	n, err := f.ReadAt(b, 0)
	if err != nil && n != len(b) {
		return nil, err
	}
	return b[:n], nil
}
