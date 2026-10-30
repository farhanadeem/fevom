package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		info, _ := d.Info()
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			lines = append(lines, fmt.Sprintf("%s -> %s", rel, target))
		case d.IsDir():
			lines = append(lines, fmt.Sprintf("%s/ %v", rel, info.Mode().Perm()))
		default:
			data, _ := os.ReadFile(path)
			lines = append(lines, fmt.Sprintf("%s %v %q", rel, info.Mode().Perm(), data))
		}
		return nil
	})
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

type setupHome struct {
	t    *testing.T
	user string
	fev  string
}

func newSetupHome(t *testing.T, files map[string]string) setupHome {
	t.Helper()
	user := t.TempDir()
	user, _ = filepath.EvalSymlinks(user)
	for name, content := range files {
		path := filepath.Join(user, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if target, ok := strings.CutPrefix(content, "-> "); ok {
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", user)
	t.Setenv("ZDOTDIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("DOCKER_CONFIG", "")
	t.Setenv(ToolsEnv, "")
	return setupHome{t, user, filepath.Join(user, ".fev")}
}

func (h setupHome) run(argv ...string) result {
	return fev(h.t, h.fev, "", append([]string{"setup", "--no-tools", "--no-start"}, argv...)...)
}

func (h setupHome) loginPath(shell string) []string {
	h.t.Helper()
	cmd := exec.Command(shell, "-l", "-c", "echo $PATH")
	cmd.Env = []string{"HOME=" + h.user, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "USER=" + os.Getenv("USER"), "TERM=dumb"}
	out, err := cmd.Output()
	if err != nil {
		h.t.Fatalf("%s -l: %v", shell, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), ":")
}

func TestSetupIsUndoneByteForByte(t *testing.T) {
	for _, c := range []struct {
		name, shell string
		files       map[string]string
		edited      string
	}{
		{"zsh, no profile", "zsh", nil, ".zprofile"},
		{"zsh, a profile", "zsh", map[string]string{".zprofile": "export EDITOR=vi\n", ".zshrc": "alias l=ls\n"}, ".zprofile"},
		{"zsh, no trailing newline", "zsh", map[string]string{".zprofile": "export EDITOR=vi"}, ".zprofile"},
		{"zsh, an empty profile", "zsh", map[string]string{".zprofile": ""}, ".zprofile"},
		{"zsh, the profile is a link", "zsh", map[string]string{"dotfiles/zprofile": "export EDITOR=vi\n", ".zprofile": "-> dotfiles/zprofile"}, "dotfiles/zprofile"},
		{"zsh, PATH set in the profile", "zsh", map[string]string{".zprofile": "export PATH=\"$HOME/mine:$PATH\"\n"}, ".zprofile"},
		{"bash, no profile", "bash", nil, ".bash_profile"},
		{"bash, .bash_profile", "bash", map[string]string{".bash_profile": "export EDITOR=vi\n", ".profile": "export A=1\n"}, ".bash_profile"},
		{"bash, .bash_login", "bash", map[string]string{".bash_login": "export EDITOR=vi\n", ".profile": "export A=1\n"}, ".bash_login"},
		{"bash, only .profile, no trailing newline", "bash", map[string]string{".profile": "export EDITOR=vi"}, ".profile"},
		{"bash, the profile is a link", "bash", map[string]string{"dotfiles/profile": "export EDITOR=vi\n", ".bash_profile": "-> dotfiles/profile"}, "dotfiles/profile"},
		{"fish, no configuration", "fish", nil, ".config/fish/conf.d/fev.fish"},
		{"fish, a configuration", "fish", map[string]string{".config/fish/config.fish": "set -x EDITOR vi\n", ".config/fish/conf.d/other.fish": "# other\n"}, ".config/fish/conf.d/fev.fish"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newSetupHome(t, c.files)
			before := snapshot(t, h.user)
			r := h.run("--shell", c.shell).want(t, 0)
			data, err := os.ReadFile(filepath.Join(h.user, c.edited))
			if err != nil || !strings.Contains(string(data), "$HOME/.fev/bin") {
				t.Fatalf("%s after setup: %q %v\n%s", c.edited, data, err, r.out)
			}
			for _, name := range []string{"fev", "fevctl", "docker", "docker-compose"} {
				self, _ := os.Executable()
				self, _ = filepath.EvalSymlinks(self)
				if target, _ := os.Readlink(filepath.Join(h.fev, "bin", name)); target != self {
					t.Errorf("bin/%s -> %q, want this program", name, target)
				}
			}
			if target, _ := os.Readlink(filepath.Join(h.user, ".docker/cli-plugins/docker-compose")); target != filepath.Join(h.fev, "tools/docker-compose") {
				t.Errorf("the compose plugin link -> %q", target)
			}
			if c.shell != "fish" {
				if path := h.loginPath(c.shell); path[0] != filepath.Join(h.fev, "bin") {
					t.Errorf("a login %s has PATH %v; want fev's bin directory first", c.shell, path)
				}
			}
			once := snapshot(t, h.user)
			h.run("--shell", c.shell).want(t, 0)
			if twice := snapshot(t, h.user); twice != once {
				t.Errorf("a second setup changed something:\n%s\n--- was\n%s", twice, once)
			}
			h.run("--shell", c.shell, "--undo").want(t, 0)
			if after := snapshot(t, h.user); after != before {
				t.Errorf("after --undo the home differs:\n%s\n--- was\n%s", after, before)
			}
		})
	}
}

func TestSetupLeavesWhatIsNotItsOwn(t *testing.T) {
	h := newSetupHome(t, map[string]string{".zprofile": "export A=1\n"})
	h.run("--shell", "zsh").want(t, 0)
	profile := filepath.Join(h.user, ".zprofile")
	f, _ := os.OpenFile(profile, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("export B=2\n")
	f.Close()
	h.run("--undo").want(t, 0)
	if data, _ := os.ReadFile(profile); string(data) != "export A=1\nexport B=2\n" {
		t.Errorf("after --undo: %q", data)
	}

	h = newSetupHome(t, nil)
	h.run("--shell", "bash").want(t, 0)
	profile = filepath.Join(h.user, ".bash_profile")
	f, _ = os.OpenFile(profile, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("export B=2\n")
	f.Close()
	h.run("--undo").want(t, 0)
	if data, _ := os.ReadFile(profile); string(data) != "export B=2\n" {
		t.Errorf("after --undo: %q", data)
	}

	h = newSetupHome(t, map[string]string{".fev/bin/docker": "#!/bin/sh\n"})
	r := h.run("--shell", "zsh").want(t, ExitFailed)
	if !strings.Contains(r.err, "is in the way") {
		t.Errorf("stderr: %q", r.err)
	}
	if _, err := os.Stat(filepath.Join(h.user, ".zprofile")); err == nil {
		t.Error("a profile was written although setup failed")
	}

	h = newSetupHome(t, nil)
	before := snapshot(t, h.user)
	t.Setenv("SHELL", "/bin/zsh")
	h.run().want(t, 0)
	h.run("--shell", "bash", "--shell", "fish").want(t, 0)
	for _, name := range []string{".zprofile", ".bash_profile", ".config/fish/conf.d/fev.fish"} {
		if _, err := os.Stat(filepath.Join(h.user, name)); err != nil {
			t.Error(err)
		}
	}
	h.run("--undo").want(t, 0)
	if after := snapshot(t, h.user); after != before {
		t.Errorf("after --undo of three shells the home differs:\n%s\n--- was\n%s", after, before)
	}
	t.Setenv("SHELL", "/bin/tcsh")
	if r := h.run(); r.code != ExitUsage {
		t.Errorf("an unknown $SHELL: exit %d %q", r.code, r.err)
	}
	if r := h.run("--shell", "tcsh"); r.code != ExitUsage {
		t.Errorf("--shell tcsh: exit %d", r.code)
	}
}

func TestADownloadThatFailsSaysWhy(t *testing.T) {
	body, err := curlFetch(context.Background(), "https://127.0.0.1:1/docker.tgz")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err == nil || len(data) != 0 || !strings.Contains(err.Error(), "curl:") {
		t.Errorf("read %d bytes, err %v; want curl's own failure", len(data), err)
	}
	body, err = curlFetch(context.Background(), "file:///etc/hosts")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if data, err := io.ReadAll(body); err == nil || len(data) != 0 {
		t.Errorf("a file: URL was fetched: %d bytes, err %v", len(data), err)
	}
}

func TestSetupUndoOffersToRemoveTheToolsAtATerminal(t *testing.T) {
	h := newSetupHome(t, map[string]string{".zprofile": "export A=1\n"})
	before := snapshot(t, h.user)
	downloaded := func() {
		for name, size := range map[string]int{"tools/docker": 3000, "tools/docker-compose": 1000, "cache/tools/docker-28.tgz": 2000} {
			path := filepath.Join(h.fev, name)
			os.MkdirAll(filepath.Dir(path), 0o755)
			os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o755)
		}
	}
	argv := []string{"setup", "--no-tools", "--no-start", "--undo"}
	undoAt := func(answer string) result {
		master, slave := openPty(t)
		defer slave.Close()
		if _, err := master.WriteString(answer); err != nil {
			t.Fatal(err)
		}
		var out, errw lockedBuffer
		code := Main(Env{Name: "fev", Args: append([]string{"--home", h.fev}, argv...), Stdin: slave, Stdout: &out, Stderr: &errw})
		return result{out.String(), errw.String(), code}
	}
	tools, cache := filepath.Join(h.fev, "tools"), filepath.Join(h.fev, "cache", "tools")
	question := "The Docker client tools that setup downloaded are still in " + tools + " and " + cache + " (5.9 KiB). Remove them too? [y/N] "
	kept := func(answer string) {
		t.Helper()
		for _, dir := range []string{tools, cache} {
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("answered %q: %v", answer, err)
			}
		}
	}

	for _, stdin := range []string{"", "y\n"} {
		h.run("--shell", "zsh").want(t, 0)
		downloaded()
		r := fev(t, h.fev, stdin, argv...).want(t, 0)
		if strings.Contains(r.err, "Remove them") || !strings.Contains(r.out, "kept "+tools+" and "+cache+" (5.9 KiB): ") {
			t.Errorf("stdin %q, not a terminal: stderr %q, stdout %q", stdin, r.err, r.out)
		}
		kept(stdin)
	}

	for _, answer := range []string{"\x04", "\n", "n\n", "no thanks\n"} {
		h.run("--shell", "zsh").want(t, 0)
		downloaded()
		r := undoAt(answer).want(t, 0)
		ends := question
		if answer == "\x04" {
			ends += "\n"
		}
		if !strings.HasSuffix(r.err, ends) || !strings.Contains(r.out, "kept "+tools+" and "+cache+"\n") {
			t.Errorf("answered %q: stderr %q, stdout %q", answer, r.err, r.out)
		}
		kept(answer)
	}

	for _, answer := range []string{"y\n", "YES\n"} {
		h.run("--shell", "zsh").want(t, 0)
		downloaded()
		r := undoAt(answer).want(t, 0)
		if !strings.HasSuffix(r.err, question) || !strings.Contains(r.out, "removed "+tools+"\nremoved "+cache+"\n") {
			t.Errorf("answered %q: stderr %q, stdout %q", answer, r.err, r.out)
		}
		if after := snapshot(t, h.user); after != before {
			t.Errorf("answered %q: the home is not as it was before setup:\n%s\n--- was\n%s", answer, after, before)
		}
	}

	h.run("--shell", "zsh").want(t, 0)
	if r := undoAt("y\n").want(t, 0); strings.Contains(r.err, "Remove them") {
		t.Errorf("asked with no tools: %q", r.err)
	}
	h.run("--shell", "zsh").want(t, 0)
	downloaded()
	t.Setenv(ToolsEnv, tools)
	if r := undoAt("y\n").want(t, 0); strings.Contains(r.err, "Remove them") {
		t.Errorf("asked with FEV_TOOLS set: %q", r.err)
	}
	if _, err := os.Stat(tools); err != nil {
		t.Error(err)
	}
}

func TestSetupLeavesTheCredentialStoreToTheDockerCLI(t *testing.T) {
	for _, c := range []struct {
		name, before string
		login        bool
		wantUndone   string
	}{
		{"no config.json", "", false, ""},
		{"no store", "{\n\t\"auths\": {\n\t\t\"example.com\": {}\n\t}\n}", false, ""},
		{"another store", "{\"credsStore\": \"desktop\"}", true, "{\"credsStore\": \"desktop\"}"},
		{"the user's own osxkeychain", "{\"credsStore\": \"osxkeychain\"}", false, "{\"credsStore\": \"osxkeychain\"}"},
		{"a login after setup", "{\n\t\"auths\": {}\n}", true, "{\n\t\"auths\": {}\n}"},
	} {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{}
			if c.before != "" {
				files[".docker/config.json"] = c.before
			}
			h := newSetupHome(t, files)
			before := snapshot(t, h.user)
			config := filepath.Join(h.user, ".docker/config.json")
			r := h.run("--shell", "zsh").want(t, 0)
			got, err := os.ReadFile(config)
			if err != nil {
				t.Fatalf("no config.json after setup: %v\n%s", err, r.out)
			}
			if rest, cur := withoutCurrentContext(t, string(got)); cur != "fev" || rest != withoutCurrentContextOf(t, c.before) {
				t.Fatalf("config.json after setup:\n%s\nwant it as it was, with currentContext fev:\n%s\n%s", got, c.before, r.out)
			}
			if c.login && !strings.Contains(c.before, "desktop") {
				os.WriteFile(config, []byte("{\n\t\"auths\": {\n\t\t\"registry.example\": {}\n\t},\n\t\"credsStore\": \"osxkeychain\"\n}"), 0o600)
			}
			u := h.run("--undo").want(t, 0)
			if c.wantUndone == "" {
				if after := snapshot(t, h.user); after != before {
					t.Errorf("after --undo the home differs:\n%s\n--- was\n%s", after, before)
				}
				return
			}
			got, _ = os.ReadFile(config)
			var g, w map[string]any
			json.Unmarshal(got, &g)
			json.Unmarshal([]byte(c.wantUndone), &w)
			delete(g, "auths")
			delete(w, "auths")
			if !reflect.DeepEqual(g, w) {
				t.Errorf("config.json after --undo:\n%s\nwant the keys of\n%s\n%s", got, c.wantUndone, u.out)
			}
		})
	}
}

func withoutCurrentContext(t *testing.T, text string) (string, string) {
	t.Helper()
	m := map[string]any{}
	if strings.TrimSpace(text) != "" {
		if err := json.Unmarshal([]byte(text), &m); err != nil {
			t.Fatalf("config.json: %v\n%s", err, text)
		}
	}
	cur, _ := m["currentContext"].(string)
	delete(m, "currentContext")
	b, _ := json.Marshal(m)
	return string(b), cur
}

func withoutCurrentContextOf(t *testing.T, text string) string {
	rest, _ := withoutCurrentContext(t, text)
	return rest
}
