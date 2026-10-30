package storage

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const (
	ImageName   = "data.img"
	DefaultSize = 1 << 40
)

func ImagePath(home string) string {
	return filepath.Join(home, "data", ImageName)
}

type Options struct {
	Size        int64
	AllowBackup bool
	Swap        func(error)
}

func Ensure(path string, opts Options) (created bool, err error) {
	size := opts.Size
	if size == 0 {
		size = DefaultSize
	}
	if size <= 0 || size%4096 != 0 {
		return false, fmt.Errorf("image size %d: must be a positive multiple of 4096", size)
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return false, err
		}
		if err := KeepsHoles(filepath.Dir(path)); err != nil {
			return false, err
		}
	}
	swapErr := ensureSwap(path)
	if opts.Swap != nil {
		opts.Swap(swapErr)
	}
	st, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true, create(path, size, opts.AllowBackup)
	case err != nil:
		return false, err
	case !st.Mode().IsRegular():
		return false, fmt.Errorf("%s is not a regular file", path)
	}
	if st.Size() < size {
		if err := os.Truncate(path, size); err != nil {
			return false, fmt.Errorf("grow %s: %w", path, err)
		}
	}
	return false, SetBackupExcluded(path, !opts.AllowBackup)
}

func create(path string, size int64, allowBackup bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := path + ".new"
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Truncate(size); err != nil {
		return fail(fmt.Errorf("size %s to %d bytes: %w", tmp, size, err))
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	if err := SetBackupExcluded(tmp, !allowBackup); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func Allocated(path string) (int64, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, &fs.PathError{Op: "stat", Path: path, Err: err}
	}
	return st.Blocks * 512, nil
}

func Reset(path string) error {
	if err := os.Remove(SwapImagePath(path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

const backupAttr = "com.apple.metadata:com_apple_backup_excludeItem"

var backupValue = append(append(
	[]byte("bplist00\x5f\x10\x11com.apple.backupd"),
	0x08),
	0, 0, 0, 0, 0, 0, 1, 1,
	0, 0, 0, 0, 0, 0, 0, 1,
	0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 28,
)

func SetBackupExcluded(path string, excluded bool) error {
	if excluded {
		if err := unix.Setxattr(path, backupAttr, backupValue, 0); err != nil {
			return fmt.Errorf("exclude %s from backups: %w", path, err)
		}
		return nil
	}
	err := unix.Removexattr(path, backupAttr)
	if err != nil && !errors.Is(err, unix.ENOATTR) {
		return fmt.Errorf("include %s in backups: %w", path, err)
	}
	return nil
}

func BackupExcluded(path string) (bool, error) {
	buf := make([]byte, 256)
	n, err := unix.Getxattr(path, backupAttr, buf)
	if errors.Is(err, unix.ENOATTR) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bytes.Equal(buf[:n], backupValue), nil
}
