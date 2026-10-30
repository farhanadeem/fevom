package doctor

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fev/host/daemon/home"
	api "fev/proto/daemon"
)

type world struct {
	t    *testing.T
	root string
	env  map[string]string
	e    Env
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "dr.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	root, _ = filepath.EvalSymlinks(root)
	user := filepath.Join(root, "u")
	l := home.Layout{Home: filepath.Join(user, ".fev"), DataDir: filepath.Join(user, ".fev", "data")}
	w := &world{t: t, root: root, env: map[string]string{}}
	self := filepath.Join(root, "app", "fev")
	w.write(self, "#!/bin/sh\n", 0o755)
	tools := filepath.Join(l.Home, "tools")
	for _, name := range []string{"docker", "docker-compose"} {
		w.write(filepath.Join(tools, name), "#!/bin/sh\n", 0o755)
	}
	for _, name := range []string{"fev", "docker", "docker-compose"} {
		w.link(self, filepath.Join(l.Home, "bin", name))
	}
	os.MkdirAll(l.RunDir(), 0o700)
	w.env["PATH"] = filepath.Join(l.Home, "bin") + ":/usr/bin:/bin"
	w.e = Env{Layout: l, UserHome: user, Getenv: func(k string) string { return w.env[k] }, Self: self, Tools: tools,
		ToolNames: []string{"docker", "docker-compose"}, LinkNames: []string{"fev", "docker", "docker-compose"}, SetUp: true,
		DockerSock: filepath.Join(root, "var-run-docker.sock")}
	w.context("fev", "unix://"+filepath.Join(l.RunDir(), "docker.sock"))
	w.write(filepath.Join(user, ".docker", "config.json"), "{\n\t\"currentContext\": \"fev\"\n}", 0o600)
	return w
}

func (w *world) write(path, data string, mode os.FileMode) {
	w.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) link(target, path string) {
	w.t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.Remove(path)
	if err := os.Symlink(target, path); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) context(name, host string) {
	sum := sha256.Sum256([]byte(name))
	w.write(filepath.Join(w.e.UserHome, ".docker", "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json"),
		`{"Name":"`+name+`","Metadata":{},"Endpoints":{"docker":{"Host":"`+host+`","SkipTLSVerify":false}}}`, 0o644)
}

func (w *world) found() map[string]Finding {
	w.t.Helper()
	got := map[string]Finding{}
	for _, f := range Check(w.e).Findings {
		if _, dup := got[f.ID]; dup && f.ID != "daemon.warning" {
			w.t.Errorf("%s found twice", f.ID)
		}
		got[f.ID] = f
	}
	return got
}

func (w *world) only(ids ...string) map[string]Finding {
	w.t.Helper()
	got := w.found()
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
		if _, ok := got[id]; !ok {
			w.t.Errorf("%s not found; found %v", id, got)
		}
	}
	for id, f := range got {
		if !want[id] {
			w.t.Errorf("found %s, not expected: %+v", id, f)
		}
	}
	return got
}

func TestASetUpFevomHasNothingToFind(t *testing.T) {
	newWorld(t).only()
}

func TestPathProblemsAreFoundAndNamed(t *testing.T) {
	w := newWorld(t)
	w.env["PATH"] = "/usr/bin:/bin"
	if f := w.only("path.missing")["path.missing"]; f.Severity != Problem || f.Fix != FixSetup {
		t.Errorf("%+v", f)
	}
	w.e.SetUp = false
	if f := w.only("path.not_set_up")["path.not_set_up"]; f.Severity != Warning || f.Fix != "" || !strings.Contains(f.Advice, "fev setup") {
		t.Errorf("%+v", f)
	}

	w = newWorld(t)
	desktop := filepath.Join(w.root, "Applications", "Docker.app", "Contents", "Resources", "bin", "docker")
	w.write(desktop, "#!/bin/sh\n", 0o755)
	w.link(desktop, filepath.Join(w.root, "usr-local-bin", "docker"))
	w.env["PATH"] = filepath.Join(w.root, "usr-local-bin") + ":" + w.env["PATH"]
	f := w.only("path.shadowed")["path.shadowed"]
	if f.Severity != Problem || !strings.Contains(f.Text, "Docker Desktop") || !strings.Contains(f.Text, "usr-local-bin/docker") || f.Fix != "" {
		t.Errorf("%+v", f)
	}
	w.env["PATH"] = filepath.Join(w.e.Layout.Home, "bin") + ":" + filepath.Join(w.root, "usr-local-bin")
	w.only()
}

