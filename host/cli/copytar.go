package cli

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"fev/proto/agent"
	api "fev/proto/daemon"
)

func isolatedMachine(ctx context.Context, c *api.Client, machine string) bool {
	l, err := c.Machines(ctx)
	if err != nil {
		return false
	}
	for _, m := range l.Machines {
		if m.Name == machine || (machine == "" && m.Name == l.Default) {
			return m.Isolated
		}
	}
	return false
}

func (a *app) pushTar(ctx context.Context, c *api.Client, machine string, paths []string, dest string) error {
	argv := []string{dest}
	for _, p := range paths {
		argv = append(argv, filepath.Base(p))
	}
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeTar(pw, paths, func(skipped string) {
			fmt.Fprintf(a.Stderr, "fev: push: %s is not a file, folder or link; left out\n", skipped)
		}))
	}()
	defer pr.Close()
	return a.copySession(ctx, c, "push", api.SessionStartParams{Machine: machine, Copy: agent.CopyIn, Argv: argv},
		sessionStreams{in: pr, out: a.Stderr, errw: a.Stderr})
}

func (a *app) pullTar(ctx context.Context, c *api.Client, machine string, from []string, dest string, many bool) error {
	into, err := os.MkdirTemp(scratchParent(dest), ".fev-pull-")
	if err != nil {
		return failf(ExitFailed, "pull: %v", err)
	}
	defer os.RemoveAll(into)
	pr, pw := io.Pipe()
	unpacked := make(chan error, 1)
	go func() {
		err := readTar(pr, into)
		io.Copy(io.Discard, pr)
		unpacked <- err
	}()
	err = a.copySession(ctx, c, "pull", api.SessionStartParams{Machine: machine, Copy: agent.CopyOut, Argv: from},
		sessionStreams{in: strings.NewReader(""), out: pw, errw: a.Stderr})
	pw.Close()
	if uerr := <-unpacked; err == nil && uerr != nil {
		return failf(ExitFailed, "pull: what the machine sent: %v", uerr)
	}
	if err != nil {
		return err
	}
	return placePulled(into, dest, many)
}

func scratchParent(dest string) string {
	if st, err := os.Stat(dest); err == nil && st.IsDir() {
		return dest
	}
	return filepath.Dir(dest)
}

func (a *app) copySession(ctx context.Context, c *api.Client, what string, p api.SessionStartParams, s sessionStreams) error {
	e, err := a.session(ctx, c, p, s)
	if err != nil {
		return err
	}
	code, err := exitStatus(e)
	if err != nil {
		return err
	}
	if code != 0 {
		return &exit{code: ExitFailed, err: fmt.Errorf("%s: the copy in the machine exited with %d", what, code)}
	}
	return nil
}

func writeTar(w io.Writer, paths []string, skipped func(string)) error {
	tw := tar.NewWriter(w)
	for _, top := range paths {
		parent := filepath.Dir(top)
		err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(parent, p)
			if err != nil {
				return err
			}
			link := ""
			switch {
			case info.Mode().IsRegular(), info.IsDir():
			case info.Mode()&fs.ModeSymlink != 0:
				if link, err = os.Readlink(p); err != nil {
					return err
				}
			default:
				skipped(p)
				return nil
			}
			h, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			h.Name = filepath.ToSlash(rel)
			if info.IsDir() {
				h.Name += "/"
			}
			h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
			h.Format = tar.FormatPAX
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				f, err := os.Open(p)
				if err != nil {
					return err
				}
				_, err = io.Copy(tw, f)
				f.Close()
				return err
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

func readTar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("%q is outside the copy", h.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := noLinkBetween(dir, filepath.Dir(target)); err != nil {
			return err
		}
		mode := fs.FileMode(h.Mode) & 0o777
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(target, mode|0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			if st, err := os.Lstat(target); err != nil || !st.IsDir() {
				return fmt.Errorf("%q is not a folder", h.Name)
			}
		case tar.TypeReg:
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			os.Chtimes(target, time.Time{}, h.ModTime)
		case tar.TypeSymlink:
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		default:
		}
	}
}

func noLinkBetween(root, p string) error {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%s is outside the copy", p)
	}
	if rel == "." {
		return nil
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return fmt.Errorf("%s is not a folder of the copy", cur)
		}
	}
	return nil
}

func placePulled(from, dest string, many bool) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		return failf(ExitFailed, "pull: %v", err)
	}
	if st, err := os.Stat(dest); err == nil && st.IsDir() {
		for _, e := range entries {
			if err := place(filepath.Join(from, e.Name()), filepath.Join(dest, e.Name())); err != nil {
				return failf(ExitFailed, "pull: %v", err)
			}
		}
		return nil
	}
	if many || len(entries) != 1 {
		return failf(ExitFailed, "pull: %s is not a folder", dest)
	}
	if err := place(filepath.Join(from, entries[0].Name()), dest); err != nil {
		return failf(ExitFailed, "pull: %v", err)
	}
	return nil
}

func place(src, dst string) error {
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		if d, err := os.Lstat(dst); err == nil && d.IsDir() {
			return fmt.Errorf("%s is a folder; %s is not", dst, filepath.Base(src))
		}
		return os.Rename(src, dst)
	}
	if d, err := os.Stat(dst); err == nil && !d.IsDir() {
		return fmt.Errorf("%s is not a folder", dst)
	} else if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return os.Rename(src, dst)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := place(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
