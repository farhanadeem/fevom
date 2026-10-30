package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"fev/host/daemon/launch"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/version"
)

func TestMain(m *testing.M) {
	if os.Getenv(ctrlCHelperEnv) != "" {
		os.Exit(ctrlCHelper())
	}
	user, err := os.MkdirTemp("", "fev-cli-home.")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", user)
	for _, name := range []string{"ZDOTDIR", "XDG_CONFIG_HOME", "DOCKER_CONFIG", "DOCKER_HOST", "DOCKER_CONTEXT", "FEV_HOME", ToolsEnv, EntryMarkEnv, "VISUAL", "EDITOR"} {
		os.Unsetenv(name)
	}
	code := m.Run()
	os.RemoveAll(user)
	os.Exit(code)
}

type fake struct {
	t    *testing.T
	home string
	ln   net.Listener

	mu      sync.Mutex
	calls   []call
	replies map[string]handler
	results map[string]json.RawMessage
	after   map[string]func(c *jsonrpc.Conn, params json.RawMessage)
	conns   []net.Conn
	exited  bool
}

func (f *fake) exit() {
	f.ln.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exited = true
	for _, c := range f.conns {
		c.Close()
	}
	f.conns = nil
}

type call struct {
	Method string
	Params string
}

type handler func(c *jsonrpc.Conn, params json.RawMessage) (any, error)

func scratchHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bh.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func noDaemon(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "started")
	script := filepath.Join(dir, "fevd")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >>"+log+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(launch.DaemonEnv, script)
	t.Setenv(DaemonArgsEnv, "")
	return log
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := newFakeIn(t, scratchHome(t))
	noDaemon(t)
	return f
}

func newFakeIn(t *testing.T, home string) *fake {
	t.Helper()
	f := &fake{t: t, home: home, replies: map[string]handler{}, results: map[string]json.RawMessage{}, after: map[string]func(*jsonrpc.Conn, json.RawMessage){}}
	fixtures, err := api.Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	pending, err := api.PendingFixtures()
	if err != nil {
		t.Fatal(err)
	}
	for _, fx := range append(fixtures, pending...) {
		if _, ok := f.results[fx.Method]; !ok && !fx.Notification {
			f.results[fx.Method] = fx.Result
		}
	}
	if err := os.MkdirAll(filepath.Join(f.home, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.ln, err = net.Listen("unix", api.SocketPath(f.home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.ln.Close() })
	go func() {
		for {
			conn, err := f.ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			exited := f.exited
			f.conns = append(f.conns, conn)
			f.mu.Unlock()
			if exited {
				conn.Close()
				continue
			}
			jsonrpc.NewConn(conn, jsonrpc.HandlerFunc(f.handle))
		}
	}()
	return f
}

func (f *fake) handle(ctx context.Context, c *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
	f.mu.Lock()
	after := f.after[method]
	f.mu.Unlock()
	if after != nil {
		jsonrpc.AfterReply(ctx, func() { go after(c, params) })
	}
	if method == api.MethodHello {
		if h := f.reply(method); h != nil {
			return h(c, params)
		}
		return api.HelloResult{APIVersion: api.APIVersion, Build: version.Build, PID: 4242, Home: f.home}, nil
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{method, compact(params)})
	f.mu.Unlock()
	if h := f.reply(method); h != nil {
		return h(c, params)
	}
	if result, ok := f.results[method]; ok {
		return result, nil
	}
	return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "the fake has no example of %s", method)
}

func (f *fake) reply(method string) handler {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replies[method]
}

func (f *fake) on(method string, fn handler) {
	f.mu.Lock()
	f.replies[method] = fn
	f.mu.Unlock()
}

func (f *fake) then(method string, fn func(c *jsonrpc.Conn, params json.RawMessage)) {
	f.mu.Lock()
	f.after[method] = fn
	f.mu.Unlock()
}

func (f *fake) fixture(method, name string) json.RawMessage {
	f.t.Helper()
	result := fixtureResult(f.t, name)
	f.on(method, func(*jsonrpc.Conn, json.RawMessage) (any, error) { return result, nil })
	return result
}

func (f *fake) made() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call{}, f.calls...)
}

func (f *fake) params(method string) string {
	f.t.Helper()
	var found []string
	for _, c := range f.made() {
		if c.Method == method {
			found = append(found, c.Params)
		}
	}
	if len(found) != 1 {
		f.t.Fatalf("%s was called %d times, want once; calls: %v", method, len(found), f.made())
	}
	return found[0]
}

func (f *fake) called(method string) bool {
	for _, c := range f.made() {
		if c.Method == method {
			return true
		}
	}
	return false
}

func compact(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var b bytes.Buffer
	if json.Compact(&b, data) != nil {
		return string(data)
	}
	return b.String()
}

func fixtureFile(t *testing.T, name string) api.Fixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "proto", "daemon", "fixtures", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx api.Fixture
	if err := json.Unmarshal(data, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

func fixtureResult(t *testing.T, name string) json.RawMessage {
	return fixtureFile(t, name).Result
}

type result struct {
	out, err string
	code     int
}

func fev(t *testing.T, home string, stdin string, argv ...string) result {
	t.Helper()
	var out, errw lockedBuffer
	code := Main(Env{
		Name: "fev", Args: append([]string{"--home", home}, argv...),
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errw,
	})
	return result{out.String(), errw.String(), code}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (f *fake) run(argv ...string) result { return fev(f.t, f.home, "", argv...) }

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (UPDATE_GOLDEN=1 go test ./cli/ writes it)", err)
	}
	if got != string(want) {
		t.Errorf("%s:\n--- got\n%s--- want\n%s", path, got, want)
	}
}

func sameJSON(t *testing.T, got string, want json.RawMessage) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal([]byte(got), &a); err != nil {
		t.Fatalf("the output is not one JSON document: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("JSON output differs from the Daemon API's example:\n got %s\nwant %s", compact([]byte(got)), compact(want))
	}
}

func (r result) want(t *testing.T, code int) result {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit status %d, want %d\nstdout: %s\nstderr: %s", r.code, code, r.out, r.err)
	}
	return r
}

func sameParams(t *testing.T, got string, want json.RawMessage) {
	t.Helper()
	var a, b any
	json.Unmarshal([]byte(got), &a)
	json.Unmarshal(want, &b)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("parameters: %s, want %s", got, compact(want))
	}
}
