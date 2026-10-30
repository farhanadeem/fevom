package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	api "fev/proto/agent"
	"fev/proto/fsevents"
	"fev/proto/version"

	"fev/guest/agent"
	"fev/guest/internal/fsnotify"
	"fev/guest/internal/logbuf"
	"fev/guest/internal/stage"
)

const (
	shareTag         = "mac"
	shareMount       = "/mnt/mac"
	shareReadaheadKB = "1024"
)

type fileShare struct {
	log *logbuf.Buffer

	mu      sync.RWMutex
	device  int
	root    int
	mounted bool
	bound   map[string]bool

	helper *fsnotify.Helper

	followMu   sync.Mutex
	followers  map[int]func(api.FileShare)
	nextFollow int
	told       *api.FileShare
}

func newFileShare(log *logbuf.Buffer) *fileShare {
	f := &fileShare{log: log, device: -1, root: -1}
	f.helper = &fsnotify.Helper{
		Build: version.Build,
		Dial: func() (io.ReadWriteCloser, error) {
			return vsock.Dial(vsock.Host, fsevents.HostPort, nil)
		},
		Logf: func(format string, args ...any) { log.Logf(logbuf.Warning, format, args...) },
	}
	f.helper.OnState = f.announce
	return f
}

func (f *fileShare) holdDevice() {
	fd, err := unix.Open(fsevents.Device, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		f.log.Logf(logbuf.Info, "fileshare: no change device (%v): changes made on the Mac raise no events in the guest", err)
		return
	}
	f.mu.Lock()
	f.device = fd
	f.mu.Unlock()
	f.helper.Emit = f.emit
	f.announce()
}

func (f *fileShare) emit(e fsevents.Event) (uint32, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.device < 0 || f.root < 0 {
		return 0, errors.New("the share is not mounted")
	}
	req := e.Ioctl(int32(f.root))
	had, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(f.device), fsevents.IoctlEmit, uintptr(unsafe.Pointer(&req[0])))
	if errno != 0 {
		return 0, errno
	}
	return uint32(had), nil
}

func findTag(tag string) bool {
	tags, _ := filepath.Glob("/sys/fs/virtiofs/*/tag")
	for _, path := range tags {
		if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) == tag {
			return true
		}
	}
	return false
}

func (f *fileShare) start() error {
	f.holdDevice()
	if !findTag(shareTag) {
		return stage.Skip("no file share attached")
	}
	if err := mount(shareTag, shareMount, "virtiofs", unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
		return err
	}
	root, err := unix.Open(shareMount, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", shareMount, err)
	}
	f.mu.Lock()
	f.root, f.mounted = root, true
	f.mu.Unlock()
	var st unix.Stat_t
	if err := unix.Fstat(root, &st); err == nil {
		bdi := fmt.Sprintf("/sys/class/bdi/%d:%d/read_ahead_kb", unix.Major(st.Dev), unix.Minor(st.Dev))
		if err := os.WriteFile(bdi, []byte(shareReadaheadKB), 0); err != nil {
			f.log.Logf(logbuf.Warning, "fileshare: cannot raise the share's readahead: %v", err)
		}
	}
	if err := f.helper.Connect(); err != nil && !errors.Is(err, fsnotify.ErrNoDevice) {
		f.log.Logf(logbuf.Info, "fileshare: no change feed: %v", err)
	}
	f.announce()
	return nil
}

func (f *fileShare) info() *api.FileShare {
	f.mu.RLock()
	mounted, device := f.mounted, f.device >= 0
	var unbound []string
	if f.bound != nil {
		for _, b := range api.SharePaths {
			if !f.bound[b.Target] {
				unbound = append(unbound, b.Target)
			}
		}
	}
	f.mu.RUnlock()
	if !mounted {
		return nil
	}
	s := f.helper.Stats()
	return &api.FileShare{
		Tag: shareTag, Target: shareMount, Device: device,
		Feed: s.Feed, FeedError: s.FeedError, Watching: s.Watching,
		Records: s.Records, Refused: s.Refused, Invalidations: s.Invalidations,
		Unbound: unbound,
	}
}

func (f *fileShare) follow(emit func(api.FileShare)) (stop func()) {
	f.followMu.Lock()
	defer f.followMu.Unlock()
	if f.followers == nil {
		f.followers = map[int]func(api.FileShare){}
	}
	f.nextFollow++
	id := f.nextFollow
	f.followers[id] = emit
	if now := f.info(); now != nil {
		f.told = now
		emit(*now)
	}
	return func() {
		f.followMu.Lock()
		delete(f.followers, id)
		f.followMu.Unlock()
	}
}

