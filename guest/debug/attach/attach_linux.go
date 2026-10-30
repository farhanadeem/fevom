//go:build linux

package attach

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

func Run(e DebugEntry) {
	runtime.LockOSThread()
	name := e.Name
	if name == "" {
		name = "fev"
	}
	fail := func(code int, format string, args ...any) {
		fmt.Fprintf(os.Stderr, name+": "+format+"\n", args...)
		os.Exit(code)
	}

	if e.Kind == DebugKindRunning && e.TargetNSPid > 0 {
		if e.DumpLock != "" {
			if fd, err := unix.Open(e.DumpLock, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600); err == nil {
				for unix.Flock(fd, unix.LOCK_EX) == unix.EINTR {
				}
				defer unix.Close(fd)
			}
		}
		pid, err := findTarget("/proc", e.TargetPidNS, e.TargetNSPid, e.TargetStart)
		if err != nil {
			fail(126, "the target's seccomp filters: %v", err)
		}
		layers, err := DumpTargetStack(pid)
		if err != nil {
			fail(126, "the target's seccomp filters: %v", err)
		}
		e.Spec.Seccomp = nil
		for _, l := range layers {
			e.Spec.Seccomp = append(e.Spec.Seccomp, WithPrefix(e.Prefix, l))
		}
	}

	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		fail(126, "give the thread its own filesystem context: %v", err)
	}

	switch e.Kind {
	case DebugKindRunning:
		if err := unix.Setns(e.MountFd, unix.CLONE_NEWNS); err != nil {
			fail(126, "enter the target's mount namespace: %v", err)
		}
	}
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		fail(126, "unshare the mount namespace: %v", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		fail(126, "make the mount namespace a slave: %v", err)
	}

	from := "/"
	if e.Kind != DebugKindRunning {
		m, err := overlayRoot(e.Lowers, e.Upper, e.Work)
		if err != nil {
			fail(126, "the target's overlay root: %v", err)
		}
		from = m
	}

	root, err := newTmpfs()
	if err != nil {
		fail(126, "the session root: %v", err)
	}
	if err := mirrorRoot(root, from, "nix"); err != nil {
		fail(126, "%v", err)
	}
	finishNix, ownNix, err := buildNix(root, e, from)
	if err != nil {
		fail(126, "%v", err)
	}
	if e.Kind != DebugKindRunning {
		for _, d := range []string{"dev", "sys", "proc"} {
			unix.Mkdirat(root, d, 0o755)
		}
		if err := freshDev(root, e.DevPtsFd); err != nil {
			fail(126, "the session's /dev: %v", err)
		}
	}
	if err := mountSetattrPath(root, unix.MOUNT_ATTR_RDONLY, false); err != nil {
		fail(126, "the session root read-only: %v", err)
	}
	if err := becomeRoot(root); err != nil {
		fail(126, "%v", err)
	}
	if err := finishNix(); err != nil {
		fail(126, "%v", err)
	}
	if ownNix {
		fmt.Fprintf(os.Stderr, "%s: the target has a /nix of its own; fev's tool store is joined to it read-only\n", name)
	}

	switch e.Kind {
	case DebugKindRunning:
		joins := []struct {
			fd   int
			flag int
			name string
		}{
			{e.NetFd, unix.CLONE_NEWNET, "net"},
			{e.IPCFd, unix.CLONE_NEWIPC, "ipc"},
			{e.UTSFd, unix.CLONE_NEWUTS, "uts"},
			{e.CgroupNSFd, unix.CLONE_NEWCGROUP, "cgroup"},
			{e.PIDFd, unix.CLONE_NEWPID, "pid"},
		}
		for _, j := range joins {
			if j.fd < 0 {
				continue
			}
			if err := unix.Setns(j.fd, j.flag); err != nil {
				fail(126, "enter the target's %s namespace: %v", j.name, err)
			}
		}
		if e.CgroupProcsFd >= 0 {
			if err := joinCgroup(e.CgroupProcsFd); err != nil {
				fail(126, "join the target's cgroup: %v", err)
			}
		}
	default:
		if err := unix.Unshare(unix.CLONE_NEWPID | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS | unix.CLONE_NEWNET); err != nil {
			fail(126, "unshare the session's namespaces: %v", err)
		}
		if e.DevCgroupProcsFd >= 0 {
			if err := joinCgroup(e.DevCgroupProcsFd); err != nil {
				fail(126, "join the deny-all device cgroup: %v", err)
			}
		}
		if err := unix.Unshare(unix.CLONE_NEWCGROUP); err != nil {
			fail(126, "unshare the cgroup namespace: %v", err)
		}
	}

	prog := e.Spec.Argv[0]
	if !strings.Contains(prog, "/") {
		p, err := lookPath(prog, e.Env)
		if err != nil {
			fail(127, "%v", err)
		}
		prog = p
	}
	e.Spec.Argv[0] = prog
	e.Spec.Env = e.Env
	e.Spec.Dir = chooseDir(e.Dir, e.Home)
	if e.Kind == DebugKindRunning && e.UserFd >= 0 {
		e.Spec.Flags |= SpecFlagJoinUser
	}
	if e.Kind != DebugKindRunning {
		e.Spec.Flags |= SpecFlagFreshNS
	}

	execStage(name, e, fail)
}

