//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"fev/proto/bridge"
)

func pushMain(prog string, args []string) int {
	if len(args) < 1 || len(args) > 2 || helpWord(args[0]) {
		fmt.Fprintf(stderr, "usage: %s PATH [MACDEST]\n", prog)
		return 2
	}
	i, err := shareInfo()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return 1
	}
	dest := i.Home
	if len(args) == 2 {
		dest = args[1]
	}
	to, ok := bridge.LinuxPathIn(dest, i.Home, i.Root, i.View)
	if !ok {
		fmt.Fprintf(stderr, "%s: %s is not among the Mac's files this machine has\n", prog, dest)
		return 1
	}
	if err := copyInto(args[0], to); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return 1
	}
	return 0
}

func pullMain(prog string, args []string) int {
	if len(args) < 1 || len(args) > 2 || helpWord(args[0]) {
		fmt.Fprintf(stderr, "usage: %s MACPATH [DEST]\n", prog)
		return 2
	}
	i, err := shareInfo()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return 1
	}
	dest := "."
	if len(args) == 2 {
		dest = args[1]
	}
	src := args[0]
	if rest, ok := strings.CutPrefix(src, "~/"); ok || src == "~" {
		src = rest
	}
	from, ok := bridge.LinuxPathIn(src, i.Home, i.Root, i.View)
	if !ok {
		fmt.Fprintf(stderr, "%s: %s is not among the Mac's files this machine has\n", prog, src)
		return 1
	}
	if err := copyInto(from, dest); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return 1
	}
	return 0
}

func shareInfo() (*bridge.Info, error) {
	i, err := info()
	if err != nil {
		return nil, err
	}
	if !i.Share {
		return nil, errors.New("this machine does not have the Mac's files (files.share is off)")
	}
	return i, nil
}

func copyInto(src, dest string) error {
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if d, err := os.Stat(dest); err == nil && d.IsDir() {
		dest = filepath.Join(dest, filepath.Base(filepath.Clean(src)))
	}
	if same(src, dest) {
		return fmt.Errorf("%s and %s are the same file", src, dest)
	}
	if st.IsDir() {
		return copyTree(src, dest)
	}
	return copyOne(src, dest, st)
}

func same(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	return err == nil && os.SameFile(sa, sb)
}

func copyTree(src, dest string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if err := os.Mkdir(target, st.Mode().Perm()|0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			return nil
		}
		return copyOne(p, target, st)
	})
}

func copyOne(src, dest string, st fs.FileInfo) error {
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		os.Remove(dest)
		return os.Symlink(target, dest)
	case !st.Mode().IsRegular():
		return fmt.Errorf("%s is not a file, a directory or a link", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if d, err := os.Lstat(dest); err == nil && (d.IsDir() || d.Mode()&fs.ModeSymlink != 0) {
		if d.IsDir() {
			return fmt.Errorf("%s is a directory", dest)
		}
		os.Remove(dest)
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dest, st.Mode().Perm())
}