func TestWhereDockerCommandsGoIsChecked(t *testing.T) {
	w := newWorld(t)
	w.env["DOCKER_HOST"] = "tcp://10.0.0.5:2375"
	w.write(filepath.Join(w.e.UserHome, ".zshrc"), "export DOCKER_HOST=tcp://10.0.0.5:2375\n", 0o644)
	if f := w.only("docker.host_env")["docker.host_env"]; !strings.Contains(f.Advice, ".zshrc") || f.Fix != "" {
		t.Errorf("%+v", f)
	}
	w.env["DOCKER_HOST"] = "unix://" + filepath.Join(w.e.Layout.RunDir(), "docker.sock")
	w.only()
	delete(w.env, "DOCKER_HOST")
	w.env["DOCKER_CONTEXT"] = "colima"
	w.only("docker.context_env")
	delete(w.env, "DOCKER_CONTEXT")

	other := filepath.Join(w.root, "other.sock")
	ln, err := net.Listen("unix", other)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	w.context("other", "unix://"+other)
	w.write(filepath.Join(w.e.UserHome, ".docker", "config.json"), `{"auths":{},"currentContext":"other"}`, 0o600)
	if f := w.only("docker.context_other")["docker.context_other"]; f.Severity != Warning || f.Fix != FixContext {
		t.Errorf("%+v", f)
	}
	ln.Close()
	os.Remove(other)
	if f := w.only("docker.context_other")["docker.context_other"]; f.Severity != Problem || !strings.Contains(f.Text, "is not there") {
		t.Errorf("a selected context whose socket is gone: %+v", f)
	}
	w.write(filepath.Join(w.e.UserHome, ".docker", "config.json"), `{"currentContext":"nothing"}`, 0o600)
	w.only("docker.context_other", "docker.contexts_stale")
	w.write(filepath.Join(w.e.UserHome, ".docker", "config.json"), `{}`, 0o600)
	w.only("docker.context_default", "docker.contexts_stale")
	w.write(filepath.Join(w.e.UserHome, ".docker", "config.json"), `{"currentContext":"fev"}`, 0o600)
	if f := w.only("docker.contexts_stale")["docker.contexts_stale"]; f.Severity != Info || !strings.Contains(f.Text, "other (") {
		t.Errorf("%+v", f)
	}
	w.context("fev", "unix:///Users/someone/.fev-old/run/docker.sock")
	w.only("docker.context_outdated", "docker.contexts_stale")
	w.write(filepath.Join(w.e.UserHome, ".docker", "config.json"), `{"currentContext":`, 0o600)
	w.only("docker.config_unreadable")

	w = newWorld(t)
	w.env["DOCKER_CONFIG"] = filepath.Join(w.root, "dc")
	w.write(filepath.Join(w.root, "dc", "config.json"), `{"currentContext":"fev"}`, 0o600)
	w.only("docker.context_missing")
}