func execStage(name string, e DebugEntry, fail func(int, string, ...any)) {
	b, err := e.Spec.Encode()
	if err != nil {
		fail(126, "the stage spec: %v", err)
	}
	const stageHoldFd, userHoldFd = 200, 201
	origStage, origUser := e.StageFd, e.UserFd
	if err := dupTo(origStage, stageHoldFd); err != nil {
		fail(126, "hold the stage descriptor: %v", err)
	}
	e.StageFd = stageHoldFd
	if origStage != stageHoldFd {
		unix.Close(origStage)
	}
	userFd := -1
	if e.Spec.Flags&SpecFlagJoinUser != 0 {
		if origUser < 0 {
			fail(126, "a user-namespace join with no user namespace")
		}
		if err := dupTo(origUser, userHoldFd); err != nil {
			fail(126, "hold the user-namespace descriptor: %v", err)
		}
		userFd = userHoldFd
		if origUser != userHoldFd {
			unix.Close(origUser)
		}
	}

	mfd, err := unix.MemfdCreate("fev-dbgstage-spec", 0)
	if err != nil {
		fail(126, "the stage spec: %v", err)
	}
	if _, err := unix.Write(mfd, b); err != nil {
		fail(126, "the stage spec: %v", err)
	}
	if _, err := unix.Seek(mfd, 0, 0); err != nil {
		fail(126, "the stage spec: %v", err)
	}
	if mfd != StageSpecFd {
		if err := unix.Dup3(mfd, StageSpecFd, 0); err != nil {
			fail(126, "the stage spec descriptor: %v", err)
		}
		unix.Close(mfd)
	}
	clearCloexec(StageSpecFd)
	if e.Spec.Flags&SpecFlagJoinUser != 0 {
		if err := unix.Dup3(userFd, StageUserFd, 0); err != nil {
			fail(126, "the user-namespace descriptor: %v", err)
		}
		clearCloexec(StageUserFd)
	}
	unix.CloseRange(StageSpecFd+2, ^uint(0), unix.CLOSE_RANGE_CLOEXEC)
	argv := []string{"fev-dbgstage"}
	err = execveat(e.StageFd, argv, nil)
	fail(126, "run the debug stage: %v", err)
}

func dupTo(fd, want int) error {
	if fd == want {
		return nil
	}
	return unix.Dup3(fd, want, unix.O_CLOEXEC)
}

func execveat(fd int, argv, env []string) error {
	argvp, err := stringSlicePtr(argv)
	if err != nil {
		return err
	}
	envp, err := stringSlicePtr(env)
	if err != nil {
		return err
	}
	empty, err := unix.BytePtrFromString("")
	if err != nil {
		return err
	}
	_, _, errno := unix.Syscall6(unix.SYS_EXECVEAT, uintptr(fd),
		uintptr(unsafe.Pointer(empty)), uintptr(unsafe.Pointer(&argvp[0])),
		uintptr(unsafe.Pointer(&envp[0])), uintptr(unix.AT_EMPTY_PATH), 0)
	return errno
}

