package machines

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/internal/reaper"
	"fev/guest/internal/session"
)

const (
	isoBase = 100000
	isoSize = 65536
)

var Stage = "/system/bin/fev-isostage"

var Busybox = "/system/engine/bin/busybox"

var isoDevices = map[string]bool{"null": true, "zero": true, "full": true, "random": true, "urandom": true, "tty": true}

var isoMaps = []syscall.SysProcIDMap{{ContainerID: 0, HostID: isoBase, Size: isoSize}}

func (r *run) macFiles() bool { return r.cfg.Share && !r.cfg.Isolated }

func delegateTo(group string, uid, gid int) error {
	for _, p := range []string{"", "cgroup.procs", "cgroup.threads", "cgroup.subtree_control"} {
		if err := os.Chown(group+"/"+p, uid, gid); err != nil && !(p != "" && errors.Is(err, os.ErrNotExist)) {
			return fmt.Errorf("give %s to the machine's user namespace: %w", p, err)
		}
	}
	return nil
}

type mappingNS struct {
	fd    int
	child *reaper.Child
	in    *os.File
}

func newMappingNS(rp *reaper.Reaper) (*mappingNS, error) {
	for _, p := range []string{Busybox, Stage} {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("an isolated machine needs the system image's %s: %w", p, err)
		}
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer pr.Close()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		pw.Close()
		return nil, err
	}
	defer null.Close()
	child, err := rp.Start(Busybox, []string{"cat"}, &os.ProcAttr{
		Dir: "/", Env: []string{}, Files: []*os.File{pr, null, null},
		Sys: &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUSER, UidMappings: isoMaps, GidMappings: isoMaps},
	})
	if err != nil {
		pw.Close()
		return nil, fmt.Errorf("a user namespace for the idmapped root: %w", err)
	}
	fd, err := unix.Open(fmt.Sprintf("/proc/%d/ns/user", child.Pid), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		pw.Close()
		rp.Signal(child, unix.SIGKILL, false)
		child.Wait()
		return nil, err
	}
	return &mappingNS{fd: fd, child: child, in: pw}, nil
}

func (n *mappingNS) close() {
	unix.Close(n.fd)
	n.in.Close()
	n.child.Wait()
}

