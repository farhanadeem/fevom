package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"fev/host/debugremote/fakeengine"
	api "fev/proto/daemon"
	"fev/proto/relay"
)

type remoteFixture struct {
	*machinesFixture
	fe     *fakeengine.Server
	config string
	sock   string
}

func remoteDaemon(t *testing.T) *remoteFixture {
	t.Helper()
	dir := t.TempDir()
	sockDir, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "e.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fe := fakeengine.New("aarch64", ln)
	t.Cleanup(func() { fe.Close() })

	config := filepath.Join(dir, "docker")
	writeContextMeta(t, config, "remote", "unix://"+sock, "a scratch engine")

	agents := filepath.Join(dir, "agents")
	makeAgentDir(t, agents, "arm64", "aarch64-linux")

	mf := machinesDaemon(t, func(m *Machines) {
		m.DebugDockerConfig = config
		m.DebugDefaultHost = "unix:///nope/default.sock"
		m.DebugAgentDir = agents
		m.holdFor = 2 * time.Second
	})
	return &remoteFixture{machinesFixture: mf, fe: fe, config: config, sock: sock}
}

func writeContextMeta(t *testing.T, config, name, host, desc string) {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	d := filepath.Join(config, "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"Name":      name,
		"Metadata":  map[string]any{"Description": desc},
		"Endpoints": map[string]any{"docker": map[string]any{"Host": host}},
	}
	b, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(d, "meta.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func makeAgentDir(t *testing.T, dir, arch, lockArch string) {
	t.Helper()
	d := filepath.Join(dir, arch)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"fev-debug-agent": "#!/stand-in\n",
		"fev-debug-stage": "#!/stand-in\n",
		"fev-debug":       "#!/stand-in\n",
		"defaultset.lock": "# fev-debug default set (" + lockArch + ")\n/nix/store/aaaa-zsh\n",
		"stamp":           "teststamp\n",
	}
	for n, body := range files {
		if err := os.WriteFile(filepath.Join(d, n), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func attachStream(t *testing.T, home, token string) *net.UnixConn {
	t.Helper()
	c, err := net.Dial("unix", api.SessionSocketPath(home))
	if err != nil {
		t.Fatal(err)
	}
	uc := c.(*net.UnixConn)
	if err := relay.WriteRequest(uc, relay.Request{Kind: relay.Exec, Target: token}); err != nil {
		uc.Close()
		t.Fatal(err)
	}
	uc.SetDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 64)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, flags, _, err := uc.ReadMsgUnix(buf, oob)
	if err != nil {
		uc.Close()
		t.Fatalf("recvmsg: %v", err)
	}
	if flags&syscall.MSG_CTRUNC != 0 || string(buf[:n]) != "ok\n" {
		uc.Close()
		t.Fatalf("reply %q flags %#x", buf[:n], flags)
	}
	msgs, _ := syscall.ParseSocketControlMessage(oob[:oobn])
	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		uc.Close()
		t.Fatalf("rights: %v", err)
	}
	uc.Close()
	fh := os.NewFile(uintptr(fds[0]), "stream")
	sc, err := net.FileConn(fh)
	fh.Close()
	if err != nil {
		t.Fatal(err)
	}
	return sc.(*net.UnixConn)
}

func TestContextsListReadsTheConfiguration(t *testing.T) {
	f := remoteDaemon(t)
	list, err := f.c.Contexts(bg)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]api.RemoteContext{}
	for _, c := range list.Contexts {
		byName[c.Name] = c
	}
	if _, ok := byName["default"]; !ok {
		t.Errorf("no default context: %+v", list.Contexts)
	}
	r, ok := byName["remote"]
	if !ok {
		t.Fatalf("no remote context: %+v", list.Contexts)
	}
	if r.Host != "unix://"+f.sock || r.Description != "a scratch engine" {
		t.Errorf("remote context wrong: %+v", r)
	}
}

func TestContextsListIsEmptyWithoutConfiguration(t *testing.T) {
	f := machinesDaemon(t)
	list, err := f.client().Contexts(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Contexts) != 0 {
		t.Fatalf("want no contexts, got %+v", list.Contexts)
	}
}

func TestContextContainersAndImagesOnDemand(t *testing.T) {
	f := remoteDaemon(t)
	f.fe.AddContainer("api", "ghcr.io/x/api:latest", true)
	f.fe.AddContainer("db", "postgres:17", false)
	f.fe.AddImage("ghcr.io/x/api:latest")

	cs, err := f.c.ContextContainers(bg, "remote")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Containers) != 2 {
		t.Fatalf("want 2 containers (all states), got %+v", cs.Containers)
	}
	states := map[string]string{}
	for _, c := range cs.Containers {
		states[c.Name] = c.State
	}
	if states["api"] != "running" || states["db"] != "exited" {
		t.Errorf("states: %+v", states)
	}
	im, err := f.c.ContextImages(bg, "remote")
	if err != nil {
		t.Fatal(err)
	}
	if len(im.Images) != 1 || len(im.Images[0].RepoTags) != 1 || im.Images[0].RepoTags[0] != "ghcr.io/x/api:latest" {
		t.Fatalf("want 1 tagged image, got %+v", im.Images)
	}
}