func (f *fileShare) announce() {
	f.followMu.Lock()
	defer f.followMu.Unlock()
	if len(f.followers) == 0 {
		return
	}
	now := f.info()
	if now == nil || (f.told != nil && f.told.Device == now.Device && f.told.Feed == now.Feed && f.told.FeedError == now.FeedError) {
		return
	}
	f.told = now
	for _, emit := range f.followers {
		emit(*now)
	}
}

func (f *fileShare) watch(ctx context.Context, paths []string) ([]fsevents.Status, error) {
	f.mu.RLock()
	mounted := f.mounted
	f.mu.RUnlock()
	if !mounted {
		return nil, errors.New("this VM has no file share")
	}
	return f.helper.Watch(ctx, paths)
}

func (f *fileShare) bind(root string) (tmp bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bound = map[string]bool{}
	if !f.mounted {
		return false
	}
	if err := unix.Mount(shareMount, root+api.ShareTarget, "", unix.MS_BIND, ""); err != nil {
		f.log.Logf(logbuf.Warning, "fileshare: cannot bind the share at the engine's %s: %v", api.ShareTarget, err)
	}
	for _, b := range api.SharePaths {
		source := shareMount + b.Source
		if st, err := os.Lstat(source); err != nil || !st.IsDir() {
			continue
		}
		if err := unix.Mount(source, root+b.Target, "", unix.MS_BIND, ""); err != nil {
			f.log.Logf(logbuf.Warning, "fileshare: cannot bind the Mac's %s at the engine's %s: %v", b.Source, b.Target, err)
			continue
		}
		f.bound[b.Target] = true
	}
	return f.bound["/tmp"]
}

var linuxRoot = map[string]bool{
	"bin": true, "boot": true, "dev": true, "etc": true, "home": true, "lib": true, "lib64": true,
	"media": true, "mnt": true, "opt": true, "proc": true, "root": true, "run": true, "sbin": true,
	"srv": true, "sys": true, "tmp": true, "usr": true, "var": true, "data": true,
}

func (f *fileShare) refuseUnmapped(root string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	if f.mounted {
		entries, _ := os.ReadDir(shareMount)
		for _, e := range entries {
			n := "/" + e.Name()
			if linuxRoot[e.Name()] || f.bound[n] || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			names = append(names, n)
		}
	} else {
		for _, b := range api.SharePaths {
			if b.Target != lateTmp {
				names = append(names, b.Target)
			}
		}
	}
	for _, n := range names {
		target := root + n
		if os.MkdirAll(target, 0o755) != nil {
			continue
		}
		if err := unix.Mount("tmpfs", target, "tmpfs", unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "size=4k,mode=0555"); err != nil {
			f.log.Logf(logbuf.Warning, "fileshare: cannot refuse the Mac's %s in the engine's root: %v", n, err)
		}
	}
}

const lateTmp = "/tmp"

func (f *fileShare) bindLate(in func(func() error) error) (bound, unbound []string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.mounted {
		return nil, nil, agent.ErrNoShare
	}
	var want []api.SharePath
	for _, b := range api.SharePaths {
		if f.bound[b.Target] {
			continue
		}
		st, err := os.Lstat(shareMount + b.Source)
		if err != nil || !st.IsDir() || b.Target == lateTmp {
			unbound = append(unbound, b.Target)
			continue
		}
		want = append(want, b)
	}
	if len(want) == 0 {
		return nil, unbound, nil
	}
	failed := map[string]error{}
	err = in(func() error {
		for _, b := range want {
			if err := unix.Mount(api.ShareTarget+b.Source, b.Target, "", unix.MS_BIND, ""); err != nil {
				failed[b.Target] = err
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("the engine's Mac paths cannot be bound now: %w", err)
	}
	wanted := map[string]bool{}
	for _, b := range want {
		wanted[b.Target] = true
	}
	unbound = nil
	for _, b := range api.SharePaths {
		switch {
		case f.bound[b.Target]:
		case wanted[b.Target] && failed[b.Target] == nil:
			f.bound[b.Target] = true
			bound = append(bound, b.Target)
		default:
			if err := failed[b.Target]; err != nil {
				f.log.Logf(logbuf.Warning, "fileshare: cannot bind the Mac's %s at the running engine's %s: %v", b.Source, b.Target, err)
			}
			unbound = append(unbound, b.Target)
		}
	}
	return bound, unbound, nil
}

func (f *fileShare) unmount() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.mounted {
		return nil
	}
	if f.root >= 0 {
		unix.Close(f.root)
		f.root = -1
	}
	if err := unix.Unmount(shareMount, 0); err != nil && !errors.Is(err, unix.EINVAL) {
		unix.Unmount(shareMount, unix.MNT_DETACH)
		return fmt.Errorf("unmount %s: %w; detached", shareMount, err)
	}
	f.mounted = false
	return nil
}
