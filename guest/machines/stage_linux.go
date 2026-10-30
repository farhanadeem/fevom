package machines

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/internal/isolate"
	"fev/guest/internal/thp"
)

const StageName = "fev-machine-stage"

func IsStage(args []string) bool { return len(args) > 0 && args[0] == StageName }

type stageSpec struct {
	Name      string           `json:"name"`
	UUID      string           `json:"uuid"`
	Subvolume string           `json:"subvolume"`
	Root      string           `json:"root"`
	Console   string           `json:"console"`
	Notify    string           `json:"notify"`
	Run       string           `json:"run"`
	Devices   []devNode        `json:"devices"`
	Init      string           `json:"init"`
	Share     bool             `json:"share,omitempty"`
	Setup     *api.MachineUser `json:"setup,omitempty"`
	Keygen    bool             `json:"keygen,omitempty"`
	View      string           `json:"view,omitempty"`
	Resolver  string           `json:"resolver,omitempty"`
	Helpers   string           `json:"helpers,omitempty"`
	Trust     string           `json:"trust,omitempty"`
	Isolated  bool             `json:"isolated,omitempty"`
	Dev       string           `json:"dev,omitempty"`
	Agent     string           `json:"agent,omitempty"`
}

const (
	stageSetupDone = 's'
	stageKeysMade  = 'k'
)

func tellSupervisor(b byte) error {
	_, err := unix.Write(5, []byte{b})
	return err
}

const machinePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

const osRelease = "NAME=\"fev\"\nID=fev\nPRETTY_NAME=\"fev\"\n"

func StageMain() {
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "fev: the machine's first stage: "+format+"\n", args...)
		os.Exit(1)
	}
	if err := thp.Restore(); err != nil {
		fmt.Fprintf(os.Stderr, "fev: the machine runs without transparent huge pages: %v\n", err)
	}
	var spec stageSpec
	b, err := io.ReadAll(os.NewFile(3, "spec"))
	if err != nil {
		fail("read its spec: %v", err)
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		fail("read its spec: %v", err)
	}
	unix.CloseRange(3, ^uint(0), unix.CLOSE_RANGE_CLOEXEC)
	if spec.Isolated {
		if err := isolate.Install(); err != nil {
			fail("%v", err)
		}
	}
	if err := loopbackUp(); err != nil {
		fmt.Fprintf(os.Stderr, "fev: the machine's loopback (lo) is down: %v\n", err)
	}
	if err := buildRoot(&spec); err != nil {
		fail("%v", err)
	}
	var trust [][]byte
	trustRead := false
	if spec.Trust != "" {
		if trust, err = readTrust(spec.Trust); err != nil {
			fmt.Fprintf(os.Stderr, "fev: the Mac's trusted roots: %v\n", err)
		} else {
			trustRead = true
		}
	}
	goAhead := os.NewFile(4, "go")
	if _, err := goAhead.Read(make([]byte, 1)); err != nil {
		fail("the supervisor went away: %v", err)
	}
	goAhead.Close()
	if err := enterRoot(spec.Root); err != nil {
		fail("%v", err)
	}
	if agentPathInMachine(spec.Agent) {
		if _, err := os.Stat(AgentSock); err == nil {
			if err := BindAgentAt("", AgentSock, spec.Agent); err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "fev: the Mac's SSH agent is not at %s: %v\n", spec.Agent, err)
			}
		}
	}
	if spec.Setup != nil {
		if err := runSetup(*spec.Setup); err != nil {
			fail("the first boot's setup of %s: %v", spec.Setup.Name, err)
		}
		if err := tellSupervisor(stageSetupDone); err != nil {
			fail("the supervisor went away: %v", err)
		}
	}
	if spec.Keygen {
		if err := makeHostKeys(); err != nil {
			fmt.Fprintf(os.Stderr, "fev: new SSH host keys: %v\n", err)
		} else {
			if err := tellSupervisor(stageKeysMade); err != nil {
				fail("the supervisor went away: %v", err)
			}
		}
	}
	if trustRead {
		ran, err := installTrust("", trust)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "fev: the Mac's trusted roots: %v\n", err)
			if err := tellSupervisor(stageTrustFailed); err != nil {
				fail("the supervisor went away: %v", err)
			}
		case ran:
			if err := tellSupervisor(stageTrustInstalled); err != nil {
				fail("the supervisor went away: %v", err)
			}
		}
	}
	if _, err := unix.Setsid(); err != nil {
		fail("setsid: %v", err)
	}
	cfd, err := unix.Open("/dev/console", unix.O_RDWR, 0)
	if err != nil {
		fail("open the console: %v", err)
	}
	unix.IoctlSetInt(cfd, unix.TIOCSCTTY, 0)
	for i := 0; i < 3; i++ {
		unix.Dup2(cfd, i)
	}
	if cfd > 2 {
		unix.Close(cfd)
	}
	unix.CloseRange(3, ^uint(0), unix.CLOSE_RANGE_CLOEXEC)
	env := []string{
		"container=lxc",
		"container_uuid=" + spec.UUID,
		"NOTIFY_SOCKET=/run/host/notify",
		"TERM=linux",
		"PATH=" + machinePath,
	}
	err = unix.Exec(spec.Init, []string{spec.Init}, env)
	if errors.Is(err, unix.ENOEXEC) {
		fail("exec %s: %v: no translator for its architecture is registered (an amd64 machine needs emulation.x86 on)", spec.Init, err)
	}
	fail("exec %s: %v", spec.Init, err)
}

