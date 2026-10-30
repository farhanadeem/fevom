package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"fev/host/daemon/home"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func TestTheEntryPointReadsDockersGlobalFlags(t *testing.T) {
	for _, c := range []struct {
		name, argv string
		want       globals
	}{
		{"docker", "ps -a", globals{rest: []string{"ps", "-a"}}},
		{"docker", "-H unix:///x ps", globals{host: "unix:///x", rest: []string{"ps"}}},
		{"docker", "--host=tcp://h:1 -H unix:///y ps", globals{host: "tcp://h:1", rest: []string{"ps"}}},
		{"docker", "-Hunix:///x ps", globals{host: "unix:///x", rest: []string{"ps"}}},
		{"docker", "--context other ps", globals{context: "other", rest: []string{"ps"}}},
		{"docker", "-c other -D --log-level debug ps", globals{context: "other", rest: []string{"ps"}}},
		{"docker", "--config /d --context=other run -H nothing", globals{context: "other", config: "/d", rest: []string{"run", "-H", "nothing"}}},
		{"docker", "run -c 512 --context x alpine", globals{rest: []string{"run", "-c", "512", "--context", "x", "alpine"}}},
		{"docker", "--tlsverify --tlscacert ca.pem -H tcp://h:2376 info", globals{host: "tcp://h:2376", rest: []string{"info"}}},
		{"docker", "", globals{}},
		{"docker", "--version", globals{}},
		{"docker-compose", "-f a.yml -p proj --context other up -d", globals{context: "other", rest: []string{"up", "-d"}}},
		{"docker-compose", "--file=a.yml -H unix:///x ps", globals{host: "unix:///x", rest: []string{"ps"}}},
		{"docker-compose", "exec web sh -c ls", globals{rest: []string{"exec", "web", "sh", "-c", "ls"}}},
	} {
		got := parseGlobals(c.name, strings.Fields(c.argv))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %s: %+v, want %+v", c.name, c.argv, got, c.want)
		}
	}
}

func dockerConfig(t *testing.T, current string, contexts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if current != "" {
		os.WriteFile(filepath.Join(dir, "config.json"), []byte(fmt.Sprintf(`{"auths": {}, "currentContext": %q}`, current)), 0o600)
	}
	for name, host := range contexts {
		sum := sha256.Sum256([]byte(name))
		meta := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]))
		os.MkdirAll(meta, 0o755)
		os.WriteFile(filepath.Join(meta, "meta.json"),
			[]byte(fmt.Sprintf(`{"Name":%q,"Metadata":{},"Endpoints":{"docker":{"Host":%q,"SkipTLSVerify":false}}}`, name, host)), 0o644)
	}
	return dir
}

func TestTheEntryPointFollowsDockersOrder(t *testing.T) {
	cfg := dockerConfig(t, "fev", map[string]string{"fev": "unix:///b/run/docker.sock", "other": "unix:///o/docker.sock"})
	none := dockerConfig(t, "", nil)
	gone := dockerConfig(t, "vanished", nil)
	for _, c := range []struct {
		argv, host, context, config string
		want                        string
	}{
		{"ps", "", "", cfg, "unix:///b/run/docker.sock"},
		{"ps", "", "other", cfg, "unix:///o/docker.sock"},
		{"ps", "tcp://e:1", "other", cfg, "tcp://e:1"},
		{"--context other ps", "tcp://e:1", "fev", cfg, "unix:///o/docker.sock"},
		{"-H tcp://f:2 --context other ps", "tcp://e:1", "fev", cfg, "tcp://f:2"},
		{"--context default ps", "", "", cfg, defaultHost},
		{"ps", "", "", none, defaultHost},
		{"--config " + cfg + " ps", "", "", none, "unix:///b/run/docker.sock"},
		{"ps", "", "", gone, ""},
		{"--context nosuch ps", "", "", cfg, ""},
	} {
		t.Setenv("DOCKER_HOST", c.host)
		t.Setenv("DOCKER_CONTEXT", c.context)
		t.Setenv("DOCKER_CONFIG", c.config)
		if got := headed(parseGlobals("docker", strings.Fields(c.argv)), "/nonexistent"); got != c.want {
			t.Errorf("docker %s with DOCKER_HOST=%q DOCKER_CONTEXT=%q: headed for %q, want %q", c.argv, c.host, c.context, got, c.want)
		}
	}
	t.Setenv("DOCKER_CONFIG", "")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	user := t.TempDir()
	os.Rename(cfg, filepath.Join(user, ".docker"))
	if got := headed(globals{}, user); got != "unix:///b/run/docker.sock" {
		t.Errorf("~/.docker: headed for %q", got)
	}
}

