package report

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fev/host/daemon/doctor"
	"fev/host/daemon/home"
)

func put(t *testing.T, path, data string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) map[string]string {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out := map[string]string{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(data)
	}
	return out
}

func TestTheBundleHoldsWhatItSaysAndNoSecrets(t *testing.T) {
	dir := t.TempDir()
	l := home.Layout{Home: filepath.Join(dir, ".fev"), DataDir: filepath.Join(dir, ".fev", "data")}
	put(t, l.ConfigFile(), `{"network.proxy": "http://alice:s3cret@proxy.example:3128", "memory_mib": 4096}`)
	put(t, filepath.Join(l.LogDir(), "fevd.log"), "daemon line\n")
	put(t, filepath.Join(l.LogDir(), "fevd.log.1"), "older daemon line\n")
	put(t, filepath.Join(l.LogDir(), "console.log"), "console line\n")
	put(t, filepath.Join(l.Home, "setup.json"), `{"profiles":[]}`)
	put(t, l.DataImage(), "DATA-IMAGE-CONTENTS")
	put(t, filepath.Join(l.Home, "https", "ca.key"), "CA-KEY-CONTENTS")
	put(t, filepath.Join(l.Home, "ssh", "id_ed25519"), "SSH-KEY-CONTENTS")
	os.Symlink("/nowhere", filepath.Join(l.Home, "bin-link"))
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.Local)
	path := filepath.Join(dir, DefaultName(now))
	if filepath.Base(path) != "fev-report-20261019-100000.zip" {
		t.Errorf("name %s", path)
	}
	d := doctor.Report{Home: l.Home, Findings: []doctor.Finding{{ID: "path.missing", Severity: doctor.Problem, Text: "not on PATH"}}}
	got, err := Write(context.Background(), path, Options{Layout: l, Doctor: d, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	files := read(t, path)
	for _, name := range []string{"README.txt", "summary.txt", "doctor.json", "home.txt", "config/config.json", "setup.json", "log/fevd.log", "log/fevd.log.1", "log/console.log"} {
		if _, ok := files[name]; !ok {
			t.Errorf("%s is not in the bundle: %v", name, got.Files)
		}
	}
	for name, data := range files {
		for _, secret := range []string{"DATA-IMAGE-CONTENTS", "CA-KEY-CONTENTS", "SSH-KEY-CONTENTS", "s3cret", "alice"} {
			if strings.Contains(data, secret) {
				t.Errorf("%s holds %q", name, secret)
			}
		}
		if strings.HasPrefix(name, "daemon/") {
			t.Errorf("%s with no daemon", name)
		}
	}
	if !strings.Contains(files["config/config.json"], "http://REDACTED@proxy.example:3128") || !strings.Contains(files["config/config.json"], `"memory_mib": 4096`) {
		t.Errorf("config: %s", files["config/config.json"])
	}
	if files["log/fevd.log.1"] != "older daemon line\n" {
		t.Errorf("a log file: %q", files["log/fevd.log.1"])
	}
	for _, want := range []string{"(not listed: fev's CA and keys)", "(not listed: fev's SSH keys", "(not listed: the data image", "bin-link -> /nowhere"} {
		if !strings.Contains(files["home.txt"], want) {
			t.Errorf("home.txt does not say %q:\n%s", want, files["home.txt"])
		}
	}
	if strings.Contains(files["home.txt"], "ca.key") || strings.Contains(files["home.txt"], "id_ed25519") {
		t.Errorf("home.txt walked a secret's directory:\n%s", files["home.txt"])
	}
	for _, want := range []string{"build: ", "macos productversion: ", "memory: ", "processors: "} {
		if !strings.Contains(files["summary.txt"], want) {
			t.Errorf("summary.txt does not say %q:\n%s", want, files["summary.txt"])
		}
	}
	if !strings.Contains(files["README.txt"], "No daemon answered") || !strings.Contains(files["doctor.json"], "path.missing") {
		t.Errorf("README:\n%s\ndoctor: %s", files["README.txt"], files["doctor.json"])
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 || got.Bytes != info.Size() {
		t.Errorf("mode %v, %d bytes said, %d there", info.Mode(), got.Bytes, info.Size())
	}
	if _, err := Write(context.Background(), path, Options{Layout: l, Now: now}); err == nil {
		t.Error("an existing report was written over")
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".fev-report-*"))
	if len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestRedactTakesOutCredentialsInURLs(t *testing.T) {
	for in, want := range map[string]string{
		`"http://u:p@h:1"`:                     `"http://REDACTED@h:1"`,
		`socks5h://user@10.0.0.1:1080 and x`:   `socks5h://REDACTED@10.0.0.1:1080 and x`,
		`"https://proxy.example:8080"`:         `"https://proxy.example:8080"`,
		`mail me@example.com, see /a/b@c`:      `mail me@example.com, see /a/b@c`,
		`["http://a:b@x", "http://c:d@y/p@q"]`: `["http://REDACTED@x", "http://REDACTED@y/p@q"]`,
	} {
		if got := string(Redact([]byte(in))); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
