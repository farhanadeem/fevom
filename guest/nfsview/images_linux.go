package nfsview

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

type LinuxImages struct {
	NS         *Namespaces
	Ctr        *Containerd
	From, Root string
	Logf       func(string, ...any)

	once    sync.Once
	initErr error
	pipeR   *os.File
	pipeW   *os.File
	control *os.File

	mu      sync.Mutex
	stopped bool
	byDev   map[uint32]*lazyImage
	byPath  map[string]*lazyImage
}

type lazyImage struct {
	path  string
	img   ImageSpec
	dev   uint32
	fd    int
	key   string
	made  bool
	busy  bool
	gone  bool
	viewd bool
}

const (
	autofsDevIoctlOpenMount = 0xC0189374
	autofsIocReady          = 0x9360
	autofsIocFail           = 0x9361
	autofsIocCatatonic      = 0x9362
	autofsDevIoctlSize      = 24
	autofsPtypeMissingDir   = 5
	autofsPacketSize        = 304
)

func (l *LinuxImages) logf(format string, args ...any) {
	if l.Logf != nil {
		l.Logf(format, args...)
	}
}

func (l *LinuxImages) init() error {
	l.once.Do(func() {
		l.byDev = map[uint32]*lazyImage{}
		l.byPath = map[string]*lazyImage{}
		l.control, l.initErr = os.OpenFile("/dev/autofs", os.O_RDONLY, 0)
		if l.initErr != nil {
			return
		}
		l.pipeR, l.pipeW, l.initErr = os.Pipe()
		if l.initErr != nil {
			return
		}
		l.sweep()
		go l.serve()
	})
	return l.initErr
}

func encodeDev(major, minor uint32) uint32 {
	return minor&0xff | major<<8 | (minor&^0xff)<<12
}

func (l *LinuxImages) Prepare(path string, img ImageSpec) error {
	if err := l.init(); err != nil {
		return err
	}
	li := &lazyImage{path: path, img: img, fd: -1}
	h := sha256.Sum256([]byte(img.Key()))
	li.key = "fev-view-" + hex.EncodeToString(h[:8])
	err := l.NS.Do(func() error {
		opts := fmt.Sprintf("fd=%d,pgrp=%d,minproto=5,maxproto=5,direct", l.pipeW.Fd(), unix.Gettid())
		if err := unix.Mount("fev-image", path, "autofs", unix.MS_NOSUID|unix.MS_NODEV, opts); err != nil {
			return fmt.Errorf("nfsview: autofs at %s: %w", path, err)
		}
		attr := unix.MountAttr{Propagation: unix.MS_UNBINDABLE}
		if err := unix.MountSetattr(unix.AT_FDCWD, path, unix.AT_NO_AUTOMOUNT, &attr); err != nil {
			unix.Unmount(path, unix.MNT_DETACH)
			return fmt.Errorf("nfsview: autofs at %s unbindable: %w", path, err)
		}
		var st unix.Statx_t
		if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_NO_AUTOMOUNT|unix.AT_SYMLINK_NOFOLLOW, 0, &st); err != nil {
			unix.Unmount(path, unix.MNT_DETACH)
			return err
		}
		li.dev = encodeDev(st.Dev_major, st.Dev_minor)
		fd, err := openMount(int(l.control.Fd()), path, li.dev)
		if err != nil {
			unix.Unmount(path, unix.MNT_DETACH)
			return fmt.Errorf("nfsview: the autofs mount at %s: %w", path, err)
		}
		li.fd = fd
		return nil
	})
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.byDev[li.dev] = li
	l.byPath[path] = li
	l.mu.Unlock()
	return nil
}

