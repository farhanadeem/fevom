package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"
)

const SalvageCaveat = "Files were read from an older commit or with the filesystem's checks turned off: " +
	"a file rewritten in place since that commit has the commit's size with newer bytes inside it, " +
	"so check each database with its own tool (pg_amcheck, sqlite3's integrity_check, mysqlcheck) before relying on it."

const (
	dockerVolumes = "@docker/volumes"
	volumeData    = "_data"
)

const (
	SalvageVolumesDir  = "volumes"
	SalvageMachinesDir = "machines"
	SalvageDockerDir   = "docker"
)

const maxSalvageErrors = 100

var volumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

type SalvageRequest struct {
	Dest     string
	Volumes  []string
	Machines []string
	Docker   bool
	Progress func(phase string, done, total int64)
}

func (r SalvageRequest) all() bool { return len(r.Volumes) == 0 && len(r.Machines) == 0 && !r.Docker }

func (r *SalvageRequest) check() error {
	r.Volumes, r.Machines = sortedNames(r.Volumes), sortedNames(r.Machines)
	if r.Dest == "" || !filepath.IsAbs(r.Dest) {
		return fmt.Errorf("salvage: destination %q is not an absolute path", r.Dest)
	}
	for _, v := range r.Volumes {
		if !volumeName.MatchString(v) {
			return fmt.Errorf("salvage: %q is not a volume's name", v)
		}
	}
	for _, m := range r.Machines {
		if err := api.CheckMachineName(m); err != nil {
			return fmt.Errorf("salvage: %v", err)
		}
	}
	return nil
}

type salvageItem struct {
	src, dst string
}

type errorList struct {
	lines []string
	n     int64
}

func (e *errorList) add(format string, args ...any) {
	e.n++
	if len(e.lines) < maxSalvageErrors {
		e.lines = append(e.lines, fmt.Sprintf(format, args...))
	}
}

func (v *Volume) Salvage(ctx context.Context, r SalvageRequest) (api.Salvaged, error) {
	if !v.out.ReadOnly {
		return api.Salvaged{}, errors.New("salvage: the data filesystem is mounted read-write and needs no salvage; " +
			"copy files out of it as usual (docker cp, a volume bound in a container, a machine's files)")
	}
	if err := r.check(); err != nil {
		return api.Salvaged{}, err
	}
	items, err := salvageItems(v.cfg.Target, r)
	if err != nil {
		return api.Salvaged{}, err
	}
	if err := prepareDest(r.Dest, items); err != nil {
		return api.Salvaged{}, err
	}
	v.cfg.Logf("storage: salvage: copying %d folders from the read-only mount (rung %d) to %s", len(items), v.out.Rung, r.Dest)
	return copyItems(ctx, items, r.Progress)
}

