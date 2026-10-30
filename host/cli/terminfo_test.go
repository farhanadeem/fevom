package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/version"
)

var macEntry, _ = base64.StdEncoding.DecodeString("GgEgAAIAAwAGAAoAeHRlcm0tZmV2dGVzdHxmZXYgdGVzdCB0ZXJtaW5hbAAAAVAA//8YAP//AAD///////8CAAcAG1tIG1sySgA=")

func terminfoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "78"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "78", "xterm-fevtest"), macEntry, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTheMacsTerminalEntryGoesWithASessionThatHasATerminal(t *testing.T) {
	noTerminalEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TERMINFO", terminfoDir(t))
	t.Setenv("TERMINFO_DIRS", "")
	master, slave := openPty(t)
	defer master.Close()
	defer slave.Close()
	unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 40, Col: 132})
	go func() {
		buf := make([]byte, 256)
		for {
			if _, err := master.Read(buf); err != nil {
				return
			}
		}
	}()
	session := func(term string, stdout *os.File) api.SessionStartParams {
		t.Helper()
		t.Setenv("TERM", term)
		f := newFake(t)
		fakeSessions(f, func(api.SessionStartParams, net.Conn, <-chan int) api.SessionExit { return api.SessionExit{} })
		var out lockedBuffer
		env := Env{Name: "fev", Args: []string{"--home", f.home, "run", "htop"}, Stdin: slave, Stdout: slave, Stderr: slave}
		if stdout == nil {
			env.Stdout, env.Stderr = &out, &out
		}
		if code := Main(env); code != 0 {
			t.Fatalf("TERM=%s: exit %d", term, code)
		}
		var p api.SessionStartParams
		json.Unmarshal([]byte(f.params(api.MethodSessionStart)), &p)
		return p
	}
	p := session("xterm-fevtest", slave)
	if !p.TTY || !bytes.Equal(p.Terminfo, macEntry) || agent.TermOf(p.Env) != "xterm-fevtest" {
		t.Errorf("with a terminal: tty %v, TERM %q, %d bytes", p.TTY, agent.TermOf(p.Env), len(p.Terminfo))
	}
	p = session("xterm-fevtest", nil)
	if !p.StdinTTY || !bytes.Equal(p.Terminfo, macEntry) {
		t.Errorf("with standard input alone a terminal: %d bytes", len(p.Terminfo))
	}
	for _, term := range []string{"dumb", "xterm-fevnone"} {
		if p := session(term, slave); p.Terminfo != nil || agent.TermOf(p.Env) != term {
			t.Errorf("TERM=%s: TERM %q, %d bytes", term, agent.TermOf(p.Env), len(p.Terminfo))
		}
	}

	t.Setenv("TERM", "xterm-fevtest")
	f := newFake(t)
	fakeSessions(f, framedEcho)
	fev(t, f.home, "", "run", "true").want(t, 0)
	var q api.SessionStartParams
	json.Unmarshal([]byte(f.params(api.MethodSessionStart)), &q)
	if q.Terminfo != nil || agent.TermOf(q.Env) != "" {
		t.Errorf("with no terminal: %+v", q)
	}
}

func TestEachHelloNamesWhereTheTerminalsEntryIs(t *testing.T) {
	t.Setenv("TERMINFO", "/Applications/Ghostty.app/Contents/Resources/terminfo")
	t.Setenv("TERMINFO_DIRS", "/opt/homebrew/share/terminfo:")
	f := newFake(t)
	var mu sync.Mutex
	var hellos []api.HelloParams
	f.on(api.MethodHello, func(_ *jsonrpc.Conn, params json.RawMessage) (any, error) {
		var p api.HelloParams
		json.Unmarshal(params, &p)
		mu.Lock()
		hellos = append(hellos, p)
		mu.Unlock()
		return api.HelloResult{APIVersion: api.APIVersion, Build: version.Build, PID: 4242, Home: f.home}, nil
	})
	f.run("status")
	a, b, err := socketPair()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer b.Close()
	var errw lockedBuffer
	Main(Env{Name: "fev", Args: []string{"--home", f.home, "ssh", "--proxy"}, Stdin: a, Stdout: a, Stderr: &errw})
	mu.Lock()
	defer mu.Unlock()
	if len(hellos) != 2 {
		t.Fatalf("%d hellos, want status's and ssh --proxy's: %+v (ssh --proxy said %q)", len(hellos), hellos, strings.TrimSpace(errw.String()))
	}
	for _, h := range hellos {
		if h.Terminfo != "/Applications/Ghostty.app/Contents/Resources/terminfo" || h.TerminfoDirs != "/opt/homebrew/share/terminfo:" {
			t.Errorf("a hello named %q and %q", h.Terminfo, h.TerminfoDirs)
		}
	}
}

func socketPair() (*os.File, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[0]), "a"), os.NewFile(uintptr(fds[1]), "b"), nil
}
