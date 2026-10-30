package copytar

import (
	"archive/tar"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type Warn func(format string, args ...any)

const beneath = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS

func Unpack(r io.Reader, dest string, names []string, warn Warn) error {
	if len(names) == 0 {
		return errors.New("nothing to copy")
	}
	top := map[string]string{}
	for _, n := range names {
		if n == "" || n == "." || n == ".." || strings.Contains(n, "/") {
			return fmt.Errorf("%q is not a name of one component", n)
		}
		if _, dup := top[n]; dup {
			return fmt.Errorf("two of the copy's items are named %q", n)
		}
		top[n] = n
	}
	root, err := destination(dest, names, top)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name, err := entryName(h.Name)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		first, rest, _ := strings.Cut(name, "/")
		mapped, ok := top[first]
		if !ok {
			return fmt.Errorf("%q is not under one of the copy's names", h.Name)
		}
		target := mapped
		if rest != "" {
			target += "/" + rest
		}
		if err := unpackOne(root, target, h, tr, warn); err != nil {
			return err
		}
	}
}

func entryName(raw string) (string, error) {
	n := strings.TrimPrefix(raw, "./")
	if path.IsAbs(n) {
		return "", fmt.Errorf("%q is outside the copy", raw)
	}
	for _, part := range strings.Split(n, "/") {
		if part == ".." {
			return "", fmt.Errorf("%q is outside the copy", raw)
		}
	}
	n = path.Clean(n)
	if n == "." {
		return "", nil
	}
	return n, nil
}

func destination(dest string, names []string, top map[string]string) (int, error) {
	open := func(dir string) (int, error) {
		fd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return -1, fmt.Errorf("%s: %w", dir, err)
		}
		return fd, nil
	}
	st, err := os.Stat(dest)
	switch {
	case err == nil && st.IsDir():
		return open(dest)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return -1, err
	case len(names) > 1 || strings.HasSuffix(dest, "/"):
		return -1, fmt.Errorf("%s is not a folder", dest)
	}
	clean := filepath.Clean(dest)
	base := filepath.Base(clean)
	if base == "/" || base == "." || base == ".." {
		return -1, fmt.Errorf("%s is not a place a copy can be made", dest)
	}
	top[names[0]] = base
	return open(filepath.Dir(clean))
}

func unpackOne(root int, target string, h *tar.Header, body io.Reader, warn Warn) error {
	dir, base := path.Split(target)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		dir = "."
	}
	parent, err := unix.Openat2(root, dir, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: beneath})
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) {
			return fmt.Errorf("%s: a link on the way; nothing is written through a link", target)
		}
		return fmt.Errorf("%s: %w", dir, err)
	}
	defer unix.Close(parent)
	perm := uint32(h.Mode) & 0o777
	var st unix.Stat_t
	there := unix.Fstatat(parent, base, &st, unix.AT_SYMLINK_NOFOLLOW) == nil
	isDir := there && st.Mode&unix.S_IFMT == unix.S_IFDIR
	switch h.Typeflag {
	case tar.TypeDir:
		if there {
			if !isDir {
				return fmt.Errorf("%s: cannot put a folder where something else is", target)
			}
			return nil
		}
		if err := unix.Mkdirat(parent, base, perm|0o700); err != nil {
			return fmt.Errorf("%s: %w", target, err)
		}
		return nil
	case tar.TypeReg:
		if isDir {
			return fmt.Errorf("%s: cannot put a file where a folder is", target)
		}
		return replace(parent, base, target, func(tmp string) error {
			fd, err := unix.Openat(parent, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
			if err != nil {
				return err
			}
			f := os.NewFile(uintptr(fd), tmp)
			_, err = io.Copy(f, body)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err == nil && !h.ModTime.IsZero() {
				t := unix.NsecToTimespec(h.ModTime.UnixNano())
				err = unix.UtimesNanoAt(parent, tmp, []unix.Timespec{{Nsec: unix.UTIME_OMIT}, t}, unix.AT_SYMLINK_NOFOLLOW)
			}
			return err
		})
	case tar.TypeSymlink:
		if isDir {
			return fmt.Errorf("%s: cannot put a link where a folder is", target)
		}
		return replace(parent, base, target, func(tmp string) error {
			return unix.Symlinkat(h.Linkname, parent, tmp)
		})
	default:
		warn("%s is not a file, folder or link; left out", strings.TrimPrefix(h.Name, "./"))
		return nil
	}
}

func replace(parent int, base, target string, mk func(tmp string) error) error {
	var raw [6]byte
	rand.Read(raw[:])
	tmp := ".fev-copy-" + hex.EncodeToString(raw[:])
	if err := mk(tmp); err != nil {
		unix.Unlinkat(parent, tmp, 0)
		return fmt.Errorf("%s: %w", target, err)
	}
	if err := unix.Renameat(parent, tmp, parent, base); err != nil {
		unix.Unlinkat(parent, tmp, 0)
		return fmt.Errorf("%s: %w", target, err)
	}
	return nil
}

func Pack(w io.Writer, paths []string, warn Warn) error {
	type item struct{ abs, base string }
	var items []item
	bases := map[string]bool{}
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(abs); err != nil {
			return fmt.Errorf("%s: %w", p, unwrapPath(err))
		}
		base := filepath.Base(abs)
		if base == "/" {
			return errors.New("/ is not copied whole")
		}
		if bases[base] {
			return fmt.Errorf("two of the paths are named %q", base)
		}
		bases[base] = true
		items = append(items, item{abs, base})
	}
	tw := tar.NewWriter(w)
	for _, it := range items {
		parent := filepath.Dir(it.abs)
		err := filepath.WalkDir(it.abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(parent, p)
			if err != nil {
				return err
			}
			return packOne(tw, p, filepath.ToSlash(rel), d, warn)
		})
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

func packOne(tw *tar.Writer, p, name string, d fs.DirEntry, warn Warn) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	header := func(fi fs.FileInfo, link string) *tar.Header {
		return &tar.Header{
			Name: name, Linkname: link, Mode: int64(fi.Mode().Perm()),
			ModTime: fi.ModTime().Truncate(time.Second), Format: tar.FormatPAX,
		}
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		link, err := os.Readlink(p)
		if err != nil {
			return err
		}
		h := header(info, link)
		h.Typeflag = tar.TypeSymlink
		return tw.WriteHeader(h)
	case info.IsDir():
		h := header(info, "")
		h.Typeflag, h.Name = tar.TypeDir, name+"/"
		return tw.WriteHeader(h)
	case info.Mode().IsRegular():
		fd, err := unix.Open(p, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		f := os.NewFile(uintptr(fd), p)
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			warn("%s is not a file, folder or link; left out", p)
			return nil
		}
		h := header(st, "")
		h.Typeflag, h.Size = tar.TypeReg, st.Size()
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.CopyN(tw, f, st.Size()); err != nil {
			return fmt.Errorf("%s: %w (it changed as it was copied)", p, err)
		}
		return nil
	default:
		warn("%s is not a file, folder or link; left out", p)
		return nil
	}
}

func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
