package daemon

import (
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

var macEntry, _ = base64.StdEncoding.DecodeString("GgEgAAIAAwAGAAoAeHRlcm0tZmV2dGVzdHxmZXYgdGVzdCB0ZXJtaW5hbAAAAVAA//8YAP//AAD///////8CAAcAG1tIG1sySgA=")

func entryDir(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "78"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "78", name), macEntry, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (f *sshFixture) ptyCommand(term, cmd string) string {
	f.t.Helper()
	c, err := ssh.Dial("tcp", "127.0.0.1:"+strconv.Itoa(f.port), f.clientConfig("default"))
	if err != nil {
		f.t.Fatal(err)
	}
	defer c.Close()
	s, err := c.NewSession()
	if err != nil {
		f.t.Fatal(err)
	}
	if err := s.RequestPty(term, 40, 80, ssh.TerminalModes{}); err != nil {
		f.t.Fatal(err)
	}
	out, err := s.Output(cmd)
	if err != nil {
		f.t.Fatalf("%s on %s: %v (%q)", cmd, term, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestAnSSHTerminalsEntryIsLookedUpWhereTheLatestHelloSaid(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := sshDaemon(t)
	f.create("u24", "ubuntu")
	want := strconv.Itoa(len(macEntry)) + " bytes for xterm-fevtest: <nil>"
	if got := f.ptyCommand("xterm-fevtest", "terminfo"); got != "none" {
		t.Fatalf("before any hello named a directory: %q", got)
	}
	hello := func(terminfo, dirs string) {
		t.Helper()
		c, _, err := api.NewClient(bg, f.conn(), "test", api.WithTerminfo(terminfo, dirs))
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	ghostty := entryDir(t, "xterm-fevtest")
	hello(ghostty, "")
	if got := f.ptyCommand("xterm-fevtest", "terminfo"); got != want {
		t.Fatalf("after a hello naming TERMINFO: %q, want %q", got, want)
	}
	hello("", "")
	if got := f.ptyCommand("xterm-fevtest", "terminfo"); got != want {
		t.Errorf("after a hello naming neither: %q", got)
	}
	if got := f.ptyCommand("xterm-fevnone", "terminfo"); got != "none" {
		t.Errorf("a name the Mac lacks: %q", got)
	}
	other := entryDir(t, "xterm-fevother")
	hello("", "/nonexistent:"+other)
	if got := f.ptyCommand("xterm-fevtest", "terminfo"); got != "none" {
		t.Errorf("the first TERMINFO stayed: %q", got)
	}
	if got := f.ptyCommand("xterm-fevother", "terminfo"); got != "none" {
		t.Errorf("another terminal's bytes under the name: %q", got)
	}
	os.MkdirAll(filepath.Join(other, "x"), 0o755)
	os.WriteFile(filepath.Join(other, "x", "xterm-fevtest"), macEntry, 0o644)
	if got := f.ptyCommand("xterm-fevtest", "terminfo"); got != want {
		t.Errorf("in TERMINFO_DIRS, Linux's layout: %q", got)
	}
	if out, err := f.exec("default", "terminfo"); err != nil || strings.TrimSpace(out) != "none" {
		t.Errorf("with no terminal: %q %v", out, err)
	}
}

func TestASessionsEntryReachesTheGuest(t *testing.T) {
	f := machinesDaemon(t)
	f.create("dev", "ubuntu")
	env := []string{"TERM=xterm-fevtest"}
	started, err := f.c.SessionStart(bg, api.SessionStartParams{Argv: []string{"terminfo"}, Shell: true, Env: env, TTY: true, Rows: 24, Cols: 80, Terminfo: macEntry}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := api.AttachSession(bg, f.layout.Home, started.Token)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(stream)
	stream.Close()
	if got, want := strings.TrimSpace(string(out)), strconv.Itoa(len(macEntry))+" bytes for xterm-fevtest: <nil>"; got != want {
		t.Errorf("the guest was given %q, want %q", got, want)
	}
	for why, p := range map[string]api.SessionStartParams{
		"no terminal": {Argv: []string{"true"}, Env: env, Terminfo: macEntry},
		"too large":   {Argv: []string{"true"}, Env: env, TTY: true, Terminfo: make([]byte, agent.MaxTerminfo+1)},
		"no TERM":     {Argv: []string{"true"}, TTY: true, Terminfo: macEntry},
	} {
		var e *jsonrpc.Error
		if _, err := f.c.SessionStart(bg, p, nil); !errors.As(err, &e) || e.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("%s: %v", why, err)
		}
	}
}