func rootFor(rp *reaper.Reaper, subvolume, target string, mounts []api.MachineMount) ([]string, error) {
	type named struct {
		tree int
		dest string
	}
	var binds []named
	var said []string
	defer func() {
		for _, b := range binds {
			unix.Close(b.tree)
		}
	}()
	if len(mounts) > 0 {
		trees, s, err := namedTrees(subvolume, mounts)
		if err != nil {
			return nil, err
		}
		said = s
		for i, t := range trees {
			if t >= 0 {
				binds = append(binds, named{t, mountDest(mounts[i])})
			}
		}
	}
	ns, err := newMappingNS(rp)
	if err != nil {
		return nil, err
	}
	defer ns.close()
	tree, err := unix.OpenTree(unix.AT_FDCWD, subvolume, unix.OPEN_TREE_CLONE|unix.O_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("open_tree %s: %w", subvolume, err)
	}
	defer unix.Close(tree)
	attr := &unix.MountAttr{
		Attr_set:  unix.MOUNT_ATTR_IDMAP,
		Attr_clr:  unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV,
		Userns_fd: uint64(ns.fd),
	}
	if err := unix.MountSetattr(tree, "", unix.AT_EMPTY_PATH, attr); err != nil {
		return nil, fmt.Errorf("idmap the machine's root: %w", err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, err
	}
	if err := unix.MoveMount(tree, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return nil, fmt.Errorf("move the machine's root to %s: %w", target, err)
	}
	rootFd, err := unix.Open(target, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Unmount(target, unix.MNT_DETACH)
		return nil, err
	}
	defer unix.Close(rootFd)
	for _, b := range binds {
		dfd, err := unix.Openat2(rootFd, strings.TrimPrefix(b.dest, "/"), &unix.OpenHow{
			Flags:   unix.O_PATH | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
		})
		if err != nil {
			said = append(said, fmt.Sprintf("%s: %v", b.dest, err))
			continue
		}
		err = unix.MoveMount(b.tree, "", dfd, "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH)
		unix.Close(dfd)
		if err != nil {
			said = append(said, fmt.Sprintf("%s: %v", b.dest, err))
		}
	}
	return said, nil
}

func mountDest(m api.MachineMount) string {
	if m.Dest != "" {
		return m.Dest
	}
	return m.Source
}

func namedTrees(subvolume string, mounts []api.MachineMount) ([]int, []string, error) {
	if !shareMounted() {
		return nil, nil, errors.New("the machine names Mac folders (mounts) and this VM has no file share")
	}
	share, err := unix.Open(api.ShareTarget, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	defer unix.Close(share)
	sub, err := unix.Open(subvolume, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	defer unix.Close(sub)
	trees := make([]int, len(mounts))
	var said []string
	for i, m := range mounts {
		trees[i] = -1
		src, err := unix.Openat2(share, strings.TrimPrefix(m.Source, "/"), &unix.OpenHow{
			Flags:   unix.O_PATH | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
		})
		if err != nil {
			said = append(said, fmt.Sprintf("the Mac's %s at %s: %v", m.Source, mountDest(m), err))
			continue
		}
		var st unix.Stat_t
		err = unix.Fstat(src, &st)
		dir := st.Mode&unix.S_IFMT == unix.S_IFDIR
		if err == nil && !dir && st.Mode&unix.S_IFMT != unix.S_IFREG {
			err = errors.New("neither a folder nor a file")
		}
		if err == nil {
			err = makePoint(sub, mountDest(m), dir)
		}
		if err != nil {
			unix.Close(src)
			said = append(said, fmt.Sprintf("the Mac's %s at %s: %v", m.Source, mountDest(m), err))
			continue
		}
		tree, err := unix.OpenTree(src, "", unix.OPEN_TREE_CLONE|unix.O_CLOEXEC|unix.AT_EMPTY_PATH|unix.AT_RECURSIVE)
		unix.Close(src)
		if err != nil {
			said = append(said, fmt.Sprintf("the Mac's %s at %s: open_tree: %v", m.Source, mountDest(m), err))
			continue
		}
		trees[i] = tree
	}
	return trees, said, nil
}

func makePoint(root int, dest string, dir bool) error {
	parts := strings.Split(strings.Trim(path.Clean(dest), "/"), "/")
	cur, err := unix.Dup(root)
	if err != nil {
		return err
	}
	defer func() { unix.Close(cur) }()
	for i, p := range parts {
		last := i == len(parts)-1
		if last && !dir {
			fd, err := unix.Openat(cur, p, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if errors.Is(err, unix.ENOENT) {
				fd, err = unix.Openat(cur, p, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
			}
			if err != nil {
				return err
			}
			var st unix.Stat_t
			err = unix.Fstat(fd, &st)
			unix.Close(fd)
			if err == nil && st.Mode&unix.S_IFMT != unix.S_IFREG {
				err = fmt.Errorf("%s is there and not a file", dest)
			}
			return err
		}
		next, err := unix.Openat(cur, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			if err := unix.Mkdirat(cur, p, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
				return err
			}
			next, err = unix.Openat(cur, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", "/"+strings.Join(parts[:i+1], "/"), err)
		}
		unix.Close(cur)
		cur = next
	}
	return nil
}

func (b *boot) startIsolated(rp *reaper.Reaper, self, prog string, argv, env []string, files [3]*os.File, sys *syscall.SysProcAttr) (*reaper.Child, error) {
	user, err := b.nsFile("user")
	if err != nil {
		return nil, err
	}
	defer user.Close()
	mnt, err := b.nsFile("mnt")
	if err != nil {
		return nil, err
	}
	defer mnt.Close()
	cg, err := b.nsFile("cgroup")
	if err != nil {
		return nil, err
	}
	defer cg.Close()
	p := session.Proc{
		Path: prog, Argv: argv, Env: env, Files: files, Sys: sys,
		MountNS: mnt, CgroupNS: cg, UserNS: user, Stage: Stage, Isolate: true,
	}
	path, args, attr, release, err := p.Attr(self)
	if err != nil {
		return nil, err
	}
	defer release()
	return rp.Start(path, args, attr)
}
