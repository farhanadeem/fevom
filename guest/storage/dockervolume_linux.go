package storage

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const VolumeArchiveRoot = "data"

func ExportVolume(dir string, config []byte, w io.Writer, progress func(int64)) (int64, error) {
	if len(config) > MaxArchiveConfig {
		return 0, fmt.Errorf("the archive's config is %d bytes, more than %d", len(config), MaxArchiveConfig)
	}
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		return 0, fmt.Errorf("%s is not a folder", dir)
	}
	return writeArchive(w, config, dir, VolumeArchiveRoot, progress)
}

func ImportVolume(dir string, r io.Reader, unpack Unpacker) ([]byte, []string, int64, error) {
	return importInto(dir, VolumeArchiveRoot, r, unpack)
}

func CloneTree(src, dst string) (int64, error) {
	var st unix.Stat_t
	if err := unix.Lstat(src, &st); err != nil {
		return 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return 0, fmt.Errorf("%s is not a folder", src)
	}
	if err := unix.Mkdir(dst, 0o700); err != nil {
		return 0, err
	}
	c := &cloner{links: map[[2]uint64]string{}}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := dst
		if rel != "." {
			to = filepath.Join(dst, rel)
		}
		return c.entry(p, to, rel == ".")
	})
	if err != nil {
		return c.n, err
	}
	for i := len(c.dirs) - 1; i >= 0; i-- {
		d := c.dirs[i]
		if err := meta(d.to, &d.st, false); err != nil {
			return c.n, fmt.Errorf("%s: %w", d.to, err)
		}
	}
	return c.n, nil
}

type clonedDir struct {
	to string
	st unix.Stat_t
}

type cloner struct {
	n     int64
	links map[[2]uint64]string
	dirs  []clonedDir
}

func (c *cloner) entry(from, to string, top bool) error {
	var st unix.Stat_t
	if err := unix.Lstat(from, &st); err != nil {
		return err
	}
	kind := st.Mode & unix.S_IFMT
	if kind != unix.S_IFDIR && st.Nlink > 1 {
		key := [2]uint64{st.Dev, st.Ino}
		if first, ok := c.links[key]; ok {
			c.n++
			return unix.Linkat(unix.AT_FDCWD, first, unix.AT_FDCWD, to, 0)
		}
		c.links[key] = to
	}
	switch kind {
	case unix.S_IFDIR:
		if !top {
			if err := unix.Mkdir(to, 0o700); err != nil {
				return err
			}
		}
		if err := copyXattrs(from, to); err != nil {
			return fmt.Errorf("%s: %w", from, err)
		}
		c.dirs = append(c.dirs, clonedDir{to: to, st: st})
		c.n++
		return nil
	case unix.S_IFREG:
		if err := cloneFile(from, to); err != nil {
			return fmt.Errorf("%s: %w", from, err)
		}
	case unix.S_IFLNK:
		target, err := os.Readlink(from)
		if err != nil {
			return err
		}
		if err := unix.Symlink(target, to); err != nil {
			return err
		}
	case unix.S_IFCHR, unix.S_IFBLK, unix.S_IFIFO:
		if err := unix.Mknod(to, st.Mode&(unix.S_IFMT|0o7777), int(st.Rdev)); err != nil {
			return err
		}
	default:
		return nil
	}
	c.n++
	link := kind == unix.S_IFLNK
	if err := owner(to, &st, link); err != nil {
		return err
	}
	if !link {
		if err := copyXattrs(from, to); err != nil {
			return fmt.Errorf("%s: %w", from, err)
		}
	}
	return times(to, &st)
}

func meta(to string, st *unix.Stat_t, link bool) error {
	if err := owner(to, st, link); err != nil {
		return err
	}
	return times(to, st)
}

func owner(to string, st *unix.Stat_t, link bool) error {
	if err := unix.Lchown(to, int(st.Uid), int(st.Gid)); err != nil {
		return err
	}
	if !link {
		return unix.Chmod(to, st.Mode&0o7777)
	}
	return nil
}

func times(to string, st *unix.Stat_t) error {
	ts := []unix.Timespec{st.Atim, st.Mtim}
	return unix.UtimesNanoAt(unix.AT_FDCWD, to, ts, unix.AT_SYMLINK_NOFOLLOW)
}

func cloneFile(from, to string) error {
	in, err := os.OpenFile(from, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	err = unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTTY) {
		_, err = io.Copy(out, in)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

func copyXattrs(from, to string) error {
	size, err := unix.Llistxattr(from, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			return nil
		}
		return err
	}
	if size == 0 {
		return nil
	}
	buf := make([]byte, size)
	if size, err = unix.Llistxattr(from, buf); err != nil {
		return err
	}
	for _, attr := range strings.Split(strings.TrimRight(string(buf[:size]), "\x00"), "\x00") {
		if attr == "" {
			continue
		}
		n, err := unix.Lgetxattr(from, attr, nil)
		if err != nil {
			if errors.Is(err, unix.ENODATA) {
				continue
			}
			return err
		}
		v := make([]byte, n)
		if n, err = unix.Lgetxattr(from, attr, v); err != nil {
			return err
		}
		if err := unix.Lsetxattr(to, attr, v[:n], 0); err != nil {
			return fmt.Errorf("%s: %w", attr, err)
		}
	}
	return nil
}
