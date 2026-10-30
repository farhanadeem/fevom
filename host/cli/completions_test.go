package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fev/host/daemon/home"
	api "fev/proto/daemon"
)

func TestCompletionsAreWrittenFoundAndLoaded(t *testing.T) {
	h := newSetupHome(t, nil)
	h.run("--shell", "zsh", "--shell", "bash", "--shell", "fish").want(t, 0)
	dir := filepath.Join(h.fev, "completions")
	for _, shell := range []string{"bash", "zsh", "fish"} {
		for _, program := range completionPrograms {
			path := completionFile(h.fev, shell, program)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if !strings.Contains(string(data), program+" __complete") && !strings.Contains(string(data), "__complete") {
				t.Errorf("%s does not complete through %s __complete", path, program)
			}
		}
	}
	env := []string{"HOME=" + h.user, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "USER=" + os.Getenv("USER"), "TERM=dumb"}
	run := func(shell string, argv ...string) string {
		t.Helper()
		cmd := exec.Command(shell, argv...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%s %v: %v\n%s", shell, argv, err, out)
		}
		return string(out)
	}
	for _, program := range completionPrograms {
		out := run("/bin/bash", "-c", ". "+completionFile(h.fev, "bash", program)+" && complete -p "+program)
		if !strings.Contains(out, "__start_"+program) {
			t.Errorf("bash: %s", out)
		}
		out = run("/bin/zsh", "-f", "-c", "autoload -U compinit && compinit -u -D && source "+completionFile(h.fev, "zsh", program)+" && print $_comps["+program+"]")
		if strings.TrimSpace(out) != "_"+program {
			t.Errorf("zsh: %q", out)
		}
	}
	if out := run("/bin/zsh", "-l", "-c", "print $fpath[1]"); strings.TrimSpace(out) != filepath.Join(dir, "zsh") {
		t.Errorf("a login zsh's fpath begins %q", out)
	}
	if out := run("/bin/bash", "-l", "-i", "-c", "complete -p docker fev docker-compose"); strings.Count(out, "complete ") != 3 {
		t.Errorf("an interactive login bash: %s", out)
	}
	if fish, err := exec.LookPath("fish"); err == nil {
		for _, program := range completionPrograms {
			run(fish, "--no-config", "-c", "source "+completionFile(h.fev, "fish", program))
		}
		if out := run(fish, "-l", "-c", "echo $fish_complete_path[1]"); strings.TrimSpace(out) != filepath.Join(dir, "fish") {
			t.Errorf("a login fish's fish_complete_path begins %q", out)
		}
	} else {
		t.Log("fish is not installed: its scripts are written and not loaded here")
	}
	h.run("--undo").want(t, 0)
	if _, err := os.Stat(dir); err == nil {
		t.Error("--undo left the completions")
	}
}

func TestAnOlderBlockIsBroughtUpToDate(t *testing.T) {
	h := newSetupHome(t, map[string]string{".zprofile": "export A=1\n"})
	h.run("--shell", "zsh").want(t, 0)
	profile := filepath.Join(h.user, ".zprofile")
	data, _ := os.ReadFile(profile)
	old := strings.Replace(string(data), completionLines("zsh", h.fev, h.user), "", 1)
	if old == string(data) {
		t.Fatal("no completions line in the block")
	}
	os.WriteFile(profile, []byte(old+"export B=2\n"), 0o600)
	r := h.run("--shell", "zsh").want(t, 0)
	if !strings.Contains(r.out, "brought fev's block up to date") {
		t.Errorf("setup: %s", r.out)
	}
	data, _ = os.ReadFile(profile)
	if !strings.Contains(string(data), "fpath=") || !strings.HasSuffix(string(data), "export B=2\n") || strings.Count(string(data), fenceOpen) != 1 {
		t.Errorf("after: %q", data)
	}
	h.run("--undo").want(t, 0)
	if data, _ := os.ReadFile(profile); string(data) != "export A=1\nexport B=2\n" {
		t.Errorf("after --undo: %q", data)
	}
}

func TestACompletionNeverStartsFevom(t *testing.T) {
	c := newEntryCase(t)
	noEngine := "DOCKER_HOST=unix://" + filepath.Join(c.home, "run", "no-engine-while-fev-is-stopped.sock")
	check := func(what string, wantNoEngine bool, argv ...string) {
		t.Helper()
		c.run("docker", argv...)
		if started := c.wasStarted(); started != "" {
			t.Errorf("%s: fev was started: %s", what, started)
		}
		if len(c.execed) == 0 || c.execed[0] != filepath.Join(c.tools, "docker") {
			t.Fatalf("%s: became %v\n%s", what, c.execed, c.stderr.String())
		}
		got := false
		for _, kv := range c.env {
			got = got || kv == noEngine
		}
		if got != wantNoEngine {
			t.Errorf("%s: pointed at no engine %v, want %v: %v", what, got, wantNoEngine, c.env)
		}
	}
	check("no daemon", true, "__complete", "ps", "")
	check("no daemon, NoDesc", true, "__completeNoDesc", "stop", "")
	check("the script", true, "completion", "zsh")
	t.Setenv("DOCKER_CONTEXT", "fev")
	check("DOCKER_CONTEXT naming fev", true, "__complete", "ps", "")
	t.Setenv("DOCKER_CONTEXT", "")
	c.listen()
	l := home.Layout{Home: c.home}
	check("a daemon with no VM", true, "__complete", "ps", "")
	if err := l.WriteInfo(home.Info{PID: os.Getpid(), VMMPID: 4242}); err != nil {
		t.Fatal(err)
	}
	check("the VM up", false, "__complete", "stop", "")
	os.WriteFile(api.StopMarkPath(c.home), nil, 0o600)
	check("after a stop", true, "__complete", "ps", "")
	if strings.Contains(c.stderr.String(), api.StoppedLine) {
		t.Errorf("a completion after a stop said %q", c.stderr.String())
	}
}

func TestCompletionsLeaveTheInvocationsFlags(t *testing.T) {
	newSetupHome(t, nil)
	home := scratchHome(t)
	started := noDaemon(t)
	r := fev(t, home, "", "--format", "json", "setup", "--no-tools", "--shell", "zsh")
	data, _ := os.ReadFile(started)
	if !strings.Contains(string(data), "--home "+home) {
		t.Errorf("setup started a daemon with %q; want this home, %s\n%+v", data, home, r)
	}
}