func openMount(control int, path string, dev uint32) (int, error) {
	buf := make([]byte, autofsDevIoctlSize+len(path)+1)
	binary.NativeEndian.PutUint32(buf[0:], 1)
	binary.NativeEndian.PutUint32(buf[4:], 0)
	binary.NativeEndian.PutUint32(buf[8:], uint32(len(buf)))
	binary.NativeEndian.PutUint32(buf[12:], ^uint32(0))
	binary.NativeEndian.PutUint32(buf[16:], dev)
	copy(buf[autofsDevIoctlSize:], path)
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(control), autofsDevIoctlOpenMount, uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
		return -1, e
	}
	fd := int(int32(binary.NativeEndian.Uint32(buf[12:])))
	unix.CloseOnExec(fd)
	return fd, nil
}

func rootIoctl(fd int, cmd uintptr, arg uintptr) error {
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), cmd, arg); e != 0 {
		return e
	}
	return nil
}

func (l *LinuxImages) serve() {
	buf := make([]byte, 512)
	for {
		n, err := l.pipeR.Read(buf)
		if err != nil {
			if !errors.Is(err, os.ErrClosed) {
				l.logf("images: the autofs pipe: %v", err)
			}
			return
		}
		if n < 44 {
			continue
		}
		typ := int32(binary.NativeEndian.Uint32(buf[4:]))
		token := binary.NativeEndian.Uint32(buf[8:])
		dev := binary.NativeEndian.Uint32(buf[12:])
		l.mu.Lock()
		li := l.byDev[dev]
		l.mu.Unlock()
		if li == nil {
			continue
		}
		if typ != autofsPtypeMissingDir {
			rootIoctl(li.fd, autofsIocFail, uintptr(token))
			continue
		}
		go l.make(li, token)
	}
}

func (l *LinuxImages) make(li *lazyImage, token uint32) {
	l.mu.Lock()
	if l.stopped || li.gone || li.busy {
		stopped := l.stopped || li.gone
		l.mu.Unlock()
		if stopped {
			rootIoctl(li.fd, autofsIocFail, uintptr(token))
		}
		return
	}
	if li.made {
		l.mu.Unlock()
		rootIoctl(li.fd, autofsIocReady, uintptr(token))
		return
	}
	li.busy = true
	l.mu.Unlock()
	err := l.mount(li)
	l.mu.Lock()
	li.busy = false
	li.made = err == nil
	l.mu.Unlock()
	if err != nil {
		l.logf("image %s: %v", li.img.Name, err)
		rootIoctl(li.fd, autofsIocFail, uintptr(token))
		return
	}
	rootIoctl(li.fd, autofsIocReady, uintptr(token))
}

func (l *LinuxImages) mount(li *lazyImage) error {
	ctx := context.Background()
	if err := l.Ctr.CreateLease(ctx, li.key); err != nil && !IsAlreadyExists(err) {
		return err
	}
	mounts, err := l.Ctr.View(ctx, li.key, li.key, ChainID(li.img.DiffIDs))
	if IsAlreadyExists(err) {
		mounts, err = l.Ctr.Mounts(ctx, li.key)
	}
	if err != nil {
		l.Ctr.DeleteLease(ctx, li.key)
		return err
	}
	li.viewd = true
	layers, err := ViewLayers(mounts, l.From, l.Root)
	if err != nil {
		return err
	}
	return l.NS.Do(func() error { return mountLayers(layers, li.fd) })
}