func TestFevomsSocketIsKnownThroughALink(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "home", "run", "docker.sock")
	os.MkdirAll(filepath.Dir(socket), 0o755)
	link := filepath.Join(dir, "docker.sock")
	os.Symlink(socket, link)
	dirLink := filepath.Join(dir, "h")
	os.Symlink(filepath.Join(dir, "home"), dirLink)
	check := func(when string) {
		for host, want := range map[string]bool{
			"unix://" + socket:                           true,
			"unix://" + link:                             true,
			"unix://" + dirLink + "/run/docker.sock":     true,
			"unix://" + filepath.Join(dir, "other.sock"): false,
			"unix://":              false,
			"tcp://127.0.0.1:2375": false,
			"ssh://host":           false,
			"":                     false,
			socket:                 false,
			"unix://" + filepath.Dir(socket) + "/../run/docker.sock": true,
		} {
			if got := isFevom(host, socket); got != want {
				t.Errorf("%s: isFevom(%q) = %v, want %v", when, host, got, want)
			}
		}
	}
	check("no socket")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	check("a socket")
}

type entryCase struct {
	t       *testing.T
	home    string
	tools   string
	started string
	execed  []string
	env     []string
	stderr  bytes.Buffer
}

func newEntryCase(t *testing.T) *entryCase {
	t.Helper()
	c := &entryCase{t: t, home: scratchHome(t), tools: t.TempDir()}
	c.started = noDaemon(t)
	for _, name := range []string{"docker", "docker-compose"} {
		os.WriteFile(filepath.Join(c.tools, name), []byte("#!/bin/sh\n"), 0o755)
	}
	t.Setenv(home.Env, c.home)
	t.Setenv(ToolsEnv, c.tools)
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv(EntryMarkEnv, "")
	t.Setenv("DOCKER_CONFIG", dockerConfig(t, "fev", map[string]string{
		"fev":   "unix://" + filepath.Join(c.home, "run", "docker.sock"),
		"other": "unix:///somewhere/else/docker.sock",
	}))
	old := execve
	execve = func(path string, argv, env []string) error {
		c.execed = append([]string{path}, argv...)
		c.env = env
		return fmt.Errorf("the test's exec returns")
	}
	t.Cleanup(func() { execve = old })
	return c
}

func (c *entryCase) run(name string, argv ...string) int {
	c.execed, c.env = nil, nil
	c.stderr.Reset()
	return Entry(Env{Name: name, Args: argv, Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &c.stderr, OriginNS: 12345})
}

func (c *entryCase) wasStarted() string {
	data, _ := os.ReadFile(c.started)
	os.Remove(c.started)
	return strings.TrimSpace(string(data))
}