func salvageItems(root string, r SalvageRequest) ([]salvageItem, error) {
	var items []salvageItem
	var missing []string
	volume := func(name string) salvageItem {
		return salvageItem{src: filepath.Join(root, dockerVolumes, name, volumeData), dst: filepath.Join(r.Dest, SalvageVolumesDir, name)}
	}
	machine := func(name string) salvageItem {
		return salvageItem{src: filepath.Join(root, MachinesSubvolume, name), dst: filepath.Join(r.Dest, SalvageMachinesDir, name)}
	}
	isDir := func(p string) bool {
		st, err := os.Lstat(p)
		return err == nil && st.IsDir()
	}
	volumes, machines := r.Volumes, r.Machines
	if r.all() {
		if entries, err := os.ReadDir(filepath.Join(root, dockerVolumes)); err == nil {
			for _, e := range entries {
				if volumeName.MatchString(e.Name()) && isDir(filepath.Join(root, dockerVolumes, e.Name(), volumeData)) {
					volumes = append(volumes, e.Name())
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("salvage: the volumes cannot be listed: %w", err)
		}
		if entries, err := os.ReadDir(filepath.Join(root, MachinesSubvolume)); err == nil {
			for _, e := range entries {
				if api.CheckMachineName(e.Name()) == nil && isDir(filepath.Join(root, MachinesSubvolume, e.Name())) {
					machines = append(machines, e.Name())
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("salvage: the machines cannot be listed: %w", err)
		}
	}
	for _, name := range volumes {
		it := volume(name)
		if !isDir(it.src) {
			missing = append(missing, "volume "+name)
			continue
		}
		items = append(items, it)
	}
	for _, name := range machines {
		it := machine(name)
		if !isDir(it.src) {
			missing = append(missing, "machine "+name)
			continue
		}
		items = append(items, it)
	}
	if r.Docker {
		items = append(items, salvageItem{src: filepath.Join(root, "@docker"), dst: filepath.Join(r.Dest, SalvageDockerDir)})
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("salvage: the disk has no %s", strings.Join(missing, ", no "))
	}
	if len(items) == 0 {
		return nil, errors.New("salvage: the disk has no volume and no machine to copy (ask for the engine's data with docker)")
	}
	return items, nil
}

func prepareDest(dest string, items []salvageItem) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("salvage: the destination: %w", err)
	}
	for _, it := range items {
		if _, err := os.Lstat(it.dst); err == nil {
			return fmt.Errorf("salvage: %s is already there; nothing was copied (choose an empty folder)", it.dst)
		}
	}
	return nil
}

func copyItems(ctx context.Context, items []salvageItem, progress func(string, int64, int64)) (api.Salvaged, error) {
	if progress == nil {
		progress = func(string, int64, int64) {}
	}
	var total int64
	for _, it := range items {
		total += treeBytes(it.src)
	}
	c := &copier{ctx: ctx, total: total, progress: progress}
	progress(api.PhaseCopy, 0, total)
	for _, it := range items {
		if err := os.MkdirAll(filepath.Dir(it.dst), 0o755); err != nil {
			return c.result(), fmt.Errorf("salvage: %w", err)
		}
		if err := c.tree(it.src, it.dst); err != nil {
			return c.result(), err
		}
	}
	progress(api.PhaseCopy, c.bytes, total)
	return c.result(), nil
}

func treeBytes(root string) int64 {
	var n int64
	filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

type copier struct {
	ctx      context.Context
	total    int64
	files    int64
	bytes    int64
	errs     errorList
	progress func(string, int64, int64)
	buf      []byte
}

func (c *copier) result() api.Salvaged {
	return api.Salvaged{Method: api.SalvageCopy, Files: c.files, Bytes: c.bytes, Errors: c.errs.lines, Lost: c.errs.n}
}

type destErr struct{ err error }

func (e destErr) Error() string { return "salvage: the destination: " + e.err.Error() }
func (e destErr) Unwrap() error { return e.err }

func (c *copier) tree(src, dst string) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	st, err := os.Lstat(src)
	if err != nil {
		c.errs.add("%s: %v", src, unwrapPath(err))
		return nil
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		return destErr{err}
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		c.errs.add("%s: the directory could not be read whole: %v", src, unwrapPath(err))
	}
	for _, e := range entries {
		if err := c.entry(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), e); err != nil {
			return err
		}
	}
	c.attrs(dst, st)
	return nil
}

func (c *copier) entry(src, dst string, e os.DirEntry) error {
	switch t := e.Type(); {
	case t.IsDir():
		return c.tree(src, dst)
	case t.IsRegular():
		return c.file(src, dst)
	case t&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			c.errs.add("%s: %v", src, unwrapPath(err))
			return nil
		}
		if err := os.Symlink(target, dst); err != nil {
			return destErr{err}
		}
		c.files++
		return nil
	default:
		return nil
	}
}

const copyChunk = 1 << 20

func (c *copier) file(src, dst string) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	in, err := os.OpenFile(src, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		c.errs.add("%s: %v", src, unwrapPath(err))
		return nil
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		c.errs.add("%s: %v", src, unwrapPath(err))
		return nil
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return destErr{err}
	}
	size := st.Size()
	if c.buf == nil {
		c.buf = make([]byte, copyChunk)
	}
	var lost int64
	var firstErr error
	fd := int(in.Fd())
	for off := int64(0); off < size; {
		if err := c.ctx.Err(); err != nil {
			out.Close()
			return err
		}
		data, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			c.bytes += size - off
			break
		}
		if err != nil {
			data = off
		}
		end, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		if err != nil || end > size {
			end = size
		}
		c.bytes += data - off
		for p := data; p < end; {
			n := min(int64(len(c.buf)), end-p)
			got, err := in.ReadAt(c.buf[:n], p)
			if got > 0 {
				if _, err := out.WriteAt(c.buf[:got], p); err != nil {
					out.Close()
					return destErr{err}
				}
			}
			c.bytes += int64(got)
			p += int64(got)
			if err != nil && !errors.Is(err, io.EOF) {
				skip := min(int64(copyChunk), end-p)
				lost += skip
				p += skip
				if firstErr == nil {
					firstErr = unwrapPath(err)
				}
			} else if errors.Is(err, io.EOF) {
				break
			}
			c.progress(api.PhaseCopy, c.bytes, c.total)
		}
		off = end
	}
	if err := out.Truncate(size); err != nil {
		out.Close()
		return destErr{err}
	}
	if err := out.Close(); err != nil {
		return destErr{err}
	}
	c.attrs(dst, st)
	c.files++
	if firstErr != nil {
		c.errs.add("%s: %v, %d bytes not read", src, firstErr, lost)
	}
	return nil
}

func (c *copier) attrs(dst string, st os.FileInfo) {
	os.Chmod(dst, st.Mode().Perm())
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		ts := []unix.Timespec{unix.NsecToTimespec(sys.Atim.Nano()), unix.NsecToTimespec(sys.Mtim.Nano())}
		unix.UtimesNanoAt(unix.AT_FDCWD, dst, ts, unix.AT_SYMLINK_NOFOLLOW)
	}
}

func unwrapPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}

func sortedNames(names []string) []string {
	out := append([]string{}, names...)
	slices.Sort(out)
	return slices.Compact(out)
}