func TestContextErrorsAreOneLine(t *testing.T) {
	f := remoteDaemon(t)
	_, err := f.c.ContextContainers(bg, "nope")
	if err == nil || !strings.Contains(err.Error(), "no Docker context") {
		t.Fatalf("a missing context: %v", err)
	}
	writeContextMeta(t, f.config, "dead", "unix:///no/such/socket.sock", "")
	_, err = f.c.ContextContainers(bg, "dead")
	if err == nil || !strings.Contains(err.Error(), "reaching context") {
		t.Fatalf("an unreachable context: %v", err)
	}
	if strings.ContainsAny(err.Error(), "\n") {
		t.Errorf("the error is more than one line: %q", err.Error())
	}
}

func TestARemoteDebugSessionStreamsThroughTheTerminal(t *testing.T) {
	f := remoteDaemon(t)
	f.fe.AddContainer("api", "ghcr.io/x/api:latest", true)
	exits := make(chan api.SessionExit, 1)
	started, err := f.c.DebugStart(bg, api.DebugStartParams{
		Target: "api", Context: "remote", Kind: "container", TTY: true, Rows: 24, Cols: 80,
		Env: []string{"TERM=xterm-256color"},
	}, func(e api.SessionExit) { exits <- e })
	if err != nil {
		t.Fatal(err)
	}
	if started.Kind != "remote" || started.Name != "api" {
		t.Fatalf("started: %+v", started)
	}
	if n := f.m.sessions.openDebug(); n != 0 {
		t.Fatalf("a remote session counted as a local one: %d", n)
	}
	conn := attachStream(t, f.layout.Home, started.Token)
	defer conn.Close()
	if _, err := conn.Write([]byte("marco\n")); err != nil {
		t.Fatal(err)
	}
	got := readUntil(t, conn, "marco", 10*time.Second)
	if !strings.Contains(got, "debug shell ready") {
		t.Errorf("no banner in the stream: %q", got)
	}
	conn.Close()
	select {
	case e := <-exits:
		if e.ExitCode != 0 && e.Error == "" {
			t.Errorf("unexpected exit: %+v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the remote session did not end after the stream closed")
	}
}

func TestARemoteStoppedContainerSaysItIsReadOnly(t *testing.T) {
	f := remoteDaemon(t)
	f.fe.AddContainer("old", "busybox:1.37", false)
	exits := make(chan api.SessionExit, 1)
	started, err := f.c.DebugStart(bg, api.DebugStartParams{
		Target: "old", Context: "remote", Kind: "container", TTY: true, Rows: 24, Cols: 80,
	}, func(e api.SessionExit) { exits <- e })
	if err != nil {
		t.Fatal(err)
	}
	conn := attachStream(t, f.layout.Home, started.Token)
	defer conn.Close()
	conn.Write([]byte("exit\n"))
	got := readUntil(t, conn, "read-only", 10*time.Second)
	if !strings.Contains(got, "read-only") {
		t.Errorf("no read-only notice in the stream: %q", got)
	}
	conn.Close()
	select {
	case <-exits:
	case <-time.After(10 * time.Second):
		t.Fatal("the session did not end")
	}
}

func readUntil(t *testing.T, conn net.Conn, want string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	var b strings.Builder
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := conn.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
			if strings.Contains(b.String(), want) {
				return b.String()
			}
		}
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			break
		}
	}
	return b.String()
}

func TestContextsListLeavesOutFevomsOwnEngine(t *testing.T) {
	f := remoteDaemon(t)
	ownDir, err := os.MkdirTemp("/tmp", "bv.own.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(ownDir) })
	ownSock := filepath.Join(ownDir, "docker.sock")
	ln, err := net.Listen("unix", ownSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f.m.DebugOwnDockerHost = ownSock

	writeContextMeta(t, f.config, "fev", "unix://"+ownSock, "fev's own engine")
	linkDir, err := os.MkdirTemp("/tmp", "bv.lnk.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(linkDir) })
	link := filepath.Join(linkDir, "docker.sock")
	if err := os.Symlink(ownSock, link); err != nil {
		t.Fatal(err)
	}
	f.m.DebugDefaultHost = "unix://" + link

	list, err := f.c.Contexts(bg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list.Contexts {
		if c.Name == "fev" {
			t.Errorf("fev's own context was listed: %+v", c)
		}
		if c.Name == "default" {
			t.Errorf("the default context resolving to fev's own socket was listed: %+v", c)
		}
	}
	found := false
	for _, c := range list.Contexts {
		if c.Name == "remote" {
			found = true
		}
	}
	if !found {
		t.Errorf("the genuine remote context was dropped: %+v", list.Contexts)
	}
	if _, err := f.m.remoteContext("fev"); err == nil {
		t.Errorf("remoteContext(fev) did not refuse fev's own engine")
	}
	if _, err := f.m.remoteContext("remote"); err != nil {
		t.Errorf("remoteContext(remote) refused a genuine engine: %v", err)
	}
}