func (c *entryCase) listen() net.Listener {
	c.t.Helper()
	os.MkdirAll(filepath.Join(c.home, "run"), 0o700)
	ln, err := net.Listen("unix", api.SocketPath(c.home))
	if err != nil {
		c.t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	c.t.Cleanup(func() { ln.Close() })
	return ln
}

func TestTheEntryPointBecomesThePinnedDocker(t *testing.T) {
	c := newEntryCase(t)
	c.listen()
	t.Setenv("SOME_VARIABLE", "kept")
	c.run("docker", "run", "--rm", "-it", "alpine", "sh")
	want := []string{filepath.Join(c.tools, "docker"), "docker", "run", "--rm", "-it", "alpine", "sh"}
	if !reflect.DeepEqual(c.execed, want) {
		t.Fatalf("became %v, want %v\n%s", c.execed, want, c.stderr.String())
	}
	if started := c.wasStarted(); started != "" {
		t.Errorf("a daemon was started although one was up: fevd %s", started)
	}
	kept, dockerHost := false, false
	for _, kv := range c.env {
		kept = kept || kv == "SOME_VARIABLE=kept"
		dockerHost = dockerHost || strings.HasPrefix(kv, "DOCKER_HOST=unix")
	}
	if !kept || dockerHost {
		t.Errorf("the environment was changed: DOCKER_HOST set or a variable lost")
	}
	added := 0
	for _, kv := range c.env {
		if kv == EntryMarkEnv+"="+strconv.Itoa(os.Getpid()) {
			added++
		}
	}
	if added != 1 || len(c.env) != len(os.Environ()) {
		t.Errorf("the environment has %d variables against %d, the mark %d times", len(c.env), len(os.Environ()), added)
	}
	c.run("docker-compose", "-f", "x.yml", "up")
	if c.execed[0] != filepath.Join(c.tools, "docker-compose") || c.execed[1] != "docker-compose" {
		t.Errorf("docker-compose became %v", c.execed)
	}
}

func TestTheEntryPointStartsFevomWhenNoneIsRunning(t *testing.T) {
	c := newEntryCase(t)
	if code := c.run("docker", "ps"); code != ExitEntry {
		t.Fatalf("exit %d, want 125\n%s", code, c.stderr.String())
	}
	started := c.wasStarted()
	if !strings.Contains(started, "--foreground") || !strings.Contains(started, "--origin-ns 12345") || strings.Contains(started, "--no-autostart") {
		t.Errorf("fevd was started with %q; want the VM at once and the entry point's own origin", started)
	}
	if c.execed != nil || !strings.HasPrefix(c.stderr.String(), "fev: cannot start: ") {
		t.Errorf("after a failed start: became %v, said %q", c.execed, c.stderr.String())
	}
	ln := c.listen()
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(api.SocketPath(c.home)); err != nil {
		t.Fatal("the stale socket file is not there")
	}
	c.run("docker", "ps")
	if c.wasStarted() == "" {
		t.Error("a stale socket file was taken for a daemon")
	}
}

func TestAfterAStopTheEntryPointStartsNothing(t *testing.T) {
	c := newEntryCase(t)
	fev(t, c.home, "", "stop").want(t, 0)
	for i := 0; i < 20; i++ {
		if code := c.run("docker", "ps"); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if c.stderr.String() != api.StoppedLine+"\n" {
			t.Fatalf("said %q, want the one line", c.stderr.String())
		}
		if c.execed != nil {
			t.Fatalf("docker was run: %v", c.execed)
		}
	}
	if started := c.wasStarted(); started != "" {
		t.Errorf("a daemon was started after a stop: fevd %s", started)
	}
	c.listen()
	if code := c.run("docker", "ps"); code != 1 || c.execed != nil {
		t.Errorf("with the mark and a socket: exit %d, became %v", code, c.execed)
	}
	c.run("docker", "--context", "other", "ps")
	if c.execed == nil {
		t.Errorf("a command for another context was refused after fev stop: %s", c.stderr.String())
	}
	fev(t, c.home, "", "start")
	if _, err := os.Stat(api.StopMarkPath(c.home)); err == nil {
		t.Fatal("fev start left the mark")
	}
	c.wasStarted()
	c.run("docker", "ps")
	if c.execed == nil || c.execed[0] != filepath.Join(c.tools, "docker") {
		t.Errorf("after fev start: became %v; %s", c.execed, c.stderr.String())
	}
}

func TestACommandForAnotherEngineStartsNothing(t *testing.T) {
	c := newEntryCase(t)
	bin := filepath.Join(c.home, "bin")
	os.MkdirAll(bin, 0o755)
	self, _ := os.Executable()
	os.Symlink(self, filepath.Join(bin, "docker"))
	theirs := t.TempDir()
	os.WriteFile(filepath.Join(theirs, "docker"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", bin+":"+theirs+":/usr/bin:/bin")

	for _, how := range []struct {
		argv          []string
		host, context string
	}{
		{[]string{"--context", "other", "ps"}, "", ""},
		{[]string{"-H", "tcp://127.0.0.1:1", "ps"}, "", ""},
		{[]string{"ps"}, "tcp://127.0.0.1:1", ""},
		{[]string{"ps"}, "", "other"},
		{[]string{"--context", "nosuch", "ps"}, "", ""},
	} {
		t.Setenv("DOCKER_HOST", how.host)
		t.Setenv("DOCKER_CONTEXT", how.context)
		c.run("docker", how.argv...)
		if c.execed == nil || c.execed[0] != filepath.Join(theirs, "docker") {
			t.Errorf("%v DOCKER_HOST=%q DOCKER_CONTEXT=%q: became %v, want the user's own docker; %s", how.argv, how.host, how.context, c.execed, c.stderr.String())
		}
		if !reflect.DeepEqual(c.execed[1:], append([]string{"docker"}, how.argv...)) {
			t.Errorf("arguments changed: %v", c.execed)
		}
		if started := c.wasStarted(); started != "" {
			t.Errorf("%v: a daemon was started for another engine: fevd %s", how.argv, started)
		}
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	if other := otherDocker("docker"); other != "" {
		t.Skipf("this Mac has a docker in /usr/bin or /bin: %s", other)
	}
	c.run("docker", "--context", "other", "ps")
	if c.execed == nil || c.execed[0] != filepath.Join(c.tools, "docker") {
		t.Errorf("with no other docker: became %v", c.execed)
	}
	if started := c.wasStarted(); started != "" {
		t.Errorf("a daemon was started for another engine: fevd %s", started)
	}
}

func TestTheEntryPointNeverRunsItself(t *testing.T) {
	c := newEntryCase(t)
	c.listen()
	self, _ := os.Executable()
	os.Remove(filepath.Join(c.tools, "docker"))
	os.Symlink(self, filepath.Join(c.tools, "docker"))
	if code := c.run("docker", "ps"); code != ExitEntry || c.execed != nil || !strings.Contains(c.stderr.String(), "is fev itself") {
		t.Errorf("a pinned docker that is fev: exit %d, became %v, said %q", code, c.execed, c.stderr.String())
	}
	t.Setenv(EntryMarkEnv, strconv.Itoa(os.Getpid()))
	if code := c.run("docker", "ps"); code != ExitEntry || c.execed != nil || !strings.Contains(c.stderr.String(), "ran itself") {
		t.Errorf("a second pass: exit %d, became %v, said %q", code, c.execed, c.stderr.String())
	}
	os.Remove(filepath.Join(c.tools, "docker"))
	os.WriteFile(filepath.Join(c.tools, "docker"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv(EntryMarkEnv, strconv.Itoa(os.Getpid()+1))
	c.run("docker", "ps")
	if c.execed == nil {
		t.Errorf("a nested docker command was refused: %s", c.stderr.String())
	}
	os.Remove(filepath.Join(c.tools, "docker"))
	if code := c.run("docker", "ps"); code != ExitEntry || !strings.Contains(c.stderr.String(), `run "fev setup"`) {
		t.Errorf("no pinned docker: exit %d, said %q", code, c.stderr.String())
	}
}

func TestTheCommandsThatPublishPortsAreKnown(t *testing.T) {
	for argv, want := range map[string]bool{
		"docker run -d -p 80:80 nginx":       true,
		"docker start web":                   true,
		"docker container run nginx":         true,
		"docker container start web":         true,
		"docker compose up -d":               true,
		"docker compose -f a.yml -p proj up": true,
		"docker-compose up":                  true,
		"docker-compose -f a.yml up -d":      true,
		"docker ps":                          false,
		"docker exec web run":                false,
		"docker compose run web":             false,
		"docker compose -f up ps":            false,
		"docker-compose down":                false,
		"docker container ls":                false,
		"docker":                             false,
		"docker compose":                     false,
		"docker build -t start .":            false,
		"docker --context fev run nginx":     true,
		"docker -H unix:///x compose up":     true,
	} {
		f := strings.Fields(argv)
		if got := publishes(f[0], parseGlobals(f[0], f[1:]).rest); got != want {
			t.Errorf("%s: publishes = %v, want %v", argv, got, want)
		}
	}
}

func TestAPortInConflictIsSaidInTheTerminalThatRanDocker(t *testing.T) {
	c := newEntryCase(t)
	f := newFakeIn(t, c.home)
	old := api.Forward{ID: "docker/000000000000/tcp/9000", Source: api.ForwardDocker, Owner: "000000000000", Proto: "tcp",
		Mac: []string{"0.0.0.0:9000", "[::]:9000"}, State: api.ForwardConflict, Error: "in use"}
	fresh := api.Forward{ID: "docker/3f2a9c1d7e10/tcp/8080", Source: api.ForwardDocker, Owner: "3f2a9c1d7e10b6a4", Proto: "tcp",
		Mac: []string{"0.0.0.0:8080", "[::]:8080"}, State: api.ForwardConflict, Error: "port 8080/tcp on the Mac is in use by another program (0.0.0.0:8080)"}
	fine := api.Forward{ID: "docker/3f2a9c1d7e10/tcp/8081", Source: api.ForwardDocker, Owner: "3f2a9c1d7e10b6a4", Proto: "tcp",
		Mac: []string{"127.0.0.1:8081"}, State: api.ForwardActive}
	ran := filepath.Join(t.TempDir(), "ran")
	os.WriteFile(filepath.Join(c.tools, "docker"), []byte("#!/bin/sh\necho \"$*\" >"+ran+"\necho from docker\nexit 7\n"), 0o755)
	table := func(after ...api.Forward) {
		f.on(api.MethodForwardsList, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
			if _, err := os.Stat(ran); err != nil {
				return api.Forwards{Forwards: []api.Forward{old}}, nil
			}
			return api.Forwards{Forwards: append([]api.Forward{old}, after...)}, nil
		})
	}
	var stdout bytes.Buffer
	run := func(argv ...string) int {
		os.Remove(ran)
		c.stderr.Reset()
		stdout.Reset()
		c.execed = nil
		return Entry(Env{Name: "docker", Args: argv, Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &c.stderr})
	}

	table(fresh, fine)
	if code := run("run", "-d", "-p", "8080:80", "nginx"); code != 7 {
		t.Fatalf("exit %d, want the command's 7; %s", code, c.stderr.String())
	}
	want := "fev: port 0.0.0.0:8080, [::]:8080 (tcp) of docker 3f2a9c1d7e10 is not served: port 8080/tcp on the Mac is in use by another program (0.0.0.0:8080); \"fev ports --retry\" tries again\n"
	if c.stderr.String() != want {
		t.Errorf("stderr:\n%s\nwant:\n%s", c.stderr.String(), want)
	}
	if stdout.String() != "from docker\n" {
		t.Errorf("stdout: %q", stdout.String())
	}
	if data, _ := os.ReadFile(ran); strings.TrimSpace(string(data)) != "run -d -p 8080:80 nginx" {
		t.Errorf("docker was run as %q", data)
	}
	table(fine)
	if code := run("start", "web"); code != 7 || c.stderr.String() != "" {
		t.Errorf("with no new conflict: exit %d, stderr %q", code, c.stderr.String())
	}
	asked := len(f.made())
	run("ps")
	if c.execed == nil || len(f.made()) != asked {
		t.Errorf("docker ps: became %v, and the daemon was asked %d more times", c.execed, len(f.made())-asked)
	}
	f.on(api.MethodHello, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		data, _ := json.Marshal(api.BuildMismatch{DaemonBuild: "aaaaaaa", ClientBuild: "bbbbbbb"})
		return nil, &jsonrpc.Error{Code: api.CodeBuildMismatch, Message: "build mismatch", Data: data}
	})
	run("run", "nginx")
	if _, err := os.Stat(ran); c.execed == nil || err == nil {
		t.Errorf("with a daemon of another build: became %v (and ran docker as a child: %v)", c.execed, err == nil)
	}
}
