package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fev/host/daemon/doctor"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func doctorWorld(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("SHELL", "/bin/zsh")
	old := systemDockerSock
	systemDockerSock = filepath.Join(t.TempDir(), "docker.sock")
	t.Cleanup(func() { systemDockerSock = old })
}

func setUpHome(t *testing.T, h string) string {
	t.Helper()
	user := t.TempDir()
	t.Setenv("HOME", user)
	dc := filepath.Join(user, ".docker")
	t.Setenv("DOCKER_CONFIG", dc)
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	os.MkdirAll(filepath.Join(h, "bin"), 0o755)
	for _, name := range selfNames {
		os.Symlink(self, filepath.Join(h, "bin", name))
	}
	t.Setenv(ToolsEnv, t.TempDir())
	for _, name := range []string{"docker", "docker-compose", "docker-buildx", "docker-credential-osxkeychain", "kubectl"} {
		os.WriteFile(filepath.Join(os.Getenv(ToolsEnv), name), []byte("#!/bin/sh\n"), 0o755)
	}
	os.WriteFile(filepath.Join(h, "setup.json"), []byte("{}\n"), 0o644)
	t.Setenv("PATH", filepath.Join(h, "bin")+":/usr/bin:/bin")
	os.MkdirAll(dc, 0o700)
	os.WriteFile(filepath.Join(dc, "config.json"), []byte(`{"currentContext":"fev"}`), 0o600)
	writeContext(t, dc, "fev", "unix://"+filepath.Join(h, "run", "docker.sock"))
	return dc
}

func TestDoctorOnAHomeNotSetUp(t *testing.T) {
	doctorWorld(t)
	noDaemon(t)
	h := scratchHome(t)
	r := fev(t, h, "", "doctor").want(t, 0)
	if !strings.Contains(r.out, "warning: fev is not set up in this home") || strings.Contains(r.out, "--fix") {
		t.Errorf("doctor:\n%s", r.out)
	}
	if r := fev(t, h, "", "-q", "doctor").want(t, 0); r.out != "" {
		t.Errorf("quiet: %q", r.out)
	}
	r = fev(t, h, "", "--format", "json", "doctor").want(t, 0)
	var rep doctor.Report
	if err := json.Unmarshal([]byte(r.out), &rep); err != nil || rep.Running || len(rep.Findings) != 1 || rep.Findings[0].ID != "path.not_set_up" {
		t.Errorf("json %s: %v", r.out, err)
	}
}

func TestDoctorFollowsARecoveredDataDisk(t *testing.T) {
	doctorWorld(t)
	f := newFake(t)
	setUpHome(t, f.home)
	recovered := doctor.RecoveredPrefix + " when the VM started (boot b7) and was recovered (step 3 of 5, its log dropped)."
	f.on(api.MethodStatus, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return api.Status{Seq: 9, State: api.StateRunning, Engine: api.EngineFailed, BootID: "b7", Since: "2026-10-19T10:00:00Z",
			PendingRestart: []string{}, Error: "the Docker engine could not start: read-only file system", Warnings: []string{recovered}}, nil
	})
	r := f.run("doctor").want(t, ExitFailed)
	for _, want := range []string{"problem: the Docker engine failed to start: the Docker engine could not start: read-only file system",
		"problem: " + recovered, "--fix checks the data disk", "2 problem(s)"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("doctor does not say %q:\n%s", want, r.out)
		}
	}
	if f.called(api.MethodStorageCheck) {
		t.Error("doctor without --fix checked the disk")
	}
	f.on(api.MethodStorageCheck, func(c *jsonrpc.Conn, params json.RawMessage) (any, error) {
		op := api.Operation{ID: "op-9", Method: api.MethodStorageCheck, Target: "data", State: api.OperationRunning, Phase: api.PhaseScrub, Started: "2026-10-19T10:00:00Z"}
		end := op
		end.State, end.Ended = api.OperationDone, "2026-10-19T10:00:04Z"
		end.Result, _ = json.Marshal(api.StorageChecked{ScrubbedBytes: 1 << 30, Corrected: 2, Uncorrectable: 3})
		go c.Notify(api.NotifyOperationProgress, api.OperationProgress{Subscription: 2, Operation: end})
		return op, nil
	})
	r = f.run("doctor", "--fix").want(t, ExitFailed)
	for _, want := range []string{"fixed: checks the data disk", "data disk checked: 1.0 GiB read, 2 sector(s) repaired, 3 that no copy could repair\n",
		"problem: " + recovered} {
		if !strings.Contains(r.out, want) {
			t.Errorf("doctor --fix does not say %q:\n%s", want, r.out)
		}
	}
	if got := f.params(api.MethodStorageCheck); got != "{}" {
		t.Errorf("asked %s", got)
	}
	r = f.run("--format", "json", "-q", "doctor").want(t, ExitFailed)
	var rep doctor.Report
	if err := json.Unmarshal([]byte(r.out), &rep); err != nil || !rep.Running || rep.Problems() != 2 {
		t.Errorf("json %s: %v", r.out, err)
	}
}

func TestDoctorFixesTheContextAndStaleSockets(t *testing.T) {
	doctorWorld(t)
	started := noDaemon(t)
	h := scratchHome(t)
	dc := setUpHome(t, h)
	other := filepath.Join(scratchHome(t), "o.sock")
	ln, err := net.Listen("unix", other)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	os.WriteFile(filepath.Join(dc, "config.json"), []byte(`{"currentContext":"other"}`), 0o600)
	writeContext(t, dc, "other", "unix://"+other)
	os.MkdirAll(filepath.Join(h, "run"), 0o700)
	sock := filepath.Join(h, "run", "docker.sock")
	leaveSocket(t, sock)
	r := fev(t, h, "", "doctor").want(t, 0)
	for _, want := range []string{"warning: the current Docker context is other", "warning: 1 socket(s) left by a daemon that is gone"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("doctor does not say %q:\n%s", want, r.out)
		}
	}
	r = fev(t, h, "", "doctor", "--fix").want(t, 0)
	if !strings.Contains(r.out, "fixed: makes the fev Docker context the current one") || !strings.Contains(r.out, "fixed: removes the stale sockets") ||
		!strings.Contains(r.out, "Nothing found") {
		t.Errorf("doctor --fix:\n%s\n%s", r.out, r.err)
	}
	if data, _ := os.ReadFile(filepath.Join(dc, "config.json")); !strings.Contains(string(data), `"currentContext": "fev"`) {
		t.Errorf("config.json: %s", data)
	}
	if data, _ := os.ReadFile(filepath.Join(h, "config", "docker-context.json")); !strings.Contains(string(data), `"previous":"other"`) {
		t.Errorf("the previous context is not remembered: %s", data)
	}
	if _, err := os.Stat(sock); err == nil {
		t.Error("the stale socket is still there")
	}
	if _, err := os.Stat(started); err == nil {
		t.Error("doctor started a daemon")
	}
}

func writeContext(t *testing.T, dir, name, host string) {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	meta := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]))
	os.MkdirAll(meta, 0o755)
	data := `{"Name":"` + name + `","Metadata":{},"Endpoints":{"docker":{"Host":"` + host + `","SkipTLSVerify":false}}}`
	if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func leaveSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
}