func mountLayers(layers []string, at int) error {
	var mfd int
	if len(layers) == 1 {
		fd, err := unix.OpenTree(unix.AT_FDCWD, layers[0], unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
		if err != nil {
			return fmt.Errorf("nfsview: open_tree %s: %w", layers[0], err)
		}
		attr := unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY, Propagation: unix.MS_PRIVATE}
		if err := unix.MountSetattr(fd, "", unix.AT_EMPTY_PATH, &attr); err != nil {
			unix.Close(fd)
			return err
		}
		mfd = fd
	} else {
		fs, err := unix.Fsopen("overlay", unix.FSOPEN_CLOEXEC)
		if err != nil {
			return fmt.Errorf("nfsview: fsopen overlay: %w", err)
		}
		defer unix.Close(fs)
		var opts [][2]string
		for _, layer := range layers {
			opts = append(opts, [2]string{"lowerdir+", layer})
		}
		opts = append(opts, [2]string{"index", "on"}, [2]string{"redirect_dir", "nofollow"}, [2]string{"nfs_export", "on"})
		for _, kv := range opts {
			if err := unix.FsconfigSetString(fs, kv[0], kv[1]); err != nil {
				return fmt.Errorf("nfsview: overlay %s=%s: %w", kv[0], kv[1], err)
			}
		}
		if err := unix.FsconfigCreate(fs); err != nil {
			return fmt.Errorf("nfsview: the image's overlay: %w", err)
		}
		mfd, err = unix.Fsmount(fs, unix.FSMOUNT_CLOEXEC, unix.MOUNT_ATTR_RDONLY|unix.MOUNT_ATTR_NOSUID|unix.MOUNT_ATTR_NODEV)
		if err != nil {
			return fmt.Errorf("nfsview: fsmount the image's overlay: %w", err)
		}
	}
	defer unix.Close(mfd)
	if err := unix.MoveMount(mfd, "", at, "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); err != nil {
		return fmt.Errorf("nfsview: mount the image's view: %w", err)
	}
	return nil
}

func (l *LinuxImages) Made(path string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	li := l.byPath[path]
	return li != nil && li.made
}

func (l *LinuxImages) Release(path string) error {
	l.mu.Lock()
	li := l.byPath[path]
	if li != nil {
		li.gone = true
		delete(l.byPath, path)
		delete(l.byDev, li.dev)
	}
	l.mu.Unlock()
	if li == nil {
		return nil
	}
	var errs []error
	if li.fd >= 0 {
		rootIoctl(li.fd, autofsIocCatatonic, 0)
	}
	errs = append(errs, l.NS.Do(func() error { return releaseHere(path, false) }))
	if li.fd >= 0 {
		unix.Close(li.fd)
	}
	if li.viewd {
		ctx := context.Background()
		if err := l.Ctr.Remove(ctx, li.key); err != nil && !IsNotFound(err) {
			errs = append(errs, err)
		}
		if err := l.Ctr.DeleteLease(ctx, li.key); err != nil && !IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (l *LinuxImages) Stop() {
	l.mu.Lock()
	l.stopped = true
	var all []*lazyImage
	for _, li := range l.byPath {
		all = append(all, li)
	}
	l.mu.Unlock()
	for _, li := range all {
		if li.fd >= 0 {
			rootIoctl(li.fd, autofsIocCatatonic, 0)
		}
	}
}

func (l *LinuxImages) Close() {
	l.mu.Lock()
	l.stopped = true
	var all []*lazyImage
	for p, li := range l.byPath {
		all = append(all, li)
		delete(l.byPath, p)
		delete(l.byDev, li.dev)
	}
	l.mu.Unlock()
	ctx := context.Background()
	for _, li := range all {
		if li.fd >= 0 {
			unix.Close(li.fd)
		}
		if li.viewd {
			if err := l.Ctr.Remove(ctx, li.key); err != nil && !IsNotFound(err) {
				l.logf("image %s: %v", li.img.Name, err)
			}
			if err := l.Ctr.DeleteLease(ctx, li.key); err != nil && !IsNotFound(err) {
				l.logf("image %s: %v", li.img.Name, err)
			}
		}
	}
	if l.pipeR != nil {
		l.pipeR.Close()
		l.pipeW.Close()
	}
	if l.control != nil {
		l.control.Close()
	}
}

func (l *LinuxImages) sweep() {
	ctx := context.Background()
	ids, err := l.Ctr.Leases(ctx)
	if err != nil {
		l.logf("images: the leases: %v", err)
		return
	}
	for _, id := range ids {
		if !strings.HasPrefix(id, "fev-view-") {
			continue
		}
		if err := l.Ctr.Remove(ctx, id); err != nil && !IsNotFound(err) {
			l.logf("images: %s: %v", id, err)
		}
		if err := l.Ctr.DeleteLease(ctx, id); err != nil && !IsNotFound(err) {
			l.logf("images: %s: %v", id, err)
		}
	}
}
