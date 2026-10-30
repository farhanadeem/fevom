package tools

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed tools.lock
var lockFile string

type Tool struct {
	Name    string
	Version string
	Licence string
	SHA256  string
	URL     string
	Member  string
}

var Plugins = []string{"docker-compose", "docker-buildx"}

func Lock() ([]Tool, error) { return parseLock(lockFile) }

func parseLock(text string) ([]Tool, error) {
	var out []Tool
	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(text))
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 6 || len(f) > 7 || f[0] != "tool" {
			return nil, fmt.Errorf("tools.lock line %d: want `tool NAME VERSION LICENCE SHA256 URL [MEMBER]`", n)
		}
		t := Tool{Name: f[1], Version: f[2], Licence: f[3], SHA256: f[4], URL: f[5]}
		if len(f) == 7 {
			t.Member = f[6]
		}
		sum, err := hex.DecodeString(t.SHA256)
		switch {
		case err != nil || len(sum) != sha256.Size:
			return nil, fmt.Errorf("tools.lock line %d: %q is not a SHA-256", n, t.SHA256)
		case !strings.HasPrefix(t.URL, "https://"):
			return nil, fmt.Errorf("tools.lock line %d: %q is not an https URL", n, t.URL)
		case t.Name != filepath.Base(t.Name) || strings.HasPrefix(t.Name, "."):
			return nil, fmt.Errorf("tools.lock line %d: %q is not a file name", n, t.Name)
		case seen[t.Name]:
			return nil, fmt.Errorf("tools.lock line %d: %s is listed twice", n, t.Name)
		}
		switch t.Licence {
		case "Apache-2.0", "MIT", "BSD-2-Clause", "BSD-3-Clause", "MPL-2.0":
		default:
			return nil, fmt.Errorf("tools.lock line %d: licence %q is not one fev ships on the Mac", n, t.Licence)
		}
		seen[t.Name] = true
		out = append(out, t)
	}
	return out, scanner.Err()
}

const maxDownload = 400 << 20

type Fetcher func(ctx context.Context, url string) (io.ReadCloser, error)

type Installer struct {
	Cache string
	Bin   string
	Fetch Fetcher
	Tools []Tool
	Logf  func(format string, args ...any)
}

func (i *Installer) logf(format string, args ...any) {
	if i.Logf != nil {
		i.Logf(format, args...)
	}
}

func sumFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (i *Installer) cached(ctx context.Context, t Tool) (string, error) {
	path := filepath.Join(i.Cache, fmt.Sprintf("%s-%s-%s", t.Name, t.Version, t.SHA256[:12]))
	if sum, err := sumFile(path); err == nil {
		if sum == t.SHA256 {
			return path, nil
		}
		i.logf("%s: the cached download does not match its pin; fetching it again", t.Name)
		os.Remove(path)
	}
	if err := os.MkdirAll(i.Cache, 0o755); err != nil {
		return "", err
	}
	fetch := i.Fetch
	if fetch == nil {
		return "", fmt.Errorf("%s %s is not in the cache at %s, and this installer was given no way to download it", t.Name, t.Version, i.Cache)
	}
	i.logf("%s %s: downloading %s", t.Name, t.Version, t.URL)
	body, err := fetch(ctx, t.URL)
	if err != nil {
		return "", fmt.Errorf("%s: %w", t.Name, err)
	}
	defer body.Close()
	tmp, err := os.CreateTemp(i.Cache, t.Name+".*.part")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, maxDownload+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return "", fmt.Errorf("%s: %w", t.Name, err)
	case n > maxDownload:
		return "", fmt.Errorf("%s: the download is larger than %d bytes", t.Name, maxDownload)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != t.SHA256 {
		return "", fmt.Errorf("%s %s: checksum mismatch: %s gave %s and tools.lock pins %s", t.Name, t.Version, t.URL, sum, t.SHA256)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

func open(download string, t Tool) (io.ReadCloser, error) {
	f, err := os.Open(download)
	if err != nil {
		return nil, err
	}
	if t.Member == "" {
		return f, nil
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", t.Name, err)
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err != nil {
			f.Close()
			if errors.Is(err, io.EOF) {
				err = fmt.Errorf("there is no %s in the archive", t.Member)
			}
			return nil, fmt.Errorf("%s: %w", t.Name, err)
		}
		if hdr.Name == t.Member && hdr.Typeflag == tar.TypeReg {
			return struct {
				io.Reader
				io.Closer
			}{io.LimitReader(tr, maxDownload), f}, nil
		}
	}
}

type Installed struct {
	Tool
	Path    string
	Changed bool
}

func (i *Installer) Install(ctx context.Context) ([]Installed, error) {
	lock := i.Tools
	if lock == nil {
		var err error
		if lock, err = Lock(); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(i.Bin, 0o755); err != nil {
		return nil, err
	}
	var out []Installed
	for _, t := range lock {
		download, err := i.cached(ctx, t)
		if err != nil {
			return out, err
		}
		src, err := open(download, t)
		if err != nil {
			return out, err
		}
		tmp, err := os.CreateTemp(i.Bin, "."+t.Name+".*.tmp")
		if err != nil {
			src.Close()
			return out, err
		}
		h := sha256.New()
		_, err = io.Copy(io.MultiWriter(tmp, h), src)
		src.Close()
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(tmp.Name())
			return out, fmt.Errorf("%s: %w", t.Name, err)
		}
		dest := filepath.Join(i.Bin, t.Name)
		done := Installed{Tool: t, Path: dest}
		if have, err := sumFile(dest); err == nil && have == hex.EncodeToString(h.Sum(nil)) {
			os.Remove(tmp.Name())
			if info, err := os.Stat(dest); err == nil && info.Mode().Perm() != 0o755 {
				os.Chmod(dest, 0o755)
			}
		} else {
			if err := os.Chmod(tmp.Name(), 0o755); err != nil {
				os.Remove(tmp.Name())
				return out, err
			}
			if err := os.Rename(tmp.Name(), dest); err != nil {
				os.Remove(tmp.Name())
				return out, err
			}
			done.Changed = true
			i.logf("%s %s: installed %s", t.Name, t.Version, dest)
		}
		out = append(out, done)
	}
	return out, nil
}

func LinkPlugins(dockerConfig, bin string) (left []string, err error) {
	dir := filepath.Join(dockerConfig, "cli-plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for _, name := range Plugins {
		target := filepath.Join(bin, name)
		link := filepath.Join(dir, name)
		info, err := os.Lstat(link)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return left, err
		case info.Mode()&os.ModeSymlink == 0:
			left = append(left, link)
			continue
		default:
			have, _ := os.Readlink(link)
			if have == target {
				continue
			}
			left = append(left, link)
			continue
		}
		tmp := link + ".fev.tmp"
		os.Remove(tmp)
		if err := os.Symlink(target, tmp); err != nil {
			return left, err
		}
		if err := os.Rename(tmp, link); err != nil {
			os.Remove(tmp)
			return left, err
		}
	}
	return left, nil
}

func UnlinkPlugins(dockerConfig, bin string) error {
	for _, name := range Plugins {
		link := filepath.Join(dockerConfig, "cli-plugins", name)
		if have, err := os.Readlink(link); err == nil && have == filepath.Join(bin, name) {
			if err := os.Remove(link); err != nil {
				return err
			}
		}
	}
	return nil
}
