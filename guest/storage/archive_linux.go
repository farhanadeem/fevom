package storage

import (
	"archive/tar"
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

	"fev/guest/internal/btrfs"
)

const (
	ArchiveConfig    = "_fev/v1/config.json"
	archiveRoot      = "rootfs"
	MaxArchiveConfig = 1 << 20
)

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (m *Machines) Export(name string, config []byte, w io.Writer, freeze Freezer, progress func(int64)) (int64, error) {
	if err := checkName(name); err != nil {
		return 0, err
	}
	if len(config) > MaxArchiveConfig {
		return 0, fmt.Errorf("the archive's config is %d bytes, more than %d", len(config), MaxArchiveConfig)
	}
	src, err := openDir(m.Path(name))
	if err != nil {
		return 0, fmt.Errorf("%w: %s", ErrNoMachine, name)
	}
	defer src.Close()
	if _, err := subvolumeID(m.Path(name)); err != nil {
		return 0, fmt.Errorf("%w: %s", ErrNoMachine, name)
	}
	d, err := m.scratch()
	if err != nil {
		return 0, err
	}
	dir, err := openDir(d)
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	snap := randomName("export-")
	if err := frozen(freeze, func() error { return btrfs.Snapshot(src, dir, snap, true, nil) }); err != nil {
		return 0, fmt.Errorf("snapshot the machine: %w", err)
	}
	snapPath := filepath.Join(d, snap)
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if err := m.deleteTree(snapPath); err != nil {
			m.v.cfg.Logf("storage: the export's snapshot %s could not be removed: %v", snap, err)
		}
	}()
	return writeArchive(w, config, snapPath, archiveRoot, progress)
}

func writeArchive(w io.Writer, config []byte, root, prefix string, progress func(int64)) (int64, error) {
	cw := &countWriter{w: w}
	tw := tar.NewWriter(cw)
	now := time.Now()
	for _, h := range []*tar.Header{
		{Typeflag: tar.TypeDir, Name: "_fev/", Mode: 0o755, ModTime: now, Format: tar.FormatPAX},
		{Typeflag: tar.TypeDir, Name: "_fev/v1/", Mode: 0o755, ModTime: now, Format: tar.FormatPAX},
	} {
		if err := tw.WriteHeader(h); err != nil {
			return cw.n, err
		}
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: ArchiveConfig, Mode: 0o644, Size: int64(len(config)),
		ModTime: now, Format: tar.FormatPAX}); err != nil {
		return cw.n, err
	}
	if _, err := tw.Write(config); err != nil {
		return cw.n, err
	}
	if err := writeTree(tw, root, prefix, func() {
		if progress != nil {
			progress(cw.n)
		}
	}); err != nil {
		return cw.n, err
	}
	if err := tw.Close(); err != nil {
		return cw.n, err
	}
	return cw.n, nil
}

func writeTree(tw *tar.Writer, root, prefix string, after func()) error {
	links := map[[2]uint64]string{}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		name := prefix
		if rel != "." {
			name = path.Join(prefix, filepath.ToSlash(rel))
		}
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		h := &tar.Header{
			Name: name, Mode: int64(st.Mode & 0o7777), Uid: int(st.Uid), Gid: int(st.Gid),
			ModTime:    time.Unix(st.Mtim.Unix()),
			AccessTime: time.Unix(st.Atim.Unix()),
			ChangeTime: time.Unix(st.Ctim.Unix()),
			Format:     tar.FormatPAX,
		}
		kind := st.Mode & unix.S_IFMT
		if kind != unix.S_IFDIR && st.Nlink > 1 {
			key := [2]uint64{st.Dev, st.Ino}
			if first, ok := links[key]; ok {
				h.Typeflag, h.Linkname = tar.TypeLink, first
				return tw.WriteHeader(h)
			}
			links[key] = name
		}
		switch kind {
		case unix.S_IFDIR:
			h.Typeflag, h.Name = tar.TypeDir, name+"/"
		case unix.S_IFREG:
			h.Typeflag, h.Size = tar.TypeReg, st.Size
		case unix.S_IFLNK:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			h.Typeflag, h.Linkname = tar.TypeSymlink, target
		case unix.S_IFCHR, unix.S_IFBLK:
			h.Typeflag = tar.TypeChar
			if kind == unix.S_IFBLK {
				h.Typeflag = tar.TypeBlock
			}
			h.Devmajor, h.Devminor = int64(unix.Major(st.Rdev)), int64(unix.Minor(st.Rdev))
		case unix.S_IFIFO:
			h.Typeflag = tar.TypeFifo
		default:
			return nil
		}
		if err := xattrs(p, h); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if h.Typeflag == tar.TypeReg && h.Size > 0 {
			f, err := os.OpenFile(p, os.O_RDONLY|unix.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			n, err := io.Copy(tw, f)
			f.Close()
			if err == nil && n != h.Size {
				err = fmt.Errorf("read %d bytes of %d", n, h.Size)
			}
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		if after != nil {
			after()
		}
		return nil
	})
}

