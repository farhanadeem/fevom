package nfsview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

const Options = "vers=4.0,soft,intr,nocallback,timeo=10,retrans=10,deadtimeout=0,mutejukebox,nfc," +
	"rsize=131072,wsize=131072,readahead=64,noatime"

var (
	mountNFS = "/sbin/mount_nfs"
	mountCmd = "/sbin/mount"
)

const ServerName = "Fevom"

type Endpoint struct {
	Port   int
	Socket string
}

func (e Endpoint) String() string {
	if e.Port != 0 {
		return fmt.Sprintf("127.0.0.1:%d", e.Port)
	}
	return e.Socket
}

func Source(e Endpoint) string {
	if e.Port != 0 {
		return ServerName + ":/"
	}
	return "<" + e.Socket + ">:/"
}

func socketOf(from string) string {
	if !strings.HasPrefix(from, "<") || !strings.HasSuffix(from, ">:/") {
		return ""
	}
	return from[1 : len(from)-3]
}

func MountArgs(e Endpoint, dir string, hidden bool) []string {
	o := Options
	if e.Port != 0 {
		o += fmt.Sprintf(",localhost,port=%d", e.Port)
	}
	if hidden {
		o += ",nobrowse"
	}
	return []string{"-o", o, Source(e), dir}
}

func Mount(ctx context.Context, e Endpoint, dir string, hidden bool) error {
	cmd := exec.CommandContext(ctx, mountNFS, MountArgs(e, dir, hidden)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount_nfs %s (%s) on %s: %v: %s", Source(e), e, dir, err, bytes.TrimSpace(out))
	}
	return nil
}

func SetHidden(ctx context.Context, e Endpoint, dir string, hidden bool) error {
	o := "noatime,browse"
	if hidden {
		o = "noatime,nobrowse"
	}
	if e.Port != 0 {
		o += fmt.Sprintf(",localhost,port=%d", e.Port)
	}
	out, err := exec.CommandContext(ctx, mountCmd, "-u", "-o", o, dir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount -u -o %s %s: %v: %s", o, dir, err, bytes.TrimSpace(out))
	}
	return nil
}

func Unmount(dir string) (forced bool, err error) {
	forced, left := unmountMirrors(dir)
	err = unmount(dir, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EINVAL), errors.Is(err, syscall.ENOENT):
		return forced, left
	case errors.Is(err, syscall.EBUSY):
		return true, ForceUnmount(dir)
	}
	return forced, errors.Join(left, fmt.Errorf("unmount %s: %w", dir, err))
}

func ForceUnmount(dir string) error {
	err := unmount(dir, unix.MNT_FORCE)
	if err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("unmount -f %s: %w", dir, err)
	}
	return leftUnder(dir)
}

var unmount = unix.Unmount

func mirrors(dir string) ([][]string, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(filepath.Clean(dir)))
	if err != nil {
		return nil, nil
	}
	table, err := mountTable()
	if err != nil {
		return nil, fmt.Errorf("the mount table: %w", err)
	}
	view := filepath.Join(parent, filepath.Base(filepath.Clean(dir)))
	levels := map[int][]string{}
	for i := range table {
		if on, ok := isMirror(&table[i], view); ok {
			d := strings.Count(on, "/")
			levels[d] = append(levels[d], on)
		}
	}
	depths := make([]int, 0, len(levels))
	for d := range levels {
		depths = append(depths, d)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(depths)))
	out := make([][]string, 0, len(depths))
	for _, d := range depths {
		out = append(out, levels[d])
	}
	return out, nil
}

func unmountMirrors(dir string) (forced bool, left error) {
	levels, err := mirrors(dir)
	if err != nil {
		return false, err
	}
	var (
		mu   sync.Mutex
		errs []error
	)
	one := func(on string) {
		err := unmount(on, 0)
		if err == nil || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOENT) {
			return
		}
		if !errors.Is(err, syscall.EBUSY) {
			mu.Lock()
			errs = append(errs, fmt.Errorf("unmount %s: %w", on, err))
			mu.Unlock()
			return
		}
		err = unmount(on, unix.MNT_FORCE)
		mu.Lock()
		defer mu.Unlock()
		forced = true
		if err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOENT) {
			errs = append(errs, fmt.Errorf("unmount -f %s: %w", on, err))
		}
	}
	for _, level := range levels {
		var wg sync.WaitGroup
		for _, on := range level {
			wg.Add(1)
			go func() {
				defer wg.Done()
				one(on)
			}()
		}
		wg.Wait()
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return forced, errors.Join(errs...)
}

func leftUnder(dir string) error {
	levels, err := mirrors(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, level := range levels {
		for _, on := range level {
			switch err := unmount(on, unix.MNT_FORCE); {
			case err == nil:
			case errors.Is(err, syscall.EINVAL), errors.Is(err, syscall.ENOENT):
				errs = append(errs, fmt.Errorf("%s is still mounted, its path gone with the view's unmount (macOS unmounts only by path); the Mac's client drops it by itself", on))
			default:
				errs = append(errs, fmt.Errorf("unmount -f %s: %w", on, err))
			}
		}
	}
	return errors.Join(errs...)
}

type Mounted struct {
	From  string
	Type  string
	Owner uint32
	Flags uint32
}

func (m Mounted) View() bool {
	return m.Type == "nfs" && (m.From == ServerName+":/" || socketOf(m.From) != "")
}

func (m Mounted) Hidden() bool { return m.Flags&unix.MNT_DONTBROWSE != 0 }

func Find(dir string) (Mounted, bool, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(filepath.Clean(dir)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Mounted{}, false, nil
		}
		return Mounted{}, false, err
	}
	real := filepath.Join(parent, filepath.Base(filepath.Clean(dir)))
	var buf []unix.Statfs_t
	for {
		n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
		if err != nil {
			return Mounted{}, false, fmt.Errorf("getfsstat: %w", err)
		}
		buf = make([]unix.Statfs_t, n+4)
		got, err := unix.Getfsstat(buf, unix.MNT_NOWAIT)
		if err != nil {
			return Mounted{}, false, fmt.Errorf("getfsstat: %w", err)
		}
		if got < len(buf) {
			buf = buf[:got]
			break
		}
	}
	for i := len(buf) - 1; i >= 0; i-- {
		s := &buf[i]
		if unix.ByteSliceToString(s.Mntonname[:]) != real {
			continue
		}
		return Mounted{
			From:  unix.ByteSliceToString(s.Mntfromname[:]),
			Type:  unix.ByteSliceToString(s.Fstypename[:]),
			Owner: s.Owner,
			Flags: s.Flags,
		}, true, nil
	}
	return Mounted{}, false, nil
}

var ErrForeignMount = errors.New("something other than fev's view is mounted there")

func Recover(dir string) (removed bool, err error) {
	m, ok, err := Find(dir)
	if err != nil || !ok {
		return false, err
	}
	if !m.View() || m.Owner != uint32(os.Getuid()) {
		return false, fmt.Errorf("%s: %w (%s, %s)", dir, ErrForeignMount, m.Type, m.From)
	}
	if err := ForceUnmount(dir); err != nil {
		return false, err
	}
	return true, nil
}
