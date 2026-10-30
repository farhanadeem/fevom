package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var bundles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/cert.pem",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/ssl/ca-bundle.pem",
}

const (
	caTrustDir   = "/etc/pki/ca-trust"
	debianBundle = "/etc/ssl/certs/ca-certificates.crt"
)

var (
	caTrustAnchor = struct{ dir, file string }{"/etc/pki/ca-trust/source/anchors", "fev-root.pem"}
	debianAnchor  = struct{ dir, file string }{"/usr/local/share/ca-certificates", "fev-root.crt"}
)

var envBundles = []string{"SSL_CERT_FILE", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO"}

const LabelCA = "dev.fevom.ca"

var containersDir = "/var/lib/docker/containers"

const maxBundle = 16 << 20

const resolveHow = unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV

func optedOut(id string) bool {
	if !validID(id) {
		return false
	}
	b, err := os.ReadFile(filepath.Join(containersDir, id, "config.v2.json"))
	if err != nil {
		return false
	}
	var c struct {
		Config struct{ Labels map[string]string }
	}
	json.Unmarshal(b, &c)
	return strings.EqualFold(strings.TrimSpace(c.Config.Labels[LabelCA]), "false")
}

type spec struct {
	Root struct {
		Path string `json:"path"`
	} `json:"root"`
	Process struct {
		Env  []string `json:"env"`
		Args []string `json:"args"`
	} `json:"process"`
	Annotations map[string]string `json:"annotations"`
	Linux       struct {
		CgroupsPath string `json:"cgroupsPath"`
	} `json:"linux"`
}

func inject(bundle, id string, root []byte) ([]string, error) {
	root = bytes.TrimSpace(root)
	if len(root) == 0 {
		return nil, nil
	}
	if optedOut(id) {
		return []string{"opted out"}, nil
	}
	raw, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return nil, err
	}
	var s spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	rootfs := s.Root.Path
	if rootfs == "" {
		return nil, errors.New("config.json names no root")
	}
	if !filepath.IsAbs(rootfs) {
		rootfs = filepath.Join(bundle, rootfs)
	}
	rfd, err := unix.Open(rootfs, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rfd)

	paths := append([]string(nil), bundles...)
	for _, e := range s.Process.Env {
		k, v, _ := strings.Cut(e, "=")
		for _, name := range envBundles {
			if k == name && path.IsAbs(v) {
				paths = append(paths, v)
			}
		}
	}
	type fileID struct{ dev, ino uint64 }
	seen := map[fileID]bool{}
	var done []string
	have := map[string]bool{}
	for _, p := range paths {
		pfd, err := openIn(rfd, p, unix.O_PATH, 0)
		if err != nil {
			continue
		}
		fd, st, err := reopenRegular(pfd, unix.O_RDWR)
		unix.Close(pfd)
		if err != nil {
			continue
		}
		if seen[fileID{st.Dev, st.Ino}] {
			unix.Close(fd)
			continue
		}
		seen[fileID{st.Dev, st.Ino}] = true
		have[p] = true
		r, err := appendRoot(fd, st.Size, root)
		unix.Close(fd)
		if err != nil {
			return done, fmt.Errorf("%s: %w", p, err)
		}
		done = append(done, p+r)
	}
	if len(done) == 0 {
		return nil, nil
	}
	var family []struct{ dir, file string }
	if fd, err := openIn(rfd, caTrustDir, unix.O_PATH|unix.O_DIRECTORY, 0); err == nil {
		unix.Close(fd)
		family = append(family, caTrustAnchor)
	} else if have[debianBundle] {
		family = append(family, debianAnchor)
	}
	for _, a := range family {
		dfd, err := mkdirIn(rfd, a.dir)
		if err != nil {
			return done, fmt.Errorf("%s: %w", a.dir, err)
		}
		err = writeFileAt(dfd, a.file, append(append([]byte(nil), root...), '\n'))
		unix.Close(dfd)
		if err != nil {
			return done, fmt.Errorf("%s/%s: %w", a.dir, a.file, err)
		}
		done = append(done, a.dir+"/"+a.file)
	}
	return done, nil
}

func openIn(rfd int, p string, flags int, mode uint32) (int, error) {
	rel := strings.TrimLeft(path.Clean("/"+p), "/")
	if rel == "" {
		rel = "."
	}
	return unix.Openat2(rfd, rel, &unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC), Mode: uint64(mode), Resolve: resolveHow})
}

func reopenRegular(pfd, flags int) (int, unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstat(pfd, &st); err != nil {
		return -1, st, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return -1, st, errNotRegular
	}
	fd, err := unix.Open(fmt.Sprintf("/proc/self/fd/%d", pfd), flags|unix.O_CLOEXEC|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	return fd, st, err
}

var errNotRegular = errors.New("not a regular file")

func appendRoot(fd int, size int64, root []byte) (string, error) {
	if size > maxBundle {
		return "", fmt.Errorf("a bundle of %d bytes", size)
	}
	buf := make([]byte, size)
	for off := 0; off < len(buf); {
		n, err := unix.Pread(fd, buf[off:], int64(off))
		if err != nil {
			return "", err
		}
		if n == 0 {
			buf = buf[:off]
			break
		}
		off += n
	}
	if bytes.Contains(buf, root) {
		return " (had it)", nil
	}
	add := append([]byte("\n"), root...)
	add = append(add, '\n')
	if _, err := unix.Pwrite(fd, add, size); err != nil {
		return "", err
	}
	return "", nil
}

func mkdirIn(rfd int, dir string) (int, error) {
	parts := strings.Split(strings.Trim(path.Clean("/"+dir), "/"), "/")
	cur := ""
	parent, err := unix.Dup(rfd)
	if err != nil {
		return -1, err
	}
	for _, part := range parts {
		cur = path.Join(cur, part)
		fd, err := openIn(rfd, cur, unix.O_PATH|unix.O_DIRECTORY, 0)
		if errors.Is(err, unix.ENOENT) {
			if err := unix.Mkdirat(parent, part, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
				unix.Close(parent)
				return -1, err
			}
			fd, err = openIn(rfd, cur, unix.O_PATH|unix.O_DIRECTORY, 0)
		}
		unix.Close(parent)
		if err != nil {
			return -1, err
		}
		parent = fd
	}
	return parent, nil
}

func writeFileAt(dfd int, name string, data []byte) error {
	const resolve = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV
	fd := -1
	for try := 0; fd < 0; try++ {
		pfd, err := unix.Openat2(dfd, name, &unix.OpenHow{Flags: unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC, Resolve: resolve})
		if err == nil {
			fd, _, err = reopenRegular(pfd, unix.O_WRONLY)
			unix.Close(pfd)
			if err != nil {
				return err
			}
			break
		}
		if !errors.Is(err, unix.ENOENT) {
			return err
		}
		fd, err = unix.Openat2(dfd, name, &unix.OpenHow{
			Flags:   unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC,
			Mode:    0o644,
			Resolve: resolve,
		})
		if errors.Is(err, unix.EEXIST) && try == 0 {
			continue
		}
		if err != nil {
			return err
		}
	}
	defer unix.Close(fd)
	if err := unix.Ftruncate(fd, 0); err != nil {
		return err
	}
	_, err := unix.Pwrite(fd, data, 0)
	return err
}
