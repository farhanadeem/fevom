package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var bg = context.Background()

func sum(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

func TestTheLockPinsTheFiveToolsByChecksumAndLicence(t *testing.T) {
	lock, err := Lock()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range lock {
		names = append(names, tool.Name+" "+tool.Licence)
		if !strings.HasPrefix(tool.URL, "https://") || len(tool.SHA256) != 64 || tool.Version == "" {
			t.Errorf("%+v", tool)
		}
		if !strings.Contains(tool.URL, strings.TrimPrefix(tool.Version, "v")) {
			t.Errorf("%s: the URL %s does not name version %s", tool.Name, tool.URL, tool.Version)
		}
	}
	want := "docker Apache-2.0, docker-compose Apache-2.0, docker-buildx Apache-2.0, docker-credential-osxkeychain MIT, kubectl Apache-2.0"
	if got := strings.Join(names, ", "); got != want {
		t.Fatalf("the lock lists %q, want %q", got, want)
	}
	fixtures, err := os.ReadFile("../../../tests/fixtures/docker-cli.lock")
	if err != nil {
		t.Skip(err)
	}
	if !strings.Contains(string(fixtures), lock[0].SHA256) {
		t.Errorf("tests/fixtures/docker-cli.lock pins a different docker CLI than tools.lock (%s)", lock[0].SHA256)
	}
}

func TestALockLineThatIsNotAPinIsRefused(t *testing.T) {
	good := "tool docker 1.0 Apache-2.0 " + strings.Repeat("ab", 32) + " https://example.com/docker-1.0"
	if tools, err := parseLock("# comment\n\n" + good + "\n"); err != nil || len(tools) != 1 {
		t.Fatal(tools, err)
	}
	for name, line := range map[string]string{
		"a GPL tool":        strings.Replace(good, "Apache-2.0", "GPL-2.0", 1),
		"no licence":        strings.Replace(good, "Apache-2.0 ", "", 1),
		"plain http":        strings.Replace(good, "https://", "http://", 1),
		"a short checksum":  strings.Replace(good, strings.Repeat("ab", 32), "abab", 1),
		"not hex":           strings.Replace(good, strings.Repeat("ab", 32), strings.Repeat("zz", 32), 1),
		"a path for a name": strings.Replace(good, "tool docker ", "tool ../docker ", 1),
		"a hidden name":     strings.Replace(good, "tool docker ", "tool .docker ", 1),
		"another keyword":   strings.Replace(good, "tool ", "source ", 1),
		"too many fields":   good + " member extra",
		"listed twice":      good + "\n" + good,
	} {
		if tools, err := parseLock(line); err == nil {
			t.Errorf("%s was accepted: %+v", name, tools)
		}
	}
}

func archive(t *testing.T, members ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, m := range members {
		hdr := &tar.Header{Name: m[0], Mode: 0o755, Size: int64(len(m[1])), Typeflag: tar.TypeReg}
		if m[1] == "->" {
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeSymlink, "/etc/passwd", 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			tw.Write([]byte(m[1]))
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

type world struct {
	Installer
	served  map[string][]byte
	fetched []string
	log     []string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	dir := t.TempDir()
	w := &world{served: map[string][]byte{}}
	w.Cache, w.Bin = filepath.Join(dir, "cache"), filepath.Join(dir, "home", "bin")
	w.Fetch = func(_ context.Context, url string) (io.ReadCloser, error) {
		w.fetched = append(w.fetched, url)
		data, ok := w.served[url]
		if !ok {
			return nil, fmt.Errorf("%s: 404 Not Found", url)
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	w.Logf = func(format string, args ...any) { w.log = append(w.log, fmt.Sprintf(format, args...)) }
	return w
}

func (w *world) add(name string, download []byte, member string) {
	url := "https://example.com/" + name
	w.served[url] = download
	w.Tools = append(w.Tools, Tool{Name: name, Version: "1.0", Licence: "MIT", SHA256: sum(download), URL: url, Member: member})
}

func content(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestToolsAreDownloadedOnceVerifiedAndLaidOutInBin(t *testing.T) {
	w := newWorld(t)
	w.add("docker", archive(t, [2]string{"docker/README", "not this"}, [2]string{"docker/docker", "the docker CLI"}), "docker/docker")
	w.add("docker-compose", []byte("compose"), "")
	done, err := w.Install(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 2 || !done[0].Changed || !done[1].Changed || done[0].Path != filepath.Join(w.Bin, "docker") {
		t.Fatalf("%+v", done)
	}
	if got := content(t, filepath.Join(w.Bin, "docker")); got != "the docker CLI" {
		t.Fatalf("bin/docker is %q", got)
	}
	if got := content(t, filepath.Join(w.Bin, "docker-compose")); got != "compose" {
		t.Fatalf("bin/docker-compose is %q", got)
	}
	entries, _ := os.ReadDir(w.Bin)
	for _, e := range entries {
		info, _ := e.Info()
		if info.Mode().Perm() != 0o755 || strings.Contains(e.Name(), "tmp") {
			t.Errorf("%s has mode %v", e.Name(), info.Mode())
		}
	}
	if len(entries) != 2 {
		t.Errorf("bin holds %d files", len(entries))
	}

	before, _ := os.Stat(filepath.Join(w.Bin, "docker"))
	done, err = w.Install(bg)
	if err != nil || done[0].Changed || done[1].Changed {
		t.Fatalf("%+v, %v", done, err)
	}
	if len(w.fetched) != 2 {
		t.Fatalf("fetched %v", w.fetched)
	}
	if after, _ := os.Stat(filepath.Join(w.Bin, "docker")); !after.ModTime().Equal(before.ModTime()) {
		t.Error("an identical tool was installed again")
	}
	os.WriteFile(filepath.Join(w.Bin, "docker-compose"), []byte("damaged"), 0o644)
	if done, err = w.Install(bg); err != nil || done[0].Changed || !done[1].Changed {
		t.Fatalf("%+v, %v", done, err)
	}
	if got := content(t, filepath.Join(w.Bin, "docker-compose")); got != "compose" || len(w.fetched) != 2 {
		t.Fatalf("bin/docker-compose is %q after %d fetches", got, len(w.fetched))
	}
}

func TestADownloadThatDoesNotMatchItsPinIsNotInstalled(t *testing.T) {
	w := newWorld(t)
	w.add("docker-buildx", []byte("buildx"), "")
	w.served["https://example.com/docker-buildx"] = []byte("buildy")
	_, err := w.Install(bg)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") || !strings.Contains(err.Error(), w.Tools[0].SHA256) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.Bin, "docker-buildx")); !os.IsNotExist(err) {
		t.Fatal("the tool was installed")
	}
	if left, _ := os.ReadDir(w.Cache); len(left) != 0 {
		t.Fatalf("the cache holds %v", left)
	}
	if left, _ := os.ReadDir(w.Bin); len(left) != 0 {
		t.Fatalf("bin holds %v", left)
	}
	delete(w.served, "https://example.com/docker-buildx")
	if _, err := w.Install(bg); err == nil || !strings.Contains(err.Error(), "docker-buildx") {
		t.Fatalf("got %v", err)
	}
}

func TestWithNoFetcherOnlyTheCacheIsUsed(t *testing.T) {
	w := newWorld(t)
	w.add("docker", []byte("#!/bin/sh\necho docker\n"), "")
	if _, err := w.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	offline := Installer{Cache: w.Cache, Bin: filepath.Join(t.TempDir(), "tools"), Tools: w.Tools}
	if _, err := offline.Install(context.Background()); err != nil {
		t.Fatalf("with the download cached and no fetcher: %v", err)
	}
	if got := content(t, filepath.Join(offline.Bin, "docker")); got != "#!/bin/sh\necho docker\n" {
		t.Errorf("installed from the cache: %q", got)
	}
	empty := Installer{Cache: filepath.Join(t.TempDir(), "cache"), Bin: filepath.Join(t.TempDir(), "tools"), Tools: w.Tools}
	_, err := empty.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "docker 1.0 is not in the cache") || !strings.Contains(err.Error(), "no way to download") {
		t.Errorf("with an empty cache and no fetcher: %v", err)
	}
	if len(w.fetched) != 1 {
		t.Errorf("fetched %d times, want the first install's one", len(w.fetched))
	}
}

func TestACachedDownloadIsVerifiedEveryTime(t *testing.T) {
	w := newWorld(t)
	w.add("docker-compose", []byte("compose"), "")
	if _, err := w.Install(bg); err != nil {
		t.Fatal(err)
	}
	cached, _ := filepath.Glob(filepath.Join(w.Cache, "docker-compose-1.0-*"))
	if len(cached) != 1 {
		t.Fatalf("cache: %v", cached)
	}
	os.WriteFile(cached[0], []byte("compost"), 0o644)
	os.Remove(filepath.Join(w.Bin, "docker-compose"))
	if _, err := w.Install(bg); err != nil {
		t.Fatal(err)
	}
	if got := content(t, filepath.Join(w.Bin, "docker-compose")); got != "compose" {
		t.Fatalf("installed %q from a cache that did not match", got)
	}
	if len(w.fetched) != 2 {
		t.Fatalf("fetched %d times, want 2: the bad cache file should have been replaced", len(w.fetched))
	}
}

func TestOnlyTheNamedMemberOfAnArchiveIsTaken(t *testing.T) {
	w := newWorld(t)
	w.add("docker", archive(t, [2]string{"../../escape", "x"}, [2]string{"docker/docker", "->"}, [2]string{"docker/other", "y"}), "docker/docker")
	if _, err := w.Install(bg); err == nil || !strings.Contains(err.Error(), "no docker/docker in the archive") {
		t.Fatalf("got %v", err)
	}
	if left, _ := os.ReadDir(w.Bin); len(left) != 0 {
		t.Fatalf("bin holds %v", left)
	}
	w2 := newWorld(t)
	w2.add("docker", []byte("not an archive"), "docker/docker")
	if _, err := w2.Install(bg); err == nil {
		t.Fatal("a download that is not an archive was installed")
	}
}

func TestPluginLinksAreAddedAndNobodyElsesAreTouched(t *testing.T) {
	dir := t.TempDir()
	config, bin := filepath.Join(dir, "docker"), filepath.Join(dir, "home", "bin")
	left, err := LinkPlugins(config, bin)
	if err != nil || len(left) != 0 {
		t.Fatal(left, err)
	}
	for _, name := range Plugins {
		if target, err := os.Readlink(filepath.Join(config, "cli-plugins", name)); err != nil || target != filepath.Join(bin, name) {
			t.Fatalf("%s -> %q, %v", name, target, err)
		}
	}
	if entries, _ := os.ReadDir(config); len(entries) != 1 || entries[0].Name() != "cli-plugins" {
		t.Fatalf("the configuration directory holds %v", entries)
	}
	if left, err := LinkPlugins(config, bin); err != nil || len(left) != 0 {
		t.Fatal(left, err)
	}

	compose, buildx := filepath.Join(config, "cli-plugins", "docker-compose"), filepath.Join(config, "cli-plugins", "docker-buildx")
	os.Remove(compose)
	os.Symlink("/Applications/Other.app/Contents/Resources/cli-plugins/docker-compose", compose)
	os.Remove(buildx)
	os.WriteFile(buildx, []byte("a binary"), 0o755)
	left, err = LinkPlugins(config, bin)
	if err != nil || len(left) != 2 {
		t.Fatal(left, err)
	}
	if target, _ := os.Readlink(compose); !strings.HasPrefix(target, "/Applications/Other.app") {
		t.Fatalf("another runtime's link now points at %q", target)
	}
	if content(t, buildx) != "a binary" {
		t.Fatal("a file that was not ours was replaced")
	}
	if err := UnlinkPlugins(config, bin); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(compose); err != nil {
		t.Fatal("another runtime's link was removed")
	}
	os.Remove(compose)
	os.Remove(buildx)
	LinkPlugins(config, bin)
	if err := UnlinkPlugins(config, bin); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(config, "cli-plugins")); len(entries) != 0 {
		t.Fatalf("after unlinking: %v", entries)
	}
}
