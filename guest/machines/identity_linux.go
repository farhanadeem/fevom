package machines

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

type identity struct {
	UUID             string
	OldName, NewName string
}

const smallFile = 1 << 20

var cloudSeeds = []string{"/var/lib/cloud/seed/nocloud-net/meta-data", "/var/lib/cloud/seed/nocloud/meta-data"}

func resetIdentity(root string, id identity) (said []string, keys int, err error) {
	rfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, err
	}
	defer unix.Close(rfd)
	rewrite := func(p string, edit func(string) (string, bool)) error {
		b, err := readInRoot(rfd, p, smallFile)
		if err != nil || b == nil {
			return err
		}
		text, ok := edit(string(b))
		if !ok {
			return nil
		}
		if _, err := writeInRoot(rfd, p, []byte(text), false); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		said = append(said, p)
		return nil
	}
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if err := rewrite(p, func(s string) (string, bool) {
			v := strings.TrimSpace(s)
			if v == "" || v == "uninitialized" || v == machineID(id.UUID) {
				return s, false
			}
			return machineID(id.UUID) + "\n", true
		}); err != nil {
			return said, 0, err
		}
	}
	if id.OldName != "" && id.OldName != id.NewName {
		if err := rewrite("/etc/hostname", func(s string) (string, bool) { return swapHostname(s, id.OldName, id.NewName) }); err != nil {
			return said, 0, err
		}
		if err := rewrite("/etc/hosts", func(s string) (string, bool) { return swapHosts(s, id.OldName, id.NewName) }); err != nil {
			return said, 0, err
		}
	}
	host := ""
	if id.OldName != "" && id.OldName != id.NewName {
		host = id.NewName
	}
	for _, p := range cloudSeeds {
		if err := rewrite(p, func(s string) (string, bool) {
			t := resetMetaData(s, id.UUID, host)
			return t, t != s
		}); err != nil {
			return said, 0, err
		}
	}
	removed, err := removeHostKeys(rfd)
	if removed > 0 {
		said = append(said, fmt.Sprintf("/etc/ssh: %d host key files", removed))
	}
	return said, removed, err
}

func removeHostKeys(rfd int) (int, error) {
	dfd, err := unix.Openat2(rfd, "etc/ssh", &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: inRoot})
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	dir := os.NewFile(uintptr(dfd), "/etc/ssh")
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, name := range names {
		if !isSSHHostKey(name) {
			continue
		}
		if err := unix.Unlinkat(dfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return n, fmt.Errorf("/etc/ssh/%s: %w", name, err)
		}
		n++
	}
	return n, nil
}

func renameHost(root, old, new string) ([]string, error) {
	rfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rfd)
	var said []string
	for _, f := range []struct {
		path string
		edit func(string) (string, bool)
	}{
		{"/etc/hostname", func(s string) (string, bool) { return swapHostname(s, old, new) }},
		{"/etc/hosts", func(s string) (string, bool) { return swapHosts(s, old, new) }},
		{cloudSeeds[0], func(s string) (string, bool) { return swapMetaHostname(s, old, new) }},
		{cloudSeeds[1], func(s string) (string, bool) { return swapMetaHostname(s, old, new) }},
	} {
		b, err := readInRoot(rfd, f.path, smallFile)
		if err != nil {
			return said, err
		}
		if b == nil {
			continue
		}
		text, ok := f.edit(string(b))
		if !ok {
			continue
		}
		if _, err := writeInRoot(rfd, f.path, []byte(text), false); err != nil {
			return said, fmt.Errorf("%s: %w", f.path, err)
		}
		said = append(said, f.path)
	}
	return said, nil
}
