package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type MoveOptions struct {
	AllowBackup bool
	Margin      uint64
	Progress    func(done, total int64)
	Committed   func() error
}

type MoveResult struct {
	Renamed   bool
	Copied    int64
	Allocated int64
}

func Move(ctx context.Context, src, dstDir string, opts MoveOptions) (MoveResult, error) {
	var res MoveResult
	dstDir = filepath.Clean(dstDir)
	if !filepath.IsAbs(dstDir) {
		return res, fmt.Errorf("%s: the data directory must be an absolute path", dstDir)
	}
	if filepath.Clean(filepath.Dir(src)) == dstDir {
		return res, fmt.Errorf("the data image is already in %s", dstDir)
	}
	dst := filepath.Join(dstDir, ImageName)
	if _, err := os.Lstat(dst); err == nil {
		return res, fmt.Errorf("%s already holds a data image (%s): fev does not write over it; remove it, or choose another directory", dstDir, ImageName)
	}
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return res, err
	}
	if err := KeepsHoles(dstDir); err != nil {
		return res, err
	}
	in, err := os.Open(src)
	if errors.Is(err, fs.ErrNotExist) {
		return res, commit(opts)
	}
	if err != nil {
		return res, err
	}
	defer in.Close()
	var sst, dst0 unix.Stat_t
	if err := unix.Fstat(int(in.Fd()), &sst); err != nil {
		return res, err
	}
	if err := unix.Stat(dstDir, &dst0); err != nil {
		return res, err
	}
	if sst.Dev == dst0.Dev {
		if err := SetBackupExcluded(src, !opts.AllowBackup); err != nil {
			return res, err
		}
		if err := os.Rename(src, dst); err != nil {
			return res, err
		}
		res.Renamed = true
		res.Allocated = sst.Blocks * 512
		if err := syncDir(dstDir); err != nil {
			return res, err
		}
		if err := commit(opts); err != nil {
			if rerr := os.Rename(dst, src); rerr != nil {
				return res, fmt.Errorf("%w; and the image could not be put back from %s: %v", err, dst, rerr)
			}
			return res, err
		}
		removeSwap(src)
		return res, nil
	}
	need := uint64(sst.Blocks*512) + opts.Margin
	var vfs unix.Statfs_t
	if err := unix.Statfs(dstDir, &vfs); err != nil {
		return res, err
	}
	if free := vfs.Bavail * uint64(vfs.Bsize); free < need {
		return res, fmt.Errorf("%s has %d MiB free; the data image occupies %d MiB and %d MiB must stay free beside it",
			dstDir, free>>20, sst.Blocks*512>>20, opts.Margin>>20)
	}
	tmp := dst + ".moving"
	os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return res, err
	}
	fail := func(err error) (MoveResult, error) {
		out.Close()
		os.Remove(tmp)
		return res, err
	}
	if err := out.Truncate(sst.Size); err != nil {
		return fail(err)
	}
	if res.Copied, err = copyData(ctx, in, out, sst.Size, sst.Blocks*512, opts.Progress); err != nil {
		return fail(err)
	}
	if _, err := unix.FcntlInt(out.Fd(), unix.F_FULLFSYNC, 0); err != nil {
		return fail(fmt.Errorf("flush %s: %w", tmp, err))
	}
	var ost unix.Stat_t
	if err := unix.Fstat(int(out.Fd()), &ost); err != nil {
		return fail(err)
	}
	res.Allocated = ost.Blocks * 512
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return res, err
	}
	if err := SetBackupExcluded(tmp, !opts.AllowBackup); err != nil {
		os.Remove(tmp)
		return res, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return res, err
	}
	if err := syncDir(dstDir); err != nil {
		return res, err
	}
	if err := commit(opts); err != nil {
		os.Remove(dst)
		return res, err
	}
	in.Close()
	if err := os.Remove(src); err != nil {
		return res, fmt.Errorf("the image is in %s, and the old one could not be removed: %w", dstDir, err)
	}
	removeSwap(src)
	return res, nil
}

func commit(opts MoveOptions) error {
	if opts.Committed == nil {
		return nil
	}
	return opts.Committed()
}

func removeSwap(dataImage string) {
	os.Remove(SwapImagePath(dataImage))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

const moveChunk = 1 << 20

func copyData(ctx context.Context, in, out *os.File, size, total int64, progress func(done, total int64)) (int64, error) {
	buf := make([]byte, moveChunk)
	zero := make([]byte, moveChunk)
	var done, written int64
	fd := int(in.Fd())
	off := int64(0)
	for off < size {
		start, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break
		}
		if err != nil {
			return written, fmt.Errorf("find data in %s: %w", in.Name(), err)
		}
		end, err := unix.Seek(fd, start, unix.SEEK_HOLE)
		if err != nil {
			return written, fmt.Errorf("find a hole in %s: %w", in.Name(), err)
		}
		for pos := start; pos < end; {
			if err := ctx.Err(); err != nil {
				return written, err
			}
			n := int64(len(buf))
			if end-pos < n {
				n = end - pos
			}
			got, err := in.ReadAt(buf[:n], pos)
			if err != nil && !(errors.Is(err, io.EOF) && int64(got) == n) {
				return written, fmt.Errorf("read %s: %w", in.Name(), err)
			}
			if !bytes.Equal(buf[:n], zero[:n]) {
				if _, err := out.WriteAt(buf[:n], pos); err != nil {
					return written, fmt.Errorf("write %s: %w", out.Name(), err)
				}
				written += n
			}
			pos += n
			done += n
			if progress != nil {
				progress(done, total)
			}
		}
		off = end
	}
	return written, nil
}