func xattrs(p string, h *tar.Header) error {
	size, err := unix.Llistxattr(p, nil)
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
	size, err = unix.Llistxattr(p, buf)
	if err != nil {
		return err
	}
	for _, attr := range strings.Split(strings.TrimRight(string(buf[:size]), "\x00"), "\x00") {
		if attr == "" {
			continue
		}
		n, err := unix.Lgetxattr(p, attr, nil)
		if err != nil {
			if errors.Is(err, unix.ENODATA) {
				continue
			}
			return err
		}
		v := make([]byte, n)
		if n, err = unix.Lgetxattr(p, attr, v); err != nil {
			return err
		}
		if h.PAXRecords == nil {
			h.PAXRecords = map[string]string{}
		}
		h.PAXRecords["SCHILY.xattr."+attr] = string(v[:n])
	}
	return nil
}

type Unpacker func(root int, r io.Reader) (int, []string, error)

func (m *Machines) Import(name string, r io.Reader, unpack Unpacker, prepare func(root string) error) ([]byte, []string, error) {
	var with func(string, []byte) error
	if prepare != nil {
		with = func(root string, _ []byte) error { return prepare(root) }
	}
	return m.ImportWith(name, r, unpack, with)
}

func (m *Machines) ImportWith(name string, r io.Reader, unpack Unpacker, prepare func(root string, config []byte) error) ([]byte, []string, error) {
	if err := checkName(name); err != nil {
		return nil, nil, err
	}
	var config []byte
	var warnings []string
	err := m.place(name, func(root string) error {
		var err error
		config, warnings, _, err = importInto(root, archiveRoot, r, unpack)
		if err != nil {
			return err
		}
		if prepare != nil {
			return prepare(root, config)
		}
		return nil
	}, func(dir *os.File, tmp string) error {
		return btrfs.CreateSubvolumeIn(dir, tmp, nil)
	})
	return config, warnings, err
}

func importInto(root, prefix string, r io.Reader, unpack Unpacker) ([]byte, []string, int64, error) {
	tr := tar.NewReader(r)
	var config []byte
	var h *tar.Header
	var err error
	for {
		h, err = tr.Next()
		if err == io.EOF {
			return nil, nil, 0, errors.New("the archive ends before its root")
		}
		if err != nil {
			return nil, nil, 0, err
		}
		n := strings.TrimPrefix(path.Clean("/"+h.Name), "/")
		if n == ArchiveConfig {
			if h.Size > MaxArchiveConfig {
				return nil, nil, 0, fmt.Errorf("%s is %d bytes, more than %d", ArchiveConfig, h.Size, MaxArchiveConfig)
			}
			if config, err = io.ReadAll(io.LimitReader(tr, MaxArchiveConfig)); err != nil {
				return nil, nil, 0, err
			}
			continue
		}
		if n == "_fev" || strings.HasPrefix(n, "_fev/") {
			continue
		}
		if n == prefix || strings.HasPrefix(n, prefix+"/") {
			break
		}
		return nil, nil, 0, fmt.Errorf("the archive holds %q, which is neither _fev/ nor %s/", h.Name, prefix)
	}
	if config == nil {
		return nil, nil, 0, fmt.Errorf("the archive has no %s before its root", ArchiveConfig)
	}
	dir, err := openDir(root)
	if err != nil {
		return nil, nil, 0, err
	}
	defer dir.Close()
	pr, pw := io.Pipe()
	type result struct {
		warnings []string
		err      error
	}
	done := make(chan result, 1)
	go func() {
		_, w, err := unpack(int(dir.Fd()), pr)
		if err != nil {
			pr.CloseWithError(err)
		} else {
			io.Copy(io.Discard, pr)
		}
		done <- result{w, err}
	}()
	tw := tar.NewWriter(pw)
	var entries int64
	werr := func() error {
		for {
			entries++
			rel, ok := strings.CutPrefix(path.Clean("/"+h.Name), "/"+prefix)
			if !ok || (rel != "" && !strings.HasPrefix(rel, "/")) {
				return fmt.Errorf("the archive holds %q after its root began", h.Name)
			}
			h.Name = "." + rel
			if h.Typeflag == tar.TypeDir {
				h.Name += "/"
			}
			if h.Typeflag == tar.TypeLink {
				target, ok := strings.CutPrefix(path.Clean("/"+h.Linkname), "/"+prefix+"/")
				if !ok {
					return fmt.Errorf("%s links to %q, outside the root", h.Name, h.Linkname)
				}
				h.Linkname = target
			}
			h.Xattrs = nil
			h.Format = tar.FormatPAX
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if h.Typeflag == tar.TypeReg {
				if _, err := io.Copy(tw, tr); err != nil {
					return err
				}
			}
			h, err = tr.Next()
			if err == io.EOF {
				return tw.Close()
			}
			if err != nil {
				return err
			}
		}
	}()
	pw.CloseWithError(werr)
	res := <-done
	if res.err != nil {
		return nil, nil, 0, res.err
	}
	if werr != nil {
		return nil, nil, 0, werr
	}
	return config, res.warnings, entries, nil
}
