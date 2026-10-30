package privhelper

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	api "fev/proto/privhelper"
)

const ResolverDir = "/etc/resolver"

const maxResolverFile = 64 << 10

func entryText(port int) []byte {
	return fmt.Appendf(nil, "%s\nnameserver 127.0.0.1\nport %d\n", api.ResolverHeader, port)
}

func ResolverFileText(port int) []byte { return entryText(port) }

func isFev(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false, err
	}
	line, err := bufio.NewReader(io.LimitReader(f, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return strings.TrimRight(line, "\n") == api.ResolverHeader, nil
}

func entries(dir string) (map[string]time.Time, error) {
	des, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]time.Time{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	for _, de := range des {
		name := de.Name()
		c, err := api.CanonicalLocalName(name, "")
		if err != nil || c != name {
			continue
		}
		path := filepath.Join(dir, name)
		ours, err := isFev(path)
		if err != nil || !ours {
			continue
		}
		fi, err := os.Lstat(path)
		if err != nil {
			continue
		}
		out[name] = fi.ModTime()
	}
	return out, nil
}

func foreignResolvers(dir string) ([]Resolver, error) {
	des, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Resolver
	for _, de := range des {
		path := filepath.Join(dir, de.Name())
		if ours, err := isFev(path); err == nil && ours {
			continue
		}
		r := Resolver{Source: path, Domains: []string{de.Name()}}
		if b, err := readSmall(path); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				f := strings.Fields(line)
				if len(f) < 2 || strings.HasPrefix(f[0], "#") {
					continue
				}
				switch f[0] {
				case "domain":
					r.Domains = append(r.Domains, f[1])
				case "search":
					r.Search = append(r.Search, f[1:]...)
				}
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func readSmall(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxResolverFile))
}

func writeEntry(dir, name string, port int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	if _, err := os.Lstat(path); err == nil {
		if ours, err := isFev(path); err != nil || !ours {
			return fmt.Errorf("%s is there and is not fev's", path)
		}
	}
	return writeAtomic(path, entryText(port), 0o644)
}

func removeEntry(path string) error {
	ours, err := isFev(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ours {
		return fmt.Errorf("%s is not fev's", path)
	}
	return os.Remove(path)
}

func writeAtomic(path string, b []byte, mode fs.FileMode) error {
	var rnd [6]byte
	rand.Read(rnd[:])
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"."+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, bytes.NewReader(b)); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
