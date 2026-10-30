package machines

import (
	"archive/tar"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func Unpack(root int, r io.Reader) (int, []string, error) {
	u := &unpacker{root: root}
	n, err := u.tar(tar.NewReader(r))
	return n, u.warnings, err
}

func Overlay(root int, dir string) ([]string, error) {
	u := &unpacker{root: root}
	return u.overlay(dir)
}

type unpacker struct {
	root     int
	dirs     []*tar.Header
	warnings []string
}

func clean(name string) (string, error) {
	p := path.Clean("/" + name)
	if p == "/" {
		return ".", nil
	}
	for _, c := range strings.Split(p[1:], "/") {
		if c == ".." {
			return "", fmt.Errorf("%q leaves the root", name)
		}
	}
	return p[1:], nil
}

func (u *unpacker) parent(rel string) (int, string, error) {
	dir, base := path.Split(rel)
	if dir == "" {
		dir = "."
	}
	fd, err := unix.Openat2(u.root, dir, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	})
	return fd, base, err
}

func (u *unpacker) tar(r *tar.Reader) (int, error) {
	n := 0
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, err
		}
		n++
		if err := u.entry(h, r); err != nil {
			return n, fmt.Errorf("%s: %w", h.Name, err)
		}
	}
	for i := len(u.dirs) - 1; i >= 0; i-- {
		h := u.dirs[i]
		rel, _ := clean(h.Name)
		if rel == "." {
			if err := u.meta(u.root, "", h, false); err != nil {
				return n, err
			}
			continue
		}
		dfd, base, err := u.parent(rel)
		if err != nil {
			return n, err
		}
		err = u.meta(dfd, base, h, false)
		unix.Close(dfd)
		if err != nil {
			return n, fmt.Errorf("%s: %w", h.Name, err)
		}
	}
	return n, nil
}

func (u *unpacker) entry(h *tar.Header, r io.Reader) error {
	rel, err := clean(h.Name)
	if err != nil {
		return err
	}
	if rel == "." {
		u.dirs = append(u.dirs, h)
		return nil
	}
	dfd, base, err := u.parent(rel)
	if err != nil {
		return err
	}
	defer unix.Close(dfd)
	mode := uint32(h.Mode & 0o7777)
	if h.Typeflag != tar.TypeDir {
		if err := unix.Unlinkat(dfd, base, 0); err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.EISDIR) {
			return err
		}
	}
	switch h.Typeflag {
	case tar.TypeDir:
		if err := unix.Mkdirat(dfd, base, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		u.dirs = append(u.dirs, h)
		return nil
	case tar.TypeReg:
		fd, err := unix.Openat(dfd, base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(fd), base)
		_, err = io.Copy(f, r)
		f.Close()
		if err != nil {
			return err
		}
	case tar.TypeSymlink:
		if err := unix.Symlinkat(h.Linkname, dfd, base); err != nil {
			return err
		}
	case tar.TypeLink:
		target, err := clean(h.Linkname)
		if err != nil {
			return err
		}
		tfd, tbase, err := u.parent(target)
		if err != nil {
			return err
		}
		err = unix.Linkat(tfd, tbase, dfd, base, 0)
		unix.Close(tfd)
		return err
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		kind := map[byte]uint32{tar.TypeChar: unix.S_IFCHR, tar.TypeBlock: unix.S_IFBLK, tar.TypeFifo: unix.S_IFIFO}[h.Typeflag]
		if err := unix.Mknodat(dfd, base, kind|mode, int(unix.Mkdev(uint32(h.Devmajor), uint32(h.Devminor)))); err != nil {
			return err
		}
	default:
		return fmt.Errorf("type %q not unpacked", h.Typeflag)
	}
	return u.meta(dfd, base, h, h.Typeflag == tar.TypeSymlink)
}

func (u *unpacker) meta(dfd int, base string, h *tar.Header, link bool) error {
	name := base
	flags := unix.AT_SYMLINK_NOFOLLOW
	if base == "" {
		name, flags = "", unix.AT_EMPTY_PATH
	}
	if !link && base != "" {
		var st unix.Stat_t
		if err := unix.Fstatat(dfd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return fmt.Errorf("%s is a link where the archive has a %s", h.Name, map[bool]string{true: "directory", false: "file"}[h.Typeflag == tar.TypeDir])
		}
	}
	if err := unix.Fchownat(dfd, name, h.Uid, h.Gid, flags); err != nil {
		return fmt.Errorf("chown: %w", err)
	}
	proc := fmt.Sprintf("/proc/self/fd/%d", dfd)
	if base != "" {
		proc += "/" + base
	}
	if !link {
		if err := unix.Fchmodat(dfd, nameOrDot(name), uint32(h.Mode&0o7777), 0); err != nil {
			return fmt.Errorf("chmod: %w", err)
		}
	}
	for k, v := range h.PAXRecords {
		var attr, value string
		switch {
		case strings.HasPrefix(k, "SCHILY.xattr."):
			attr, value = strings.TrimPrefix(k, "SCHILY.xattr."), v
		case strings.HasPrefix(k, "LIBARCHIVE.xattr."):
			attr = strings.TrimPrefix(k, "LIBARCHIVE.xattr.")
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				continue
			}
			value = string(b)
		default:
			continue
		}
		if err := unix.Lsetxattr(proc, attr, []byte(value), 0); err != nil {
			u.warnings = append(u.warnings, fmt.Sprintf("%s: xattr %s: %v", h.Name, attr, err))
		}
	}
	ts := []unix.Timespec{unix.NsecToTimespec(h.ModTime.UnixNano()), unix.NsecToTimespec(h.ModTime.UnixNano())}
	unix.UtimesNanoAt(dfd, nameOrDot(name), ts, unix.AT_SYMLINK_NOFOLLOW)
	return nil
}

func nameOrDot(n string) string {
	if n == "" {
		return "."
	}
	return n
}

func (u *unpacker) overlay(dir string) ([]string, error) {
	var written []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dfd, base, err := u.parent(rel)
		if err != nil {
			return err
		}
		defer unix.Close(dfd)
		var st unix.Stat_t
		mode, uid, gid := uint32(0o644), 0, 0
		if unix.Fstatat(dfd, base, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFREG {
			mode, uid, gid = st.Mode&0o7777, int(st.Uid), int(st.Gid)
		}
		unix.Unlinkat(dfd, base, 0)
		fd, err := unix.Openat(dfd, base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(fd), base)
		_, err = f.Write(data)
		f.Close()
		unix.Fchownat(dfd, base, uid, gid, unix.AT_SYMLINK_NOFOLLOW)
		unix.Fchmodat(dfd, base, mode, 0)
		written = append(written, "/"+rel)
		return err
	})
	return written, err
}