func stringSlicePtr(ss []string) ([]*byte, error) {
	out := make([]*byte, 0, len(ss)+1)
	for _, s := range ss {
		p, err := unix.BytePtrFromString(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	out = append(out, nil)
	return out, nil
}

func newTmpfs() (int, error) {
	fs, err := unix.Fsopen("tmpfs", unix.FSOPEN_CLOEXEC)
	if err != nil {
		return -1, fmt.Errorf("fsopen tmpfs: %w", err)
	}
	defer unix.Close(fs)
	if err := unix.FsconfigSetString(fs, "mode", "0755"); err != nil {
		return -1, fmt.Errorf("fsconfig mode: %w", err)
	}
	if err := unix.FsconfigCreate(fs); err != nil {
		return -1, fmt.Errorf("fsconfig create tmpfs: %w", err)
	}
	m, err := unix.Fsmount(fs, unix.FSMOUNT_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("fsmount tmpfs: %w", err)
	}
	return m, nil
}

func moveInto(tree, rootfd int, name string) error {
	if err := unix.MoveMount(tree, "", rootfd, name, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount onto %s: %w", name, err)
	}
	unix.Close(tree)
	return nil
}

func bindInto(rootfd int, src, name string, isDir bool) error {
	if isDir {
		if err := unix.Mkdirat(rootfd, name, 0o755); err != nil && err != unix.EEXIST {
			return fmt.Errorf("mkdir %s: %w", name, err)
		}
	} else {
		fd, err := unix.Openat(rootfd, name, unix.O_CREAT|unix.O_WRONLY|unix.O_CLOEXEC, 0o644)
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		unix.Close(fd)
	}
	t, err := unix.OpenTree(unix.AT_FDCWD, src, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|uint(unix.AT_RECURSIVE))
	if err != nil {
		return fmt.Errorf("open_tree %s: %w", src, err)
	}
	return moveInto(t, rootfd, name)
}

func mirrorRoot(rootfd int, from, skip string) error {
	d, err := os.Open(from)
	if err != nil {
		return fmt.Errorf("open %s: %w", from, err)
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("read %s: %w", from, err)
	}
	base := from
	if base == "/" {
		base = ""
	}
	for _, n := range names {
		if n == skip {
			continue
		}
		src := base + "/" + n
		var st unix.Stat_t
		if err := unix.Lstat(src, &st); err != nil {
			continue
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			buf := make([]byte, 4096)
			k, err := unix.Readlink(src, buf)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", src, err)
			}
			if err := unix.Symlinkat(string(buf[:k]), rootfd, n); err != nil && err != unix.EEXIST {
				return fmt.Errorf("symlink %s: %w", n, err)
			}
		case unix.S_IFDIR:
			if err := bindInto(rootfd, src, n, true); err != nil {
				return err
			}
		case unix.S_IFREG:
			if err := bindInto(rootfd, src, n, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func buildNix(rootfd int, e DebugEntry, from string) (finish func() error, ownNix bool, err error) {
	if err := unix.Mkdirat(rootfd, "nix", 0o755); err != nil && err != unix.EEXIST {
		return nil, false, fmt.Errorf("mkdir /nix: %w", err)
	}
	nixfs, err := newTmpfs()
	if err != nil {
		return nil, false, fmt.Errorf("the /nix tmpfs: %w", err)
	}
	if err := moveInto(nixfs, rootfd, "nix"); err != nil {
		return nil, false, err
	}
	nix, err := unix.Openat(rootfd, "nix", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false, fmt.Errorf("open /nix: %w", err)
	}
	mk := func(path string) error {
		cur := nix
		for _, part := range strings.Split(path, "/") {
			if part == "" {
				continue
			}
			unix.Mkdirat(cur, part, 0o755)
			next, err := unix.Openat(cur, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if cur != nix {
				unix.Close(cur)
			}
			if err != nil {
				return err
			}
			cur = next
		}
		if cur != nix {
			unix.Close(cur)
		}
		return nil
	}
	mkfile := func(rel string) error {
		fd, err := unix.Openat(nix, rel, unix.O_CREAT|unix.O_WRONLY|unix.O_CLOEXEC, 0o644)
		if err != nil {
			return err
		}
		return unix.Close(fd)
	}
	fail := func(err error) (func() error, bool, error) {
		unix.Close(nix)
		return nil, false, err
	}
	for _, p := range []string{"store", "var/fev/profile", "var/fev/etc", "var/fev/bin", "var/fev/home", "var/fev/run"} {
		if err := mk(p); err != nil {
			return fail(fmt.Errorf("mkdir /nix/%s: %w", p, err))
		}
	}
	ownBase := strings.TrimSuffix(from, "/") + "/nix"
	var st unix.Stat_t
	ownNix = unix.Lstat(ownBase, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR
	ownStore := false
	if ownNix {
		ownStore = unix.Lstat(ownBase+"/store", &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR
	}
	storeAt := "store"
	if ownStore {
		storeAt = ".fev-store"
		for _, d := range []string{".fev-store", ".target-store"} {
			if err := mk(d); err != nil {
				return fail(err)
			}
		}
		t, err := unix.OpenTree(unix.AT_FDCWD, ownBase+"/store", unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|uint(unix.AT_RECURSIVE))
		if err != nil {
			return fail(fmt.Errorf("the target's /nix/store: %w", err))
		}
		if err := moveTreeAt(t, nix, ".target-store"); err != nil {
			return fail(err)
		}
	}
	if err := moveTreeAt(e.StoreTreeFd, nix, storeAt); err != nil {
		return fail(err)
	}
	if ownNix {
		for _, sub := range []string{"", "var"} {
			dir := ownBase
			if sub != "" {
				dir += "/" + sub
			}
			d, err := os.Open(dir)
			if err != nil {
				continue
			}
			names, _ := d.Readdirnames(-1)
			d.Close()
			for _, n := range names {
				if (sub == "" && (n == "store" || n == "var")) || (sub == "var" && n == "fev") || strings.HasPrefix(n, ".") {
					continue
				}
				src := dir + "/" + n
				if unix.Lstat(src, &st) != nil {
					continue
				}
				rel := n
				if sub != "" {
					rel = sub + "/" + n
				}
				switch st.Mode & unix.S_IFMT {
				case unix.S_IFDIR:
					if err := bindInto(nix, src, rel, true); err != nil {
						return fail(err)
					}
				case unix.S_IFREG:
					if err := bindInto(nix, src, rel, false); err != nil {
						return fail(err)
					}
				case unix.S_IFLNK:
					buf := make([]byte, 4096)
					if k, err := unix.Readlink(src, buf); err == nil {
						unix.Symlinkat(string(buf[:k]), nix, rel)
					}
				}
			}
		}
	}
	if e.ProfileTreeFd >= 0 {
		if err := moveTreeAt(e.ProfileTreeFd, nix, "var/fev/profile"); err != nil {
			return fail(err)
		}
	}
	if e.EtcTreeFd >= 0 {
		if err := moveTreeAt(e.EtcTreeFd, nix, "var/fev/etc"); err != nil {
			return fail(err)
		}
	}
	if e.GalleyTreeFd >= 0 {
		if err := mkfile("var/fev/bin/galley"); err != nil {
			return fail(err)
		}
		if err := moveTreeAt(e.GalleyTreeFd, nix, "var/fev/bin/galley"); err != nil {
			return fail(err)
		}
	}
	if e.SocketTreeFd >= 0 {
		if err := mkfile("var/fev/run/store.sock"); err != nil {
			return fail(err)
		}
		if err := moveTreeAt(e.SocketTreeFd, nix, "var/fev/run/store.sock"); err != nil {
			return fail(err)
		}
	}
	if e.HomeTreeFd >= 0 {
		if err := moveTreeAt(e.HomeTreeFd, nix, "var/fev/home"); err != nil {
			return fail(err)
		}
	}
	finish = func() error {
		defer unix.Close(nix)
		if ownStore {
			if err := unionStore("/nix/store", "/nix/.fev-store", "/nix/.target-store"); err != nil {
				return err
			}
			for _, d := range []string{"/nix/.fev-store", "/nix/.target-store"} {
				unix.Unmount(d, unix.MNT_DETACH)
				unix.Rmdir(d)
			}
		}
		if err := mountSetattrPath(nix, unix.MOUNT_ATTR_RDONLY, false); err != nil {
			return fmt.Errorf("/nix read-only: %w", err)
		}
		return nil
	}
	return finish, ownNix, nil
}

func unionStore(dst string, lowers ...string) error {
	fs, err := unix.Fsopen("overlay", unix.FSOPEN_CLOEXEC)
	if err != nil {
		return fmt.Errorf("fsopen overlay: %w", err)
	}
	defer unix.Close(fs)
	for _, l := range lowers {
		if err := unix.FsconfigSetString(fs, "lowerdir+", l); err != nil {
			return fmt.Errorf("the joined /nix/store, lowerdir+ %s: %w", l, err)
		}
	}
	if err := unix.FsconfigCreate(fs); err != nil {
		return fmt.Errorf("the joined /nix/store: %w", err)
	}
	m, err := unix.Fsmount(fs, unix.FSMOUNT_CLOEXEC, unix.MOUNT_ATTR_RDONLY|unix.MOUNT_ATTR_NODEV|unix.MOUNT_ATTR_NOSUID)
	if err != nil {
		return fmt.Errorf("fsmount the joined /nix/store: %w", err)
	}
	defer unix.Close(m)
	return unix.MoveMount(m, "", unix.AT_FDCWD, dst, unix.MOVE_MOUNT_F_EMPTY_PATH)
}

func freshDev(rootfd, ptsFd int) error {
	fs, err := unix.Fsopen("tmpfs", unix.FSOPEN_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(fs)
	for k, v := range map[string]string{"mode": "0755", "size": "65536k"} {
		if err := unix.FsconfigSetString(fs, k, v); err != nil {
			return err
		}
	}
	if err := unix.FsconfigCreate(fs); err != nil {
		return err
	}
	dev, err := unix.Fsmount(fs, unix.FSMOUNT_CLOEXEC, unix.MOUNT_ATTR_NOSUID|unix.MOUNT_ATTR_NOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(dev)
	old := unix.Umask(0)
	defer unix.Umask(old)
	for _, n := range []struct {
		name         string
		major, minor uint32
	}{{"null", 1, 3}, {"zero", 1, 5}, {"full", 1, 7}, {"random", 1, 8}, {"urandom", 1, 9}, {"tty", 5, 0}} {
		if err := unix.Mknodat(dev, n.name, unix.S_IFCHR|0o666, int(unix.Mkdev(n.major, n.minor))); err != nil {
			return fmt.Errorf("mknod /dev/%s: %w", n.name, err)
		}
	}
	for name, target := range map[string]string{
		"ptmx": "pts/ptmx", "fd": "/proc/self/fd", "stdin": "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2",
	} {
		if err := unix.Symlinkat(target, dev, name); err != nil {
			return err
		}
	}
	for _, d := range []string{"pts", "shm", "mqueue"} {
		if err := unix.Mkdirat(dev, d, 0o755); err != nil {
			return err
		}
	}
	if ptsFd >= 0 {
		if err := moveTreeAt(ptsFd, dev, "pts"); err != nil {
			return err
		}
	}
	shmfs, err := unix.Fsopen("tmpfs", unix.FSOPEN_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(shmfs)
	for k, v := range map[string]string{"mode": "1777", "size": "65536k"} {
		if err := unix.FsconfigSetString(shmfs, k, v); err != nil {
			return err
		}
	}
	if err := unix.FsconfigCreate(shmfs); err != nil {
		return err
	}
	shm, err := unix.Fsmount(shmfs, unix.FSMOUNT_CLOEXEC, unix.MOUNT_ATTR_NOSUID|unix.MOUNT_ATTR_NODEV|unix.MOUNT_ATTR_NOEXEC)
	if err != nil {
		return err
	}
	if err := moveTreeAt(shm, dev, "shm"); err != nil {
		return err
	}
	return unix.MoveMount(dev, "", rootfd, "dev", unix.MOVE_MOUNT_F_EMPTY_PATH)
}

func moveTreeAt(tree, dirfd int, rel string) error {
	if err := unix.MoveMount(tree, "", dirfd, rel, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount onto /nix/%s: %w", rel, err)
	}
	unix.Close(tree)
	return nil
}

func overlayRoot(lowers []string, upper, work string) (string, error) {
	const mnt = "/run"
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return "", err
	}
	if err := unix.Mount("tmpfs", mnt, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755"); err != nil {
		return "", fmt.Errorf("a tmpfs at %s: %w", mnt, err)
	}
	dst := mnt + "/dbg-root"
	os.Mkdir(dst, 0o755)
	fs, err := unix.Fsopen("overlay", unix.FSOPEN_CLOEXEC)
	if err != nil {
		return "", fmt.Errorf("fsopen overlay: %w", err)
	}
	defer unix.Close(fs)
	for _, l := range lowers {
		if err := unix.FsconfigSetString(fs, "lowerdir+", l); err != nil {
			return "", fmt.Errorf("overlay lowerdir+ %s: %w", l, err)
		}
	}
	if upper != "" {
		if err := unix.FsconfigSetString(fs, "upperdir", upper); err != nil {
			return "", fmt.Errorf("overlay upperdir: %w", err)
		}
		if err := unix.FsconfigSetString(fs, "workdir", work); err != nil {
			return "", fmt.Errorf("overlay workdir: %w", err)
		}
	}
	if err := unix.FsconfigSetString(fs, "redirect_dir", "off"); err != nil {
		return "", fmt.Errorf("overlay redirect_dir: %w", err)
	}
	if err := unix.FsconfigSetString(fs, "metacopy", "off"); err != nil {
		return "", fmt.Errorf("overlay metacopy: %w", err)
	}
	if err := unix.FsconfigCreate(fs); err != nil {
		return "", fmt.Errorf("overlay create: %w", err)
	}
	m, err := unix.Fsmount(fs, unix.FSMOUNT_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("fsmount overlay: %w", err)
	}
	if err := unix.MoveMount(m, "", unix.AT_FDCWD, dst, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return "", fmt.Errorf("move_mount the overlay: %w", err)
	}
	unix.Close(m)
	return dst, nil
}

func becomeRoot(rootfd int) error {
	if err := unix.MoveMount(rootfd, "", unix.AT_FDCWD, "/", unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount the new root over /: %w", err)
	}
	if err := unix.Fchdir(rootfd); err != nil {
		return fmt.Errorf("fchdir the new root: %w", err)
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach the old root: %w", err)
	}
	return unix.Chdir("/")
}

func mountSetattrPath(fd int, set uint64, recursive bool) error {
	flags := uint(unix.AT_EMPTY_PATH)
	if recursive {
		flags |= unix.AT_RECURSIVE
	}
	return unix.MountSetattr(fd, "", flags, &unix.MountAttr{Attr_set: set})
}

func joinCgroup(procsFd int) error {
	_, err := unix.Write(procsFd, []byte(fmt.Sprintf("%d\n", os.Getpid())))
	return err
}

func clearCloexec(fd int) {
	unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0)
}

func lookPath(file string, env []string) (string, error) {
	path := envValue(env, "PATH")
	if path == "" {
		path = defaultTargetPath
	}
	var denied string
	for _, dir := range strings.Split(path, ":") {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, file)
		st, err := os.Stat(cand)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if unix.Access(cand, unix.X_OK) == nil {
			return cand, nil
		}
		if denied == "" {
			denied = cand
		}
	}
	if denied != "" {
		return "", fmt.Errorf("%s: %w", denied, unix.EACCES)
	}
	return "", fmt.Errorf("%s: command not found", file)
}

func chooseDir(dir, home string) string {
	if dir != "" {
		return dir
	}
	if home != "" {
		return home
	}
	return "/"
}
