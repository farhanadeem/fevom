package emulation

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	var c Config
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	return c, err
}

func Inspect(root string, argv, env []string) (Facts, error) {
	rfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return Facts{}, err
	}
	defer unix.Close(rfd)
	read := func(path string, limit int64) ([]byte, error) {
		pfd, err := unix.Openat2(rfd, path, &unix.OpenHow{Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS})
		if err != nil {
			return nil, err
		}
		defer unix.Close(pfd)
		var st unix.Stat_t
		if err := unix.Fstat(pfd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
			return nil, fmt.Errorf("%s is not a regular file", path)
		}
		fd, err := unix.Open(fmt.Sprintf("/proc/self/fd/%d", pfd), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		f := os.NewFile(uintptr(fd), path)
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, limit))
	}
	facts := Facts{}
	b, err := read("/etc/os-release", 8192)
	if err != nil {
		b, _ = read("/usr/lib/os-release", 8192)
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, value, _ := strings.Cut(line, "=")
		value = strings.Trim(value, "\"'")
		switch key {
		case "ID":
			facts.Distro = value
		case "VERSION_ID":
			facts.Version = value
		}
	}
	paths := []string{"/bin/sh", "/sbin/init"}
	if len(argv) > 0 {
		facts.Sandbox = SandboxName(argv[0])
		if strings.Contains(argv[0], "/") {
			paths = append(paths, argv[0])
		} else {
			path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
			for _, e := range env {
				if p, ok := strings.CutPrefix(e, "PATH="); ok {
					path = p
				}
			}
			for _, dir := range strings.Split(path, ":") {
				if filepath.IsAbs(dir) {
					paths = append(paths, filepath.Join(dir, argv[0]))
				}
			}
		}
	}
	for _, path := range paths {
		b, _ := read(path, 20)
		if len(b) >= 20 && string(b[:4]) == "\x7fELF" && b[5] == 1 && b[18] == 62 && b[19] == 0 {
			facts.AMD64 = true
			break
		}
	}
	if facts.Sandbox == "" {
		for _, path := range []string{"/usr/bin/nix", "/usr/bin/nix-daemon", "/nix/var/nix/profiles/default/bin/nix", "/usr/bin/pacman", "/usr/bin/bwrap", "/usr/bin/firejail", "/usr/bin/podman", "/usr/bin/crun", "/usr/bin/runc", "/usr/bin/containerd", "/usr/local/bin/k3s"} {
			if _, err := read(path, 1); err == nil {
				facts.Sandbox = filepath.Base(path)
				break
			}
		}
	}
	return facts, nil
}

func SetCgroup(dir, engine string) error {
	if err := os.WriteFile(filepath.Join(dir, "cgroup.binfmt_group"), []byte(fmt.Sprint(Group(engine))), 0); err != nil {
		return fmt.Errorf("select %s for workload cgroup: %w (boot the matching fev kernel)", engine, err)
	}
	return nil
}