type mountStep struct {
	source, target, fstype string
	flags                  uintptr
	data                   string
}

func buildRoot(spec *stageSpec) error {
	r := spec.Root
	const quiet = unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC
	do := func(steps ...mountStep) error {
		for _, m := range steps {
			target := r + m.target
			if m.flags&unix.MS_REMOUNT == 0 {
				if err := mountPoint(m.source, target, m.flags&unix.MS_BIND != 0 && m.fstype == ""); err != nil {
					return err
				}
			}
			if err := unix.Mount(m.source, target, m.fstype, m.flags, m.data); err != nil {
				return fmt.Errorf("mount %s on the machine's %s: %w", m.source, filepath.Join("/", m.target), err)
			}
		}
		return nil
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		return fmt.Errorf("make / a slave: %w", err)
	}
	if spec.Isolated {
		if err := unix.Mount(r, r, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("bind the machine's root on itself: %w", err)
		}
	} else {
		if err := do(
			mountStep{spec.Subvolume, "", "", unix.MS_BIND, ""},
			mountStep{"", "", "", unix.MS_BIND | unix.MS_REMOUNT, ""},
		); err != nil {
			return err
		}
	}
	if err := do(
		mountStep{"proc", "/proc", "proc", quiet, ""},
		mountStep{r + "/proc/sys", "/proc/sys", "", unix.MS_BIND, ""},
		mountStep{"", "/proc/sys", "", unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | quiet, ""},
		mountStep{r + "/proc/sys/net", "/proc/sys/net", "", unix.MS_BIND, ""},
		mountStep{"", "/proc/sys/net", "", unix.MS_BIND | unix.MS_REMOUNT | quiet, ""},
		mountStep{"sysfs", "/sys", "sysfs", quiet | unix.MS_RDONLY, ""},
		mountStep{"cgroup2", "/sys/fs/cgroup", "cgroup2", quiet, ""},
		mountStep{"tmpfs", "/dev", "tmpfs", unix.MS_NOSUID | unix.MS_STRICTATIME, "mode=0755,size=4m"},
	); err != nil {
		return err
	}
	for _, n := range spec.Devices {
		p := filepath.Join(r, "dev", n.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if spec.Isolated {
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				return err
			}
			if err := unix.Mount(filepath.Join(spec.Dev, n.Path), p, "", unix.MS_BIND, ""); err != nil {
				return fmt.Errorf("bind /dev/%s: %w", n.Path, err)
			}
			continue
		}
		if err := unix.Mknod(p, unix.S_IFCHR|n.Mode, int(unix.Mkdev(n.Major, n.Minor))); err != nil {
			return fmt.Errorf("mknod /dev/%s: %w", n.Path, err)
		}
		if err := unix.Chmod(p, n.Mode); err != nil {
			return err
		}
	}
	for _, l := range []struct{ name, target string }{
		{"fd", "/proc/self/fd"}, {"stdin", "/proc/self/fd/0"}, {"stdout", "/proc/self/fd/1"},
		{"stderr", "/proc/self/fd/2"}, {"ptmx", "pts/ptmx"},
	} {
		if err := os.Symlink(l.target, filepath.Join(r, "dev", l.name)); err != nil {
			return err
		}
	}
	if err := do(
		mountStep{"devpts", "/dev/pts", "devpts", unix.MS_NOSUID | unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=620,gid=5"},
		mountStep{"tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		mountStep{"mqueue", "/dev/mqueue", "mqueue", quiet, ""},
		mountStep{spec.Console, "/dev/console", "", unix.MS_BIND, ""},
		mountStep{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_STRICTATIME, "mode=0755"},
	); err != nil {
		return err
	}
	host := filepath.Join(r, "run/host")
	if err := os.MkdirAll(filepath.Join(host, "inaccessible"), 0o755); err != nil {
		return err
	}
	for name, content := range map[string]string{"os-release": osRelease, "container-manager": "lxc\n"} {
		if err := os.WriteFile(filepath.Join(host, name), []byte(content), 0o444); err != nil {
			return err
		}
	}
	in := filepath.Join(host, "inaccessible")
	if err := os.Mkdir(filepath.Join(in, "dir"), 0); err != nil {
		return err
	}
	for name, mode := range map[string]uint32{"reg": unix.S_IFREG, "fifo": unix.S_IFIFO, "sock": unix.S_IFSOCK, "chr": unix.S_IFCHR, "blk": unix.S_IFBLK} {
		if spec.Isolated && name == "blk" {
			continue
		}
		if err := unix.Mknod(filepath.Join(in, name), mode, 0); err != nil {
			return fmt.Errorf("mknod /run/host/inaccessible/%s: %w", name, err)
		}
	}
	if err := unix.Chmod(filepath.Join(in, "dir"), 0); err != nil {
		return err
	}
	if err := do(
		mountStep{spec.Notify, "/run/host/notify", "", unix.MS_BIND, ""},
		mountStep{spec.Run, "/run/fev", "", unix.MS_BIND, ""},
	); err != nil {
		return err
	}
	if spec.Share {
		if err := bindShare(r); err != nil {
			return err
		}
	}
	if spec.Helpers != "" {
		if err := bindHelpers(r, spec.Helpers, do); err != nil {
			return err
		}
	}
	if spec.View != "" {
		if err := do(mountStep{spec.View, "/mnt/machines", "", unix.MS_BIND | unix.MS_REC, ""}); err != nil {
			return err
		}
	}
	if err := mountFstabTmp(r); err != nil {
		return fmt.Errorf("the machine's /tmp: %w", err)
	}
	if err := writeDropIns(r, filepath.Join(r, "run")); err != nil {
		return fmt.Errorf("the machine's drop-ins in /run: %w", err)
	}
	if err := writeResolvedDropIn(r, filepath.Join(r, "run"), spec.Resolver); err != nil {
		return fmt.Errorf("the machine's resolver drop-in in /run: %w", err)
	}
	return unix.Sethostname([]byte(spec.Name))
}

func bindHelpers(r, helpers string, do func(...mountStep) error) error {
	if st, err := os.Stat(helpers); err != nil || !st.IsDir() {
		fmt.Fprintln(os.Stderr, "fev: this VM has no helper for machines; the machine has no mac command")
		return nil
	}
	for _, p := range []string{"/opt", "/opt/fev"} {
		if st, err := os.Lstat(r + p); err == nil && st.Mode()&os.ModeSymlink != 0 {
			fmt.Fprintf(os.Stderr, "fev: %s is a link in the machine; it has no mac command\n", p)
			return nil
		}
	}
	return do(
		mountStep{helpers, "/opt/fev", "", unix.MS_BIND | unix.MS_REC, ""},
		mountStep{"", "/opt/fev", "", unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV, ""},
	)
}

func mountPoint(source, target string, bind bool) error {
	if bind {
		if st, err := os.Stat(source); err == nil && !st.IsDir() {
			if _, err := os.Lstat(target); err == nil {
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.WriteFile(target, nil, 0o600)
		}
	}
	if _, err := os.Stat(target); err == nil {
		return nil
	}
	return os.MkdirAll(target, 0o755)
}

func enterRoot(root string) error {
	if err := unix.Chdir(root); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", "."); err == nil {
		if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
			return fmt.Errorf("detach init's root: %w", err)
		}
	} else {
		if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("move the machine's root: %w", err)
		}
		if err := unix.Chroot("."); err != nil {
			return err
		}
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SHARED, ""); err != nil {
		return fmt.Errorf("make the machine's root shared: %w", err)
	}
	return nil
}

var sshKeygens = []string{"/usr/bin/ssh-keygen", "/bin/ssh-keygen"}

func makeHostKeys() error {
	for _, p := range sshKeygens {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		cmd := exec.Command(p, "-A")
		cmd.Env = []string{"PATH=" + machinePath, "LANG=C"}
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s -A: %w", p, err)
		}
		return nil
	}
	return nil
}

func mountFstabTmp(r string) error {
	root, err := os.Open(r)
	if err != nil {
		return err
	}
	defer root.Close()
	fd, err := unix.Openat2(int(root.Fd()), "etc/fstab", &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK, Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return nil
	}
	f := os.NewFile(uintptr(fd), "fstab")
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	f.Close()
	if err != nil || !fstabTmpfsTmp(string(b)) {
		return nil
	}
	tfd, err := unix.Openat2(int(root.Fd()), "tmp", &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return nil
	}
	defer unix.Close(tfd)
	var st unix.Stat_t
	if err := unix.Fstat(tfd, &st); err != nil || st.Mode&0o002 != 0 {
		return nil
	}
	return unix.Mount("tmpfs", fmt.Sprintf("/proc/self/fd/%d", tfd), "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777")
}
