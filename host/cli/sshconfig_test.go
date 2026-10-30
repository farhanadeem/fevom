package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupIncludesTheSSHEntryAndUndoesIt(t *testing.T) {
	existing := "Host work\n  HostName work.example\n  User me\n\nHost *\n  ServerAliveInterval 30\n"
	for _, c := range []struct {
		name   string
		files  map[string]string
		edited string
	}{
		{"no ~/.ssh", nil, ".ssh/config"},
		{"~/.ssh and no config", map[string]string{".ssh/known_hosts": "x ssh-ed25519 AAAA\n"}, ".ssh/config"},
		{"a config with hosts", map[string]string{".ssh/config": existing}, ".ssh/config"},
		{"a config with no trailing newline", map[string]string{".ssh/config": "Host a\n  User b"}, ".ssh/config"},
		{"an empty config", map[string]string{".ssh/config": ""}, ".ssh/config"},
		{"the config is a link", map[string]string{"dotfiles/ssh_config": existing, ".ssh/config": "-> ../dotfiles/ssh_config"}, "dotfiles/ssh_config"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newSetupHome(t, c.files)
			before := snapshot(t, h.user)
			h.run("--shell", "zsh").want(t, 0)
			data, err := os.ReadFile(filepath.Join(h.user, c.edited))
			if err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(h.fev, "ssh", "config")
			if !strings.HasPrefix(string(data), sshFenceOpen+"\n") || !strings.Contains(string(data), "\nInclude \""+entry+"\"\n") {
				t.Fatalf("%s:\n%s", c.edited, data)
			}
			if orig, ok := c.files[c.edited]; ok && !strings.HasSuffix(string(data), sshFenceClose+"\n"+orig) {
				t.Errorf("the user's lines are not kept after the block:\n%s", data)
			}
			if c.files == nil {
				info, _ := os.Stat(filepath.Join(h.user, ".ssh"))
				file, _ := os.Stat(filepath.Join(h.user, ".ssh", "config"))
				if info.Mode().Perm() != 0o700 || file.Mode().Perm() != 0o600 {
					t.Errorf("~/.ssh %v, config %v", info.Mode().Perm(), file.Mode().Perm())
				}
			}
			if link, ok := c.files[".ssh/config"]; ok && strings.HasPrefix(link, "-> ") {
				if fi, _ := os.Lstat(filepath.Join(h.user, ".ssh", "config")); fi.Mode()&os.ModeSymlink == 0 {
					t.Error("the link was replaced by a file")
				}
			}
			once := snapshot(t, h.user)
			h.run("--shell", "zsh").want(t, 0)
			if twice := snapshot(t, h.user); twice != once {
				t.Errorf("a second setup changed something:\n%s\n--- was\n%s", twice, once)
			}
			h.run("--shell", "zsh", "--undo").want(t, 0)
			if after := snapshot(t, h.user); after != before {
				t.Errorf("after --undo the home differs:\n%s\n--- was\n%s", after, before)
			}
		})
	}
}

func TestSetupDoesNotPutBackARemovedSSHInclude(t *testing.T) {
	h := newSetupHome(t, map[string]string{".ssh/config": "Host a\n  User b\n"})
	h.run("--shell", "zsh").want(t, 0)
	path := filepath.Join(h.user, ".ssh", "config")
	os.WriteFile(path, []byte("Host a\n  User c\n"), 0o600)
	h.run("--shell", "zsh").want(t, 0)
	if data, _ := os.ReadFile(path); string(data) != "Host a\n  User c\n" {
		t.Errorf("a second setup put the block back:\n%s", data)
	}
	h.run("--shell", "zsh", "--undo").want(t, 0)
	if data, _ := os.ReadFile(path); string(data) != "Host a\n  User c\n" {
		t.Errorf("--undo changed the user's file:\n%s", data)
	}
}

func TestOpenSSHReadsTheIncludedEntry(t *testing.T) {
	h := newSetupHome(t, map[string]string{".ssh/config": "Host *\n  User someoneelse\n  Port 2200\n"})
	h.run("--shell", "zsh").want(t, 0)
	os.MkdirAll(filepath.Join(h.fev, "ssh"), 0o700)
	os.WriteFile(filepath.Join(h.fev, "ssh", "config"), []byte("Host fev\n  HostName 127.0.0.1\n  Port 32122\n  User default\n"), 0o600)
	out, err := runSSHG(h.user, "fev")
	if err != nil {
		t.Skipf("ssh -G: %v", err)
	}
	for _, want := range []string{"\nuser default\n", "\nport 32122\n", "\nhostname 127.0.0.1\n"} {
		if !strings.Contains("\n"+out, want) {
			t.Errorf("ssh -G fev lacks %q:\n%s", strings.TrimSpace(want), out)
		}
	}
}

func runSSHG(home, host string) (string, error) {
	cmd := exec.Command("ssh", "-F", filepath.Join(home, ".ssh", "config"), "-G", host)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	return string(out), err
}

func TestSSHPrintsTheDetails(t *testing.T) {
	f := newFake(t)
	r := f.run("ssh").want(t, 0)
	golden(t, "ssh", r.out)
	sameJSON(t, f.run("ssh", "--format", "json").want(t, 0).out, fixtureResult(t, "ssh.info.exposed"))
	if r := f.run("ssh", "--proxy"); r.code != ExitFailed || !strings.Contains(r.err, "ProxyCommand") {
		t.Errorf("ssh --proxy by hand: %d %q", r.code, r.err)
	}
}