func TestStaleSocketsAreFoundAndRemoved(t *testing.T) {
	w := newWorld(t)
	path := filepath.Join(w.e.Layout.RunDir(), "docker.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	w.write(filepath.Join(w.e.Layout.RunDir(), "fevd.json"), "{}", 0o600)
	if f := w.only("daemon.stale_sockets")["daemon.stale_sockets"]; f.Fix != FixSockets || !strings.Contains(f.Text, path) {
		t.Errorf("%+v", f)
	}
	lock, held, err := home.Lock(w.e.Layout.DaemonLock(), false)
	if err != nil || held {
		t.Fatal(err, held)
	}
	w.only("daemon.unresponsive")
	if removed, err := RemoveStaleSockets(w.e.Layout); err != nil || len(removed) != 0 {
		t.Errorf("removed %v under a live daemon's lock: %v", removed, err)
	}
	w.e.Answered = true
	w.only()
	lock.Close()
	w.e.Answered = false
	removed, err := RemoveStaleSockets(w.e.Layout)
	if err != nil || len(removed) != 1 || removed[0] != path {
		t.Errorf("removed %v: %v", removed, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the stale socket is still there")
	}
	if _, err := os.Stat(filepath.Join(w.e.Layout.RunDir(), "fevd.json")); err != nil {
		t.Error("a file that is not a socket was removed")
	}
	w.only()
}

func TestSetupsLinksAndToolsAreChecked(t *testing.T) {
	w := newWorld(t)
	os.Remove(filepath.Join(w.e.Layout.Home, "bin", "docker-compose"))
	os.Remove(filepath.Join(w.e.Tools, "docker"))
	got := w.only("setup.links_missing", "setup.tools_missing")
	if f := got["setup.links_missing"]; !strings.Contains(f.Text, "docker-compose") || f.Fix != FixSetup {
		t.Errorf("%+v", f)
	}
	w = newWorld(t)
	old := filepath.Join(w.root, "old", "fev")
	w.write(old, "#!/bin/sh\n", 0o755)
	w.link(old, filepath.Join(w.e.Layout.Home, "bin", "fev"))
	w.only("setup.links_elsewhere")
}

func TestTheSystemDockerSocketIsChecked(t *testing.T) {
	w := newWorld(t)
	w.link(filepath.Join(w.root, "gone", "docker.sock"), w.e.DockerSock)
	w.only("docker.system_socket_stale")
	other := filepath.Join(w.root, "Applications", "Docker.app", "docker.sock")
	w.write(other, "", 0o600)
	w.link(other, w.e.DockerSock)
	if f := w.only("docker.system_socket_other")["docker.system_socket_other"]; !strings.Contains(f.Text, "Docker Desktop") || f.Severity != Info {
		t.Errorf("%+v", f)
	}
	w.link(filepath.Join(w.e.Layout.RunDir(), "docker.sock"), w.e.DockerSock)
	w.only()
}

func TestTheDaemonsOwnWordIsAFinding(t *testing.T) {
	w := newWorld(t)
	w.e.Answered = true
	w.e.Status = &api.Status{State: api.StateRunning, Engine: api.EngineFailed, Error: "dockerd: failed to start containerd",
		Warnings: []string{RecoveredPrefix + " when the VM started (boot b3) and was recovered (step 3 of 5).", "port 8080 is taken"}}
	got := w.found()
	if f := got["daemon.engine_failed"]; f.Severity != Problem || !strings.Contains(f.Text, "containerd") {
		t.Errorf("%+v", f)
	}
	if f := got["data.recovered"]; f.Severity != Problem || f.Fix != FixCheck {
		t.Errorf("%+v", f)
	}
	if f := got["daemon.warning"]; f.Text != "port 8080 is taken" || f.Fix != "" {
		t.Errorf("%+v", f)
	}
	w.e.Status = &api.Status{State: api.StateStarting, Phase: api.PhaseEngine, Engine: api.EngineFailed,
		Error: "the Docker engine failed: the data filesystem is mounted read-only", Warnings: []string{DamagedPrefix + " (boot b8)."}}
	if f := w.found()["data.damaged"]; f.Fix != "" || f.Severity != Problem || !strings.Contains(f.Advice, "fev salvage DIR") || !strings.Contains(f.Advice, "fev reset") {
		t.Errorf("rung 4 with the engine held: %+v", f)
	}
	w.e.Status.Warnings = []string{RecoveredPrefix + " when the VM started (boot b8) and was recovered (step 3 of 5)."}
	if f := w.found()["data.recovered"]; f.Fix != FixCheck || f.Severity != Problem {
		t.Errorf("rung 3 with the engine held: %+v", f)
	}
	w.e.Status = &api.Status{State: api.StateStopped, Engine: api.EngineStopped, Error: "the VMM exited with status 1",
		Warnings: []string{RecoveredPrefix + " ..."}}
	got = w.found()
	if f := got["data.recovered"]; f.Fix != "" {
		t.Errorf("a check offered with the VM stopped: %+v", f)
	}
	if _, ok := got["daemon.vm_failed"]; !ok {
		t.Errorf("no word of the VM's error: %v", got)
	}
}
