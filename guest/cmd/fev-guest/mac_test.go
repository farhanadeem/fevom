//go:build linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fev/proto/bridge"
)

type fakeBridge struct {
	t       *testing.T
	headers chan *bridge.Header
	stdin   chan []byte
}

func newFakeBridge(t *testing.T, serve func(h *bridge.Header, r *bufio.Reader, c net.Conn)) *fakeBridge {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "mac.sock")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	old := socketPath
	socketPath = p
	t.Cleanup(func() { socketPath = old })
	f := &fakeBridge{t: t, headers: make(chan *bridge.Header, 8), stdin: make(chan []byte, 8)}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				h, _, err := bridge.ReadHeader(r)
				if err != nil {
					return
				}
				f.headers <- h
				serve(h, r, c)
			}()
		}
	}()
	return f
}

func streams(t *testing.T, in string) func() (string, string) {
	t.Helper()
	dir := t.TempDir()
	open := func(name string) *os.File {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	i, o, e := open("in"), open("out"), open("err")
	i.WriteString(in)
	i.Seek(0, 0)
	oi, oo, oe := stdin, stdout, stderr
	stdin, stdout, stderr = i, o, e
	t.Cleanup(func() { stdin, stdout, stderr = oi, oo, oe })
	return func() (string, string) {
		a, _ := os.ReadFile(o.Name())
		b, _ := os.ReadFile(e.Name())
		return string(a), string(b)
	}
}

func echoRun(exit bridge.Exit) func(h *bridge.Header, r *bufio.Reader, c net.Conn) {
	return func(h *bridge.Header, r *bufio.Reader, c net.Conn) {
		bridge.WriteOK(c)
		buf := make([]byte, bridge.MaxFrame)
		var in []byte
		for {
			s, n, err := bridge.ReadFrame(r, buf)
			if err != nil {
				return
			}
			if s == bridge.FrameStdin {
				if n == 0 {
					break
				}
				in = append(in, buf[:n]...)
			}
		}
		bridge.WriteFrame(c, bridge.FrameStdout, in)
		bridge.WriteFrame(c, bridge.FrameStderr, []byte("on stderr\n"))
		b, _ := json.Marshal(exit)
		bridge.WriteFrame(c, bridge.FrameExit, b)
	}
}

func TestMacRunsTheCommandOnTheMacAndEndsWithItsStatus(t *testing.T) {
	f := newFakeBridge(t, echoRun(bridge.Exit{Code: 3}))
	out := streams(t, "typed\n")
	t.Setenv("FEVENV", "FOO:BAR/p")
	t.Setenv("FOO", "1")
	t.Setenv("BAR", "two")
	t.Setenv("SECRET", "no")
	dir := t.TempDir()
	t.Chdir(dir)
	if got := macMain([]string{"uname", "-s"}); got != 3 {
		t.Errorf("status %d, want 3", got)
	}
	h := <-f.headers
	if h.Verb != bridge.VerbRun || strings.Join(h.Argv, " ") != "uname -s" || h.Dir != dir || h.TTY() {
		t.Errorf("header %+v", h)
	}
	if env := strings.Join(h.Env, " "); env != "FOO=1 BAR=two" {
		t.Errorf("environment %q", env)
	}
	if o, e := out(); o != "typed\n" || e != "on stderr\n" {
		t.Errorf("stdout %q stderr %q", o, e)
	}
}

func TestASignalsEndIs128AndItsNumber(t *testing.T) {
	newFakeBridge(t, echoRun(bridge.Exit{Signal: 9}))
	streams(t, "")
	if got := macMain([]string{"run", "sleep", "9"}); got != 137 {
		t.Errorf("status %d", got)
	}
}

func TestARefusalIsSaid(t *testing.T) {
	newFakeBridge(t, func(_ *bridge.Header, _ *bufio.Reader, c net.Conn) {
		bridge.WriteError(c, "uid 65534 may not use the Mac's commands from this machine; its socket serves root and me")
	})
	out := streams(t, "")
	if got := macMain([]string{"true"}); got != failed {
		t.Errorf("status %d", got)
	}
	if _, e := out(); !strings.Contains(e, "mac: uid 65534 may not use") {
		t.Errorf("stderr %q", e)
	}
	socketPath = filepath.Join(t.TempDir(), "none.sock")
	streams(t, "")
	if got := macMain([]string{"true"}); got != failed {
		t.Errorf("no socket: status %d", got)
	}
}

func TestMacAndMacctlWords(t *testing.T) {
	f := newFakeBridge(t, func(h *bridge.Header, r *bufio.Reader, c net.Conn) {
		if h.Verb != bridge.VerbRun {
			bridge.WriteOK(c)
			return
		}
		echoRun(bridge.Exit{})(h, r, c)
	})
	for _, c := range []struct {
		name string
		args []string
		verb string
		argv string
	}{
		{"mac", []string{"completion", "zsh"}, bridge.VerbRun, "completion zsh"},
		{"pbcopy", []string{"-pboard", "general"}, bridge.VerbRun, "pbcopy -pboard general"},
		{"open", []string{"."}, bridge.VerbRun, "open ."},
		{"macctl", []string{"run", "sw_vers"}, bridge.VerbRun, "sw_vers"},
		{"mac", []string{"link", "sw_vers", "say"}, bridge.VerbLink, ""},
		{"macctl", []string{"unlink", "say"}, bridge.VerbUnlink, ""},
		{"mac", []string{"notify", "-t", "build", "done", "at", "last"}, bridge.VerbNotify, ""},
		{"fev-guest", []string{"mac", "true"}, bridge.VerbRun, "true"},
	} {
		streams(t, "")
		if got := dispatch(c.name, c.args); got != 0 {
			t.Errorf("%s %v: status %d", c.name, c.args, got)
			continue
		}
		h := <-f.headers
		if h.Verb != c.verb || strings.Join(h.Argv, " ") != c.argv {
			t.Errorf("%s %v: header %+v", c.name, c.args, h)
		}
		switch h.Verb {
		case bridge.VerbLink:
			if strings.Join(h.Names, " ") != "sw_vers say" {
				t.Errorf("link names %v", h.Names)
			}
		case bridge.VerbNotify:
			if h.Title != "build" || h.Message != "done at last" {
				t.Errorf("notify %+v", h)
			}
		}
	}
	out := streams(t, "")
	if got := dispatch("macctl", []string{"uname"}); got != 2 {
		t.Errorf("macctl uname: %d", got)
	}
	if _, e := out(); !strings.Contains(e, "macctl run uname") {
		t.Errorf("macctl's word: %q", e)
	}
	select {
	case h := <-f.headers:
		t.Errorf("macctl ran an unknown word: %+v", h)
	default:
	}
}

func TestMacctlCompletion(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		out := streams(t, "")
		if got := macctlMain([]string{"completion", shell}); got != 0 {
			t.Errorf("%s: %d", shell, got)
		}
		o, _ := out()
		for _, want := range []string{"macctl", "push", "pull", "notify", "completion"} {
			if !strings.Contains(o, want) {
				t.Errorf("%s's script has no %q:\n%s", shell, want, o)
			}
		}
	}
	streams(t, "")
	if got := macctlMain([]string{"completion", "tcsh"}); got != 2 {
		t.Errorf("tcsh: %d", got)
	}
	out := streams(t, "")
	macMain([]string{"--help"})
	if o, _ := out(); !strings.Contains(o, "push") || !strings.Contains(o, "osascript") {
		t.Errorf("mac --help: %q", o)
	}
}

func TestCopiesAreWholeAndReplaceWithoutAWord(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "a"), []byte("new a"), 0o640)
	os.WriteFile(filepath.Join(src, "sub", "b"), []byte("b"), 0o600)
	os.Symlink("a", filepath.Join(src, "link"))
	dest := filepath.Join(dir, "dest")
	os.MkdirAll(filepath.Join(dest, "src"), 0o755)
	os.WriteFile(filepath.Join(dest, "src", "a"), []byte("old a, longer"), 0o644)
	if err := copyInto(src, dest); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "src", "a")); string(b) != "new a" {
		t.Errorf("a: %q", b)
	}
	if st, _ := os.Stat(filepath.Join(dest, "src", "a")); st.Mode().Perm() != 0o640 {
		t.Errorf("a's mode %v", st.Mode())
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "src", "sub", "b")); string(b) != "b" {
		t.Errorf("sub/b: %q", b)
	}
	if target, err := os.Readlink(filepath.Join(dest, "src", "link")); err != nil || target != "a" {
		t.Errorf("link: %q %v", target, err)
	}
	if err := copyInto(filepath.Join(src, "a"), filepath.Join(dir, "renamed")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "renamed")); !bytes.Equal(b, []byte("new a")) {
		t.Errorf("renamed: %q", b)
	}
	if err := copyInto(filepath.Join(src, "a"), src); err == nil {
		t.Error("a file was copied over itself")
	}
}

func TestPushWithoutTheShareSaysSo(t *testing.T) {
	newFakeBridge(t, func(_ *bridge.Header, _ *bufio.Reader, c net.Conn) {
		bridge.WriteOK(c)
		json.NewEncoder(c).Encode(bridge.Info{Home: "/Users/me", User: "me", Share: false})
	})
	out := streams(t, "")
	if got := macMain([]string{"push", "x"}); got != 1 {
		t.Errorf("status %d", got)
	}
	if _, e := out(); !strings.Contains(e, "files.share is off") {
		t.Errorf("stderr %q", e)
	}
}

func TestAMacProgramRunByPathIsSentAsExec(t *testing.T) {
	f := newFakeBridge(t, echoRun(bridge.Exit{Code: 7}))
	out := streams(t, "in\n")
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	os.MkdirAll(real, 0o755)
	os.WriteFile(filepath.Join(real, "tool"), []byte{0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 2}, 0o755)
	os.Symlink(filepath.Join(real, "tool"), filepath.Join(dir, "link"))
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(dir)
	if got := dispatch("fev-macexec", []string{"./link", "my-tool", "a b"}); got != 7 {
		t.Fatalf("ended %d, want the Mac's 7", got)
	}
	h := <-f.headers
	if h.Verb != bridge.VerbExec || h.Path != filepath.Join(real, "tool") || strings.Join(h.Argv, "|") != "my-tool|a b" || h.Dir != dir {
		t.Errorf("sent %+v", h)
	}
	if o, _ := out(); o != "in\n" {
		t.Errorf("standard output %q", o)
	}
	dispatch("fev-macexec", []string{filepath.Join(real, "tool"), ""})
	if h := <-f.headers; len(h.Argv) != 1 || h.Argv[0] != filepath.Join(real, "tool") {
		t.Errorf("an empty argv[0] sent as %q", h.Argv)
	}
}

func TestAMacProgramWithNoBridgeIsOneLineAndRefused(t *testing.T) {
	old := socketPath
	socketPath = filepath.Join(t.TempDir(), "none", "mac.sock")
	defer func() { socketPath = old }()
	out := streams(t, "")
	if got := dispatch("fev-macexec", []string{"/x/echo", "echo", "hi"}); got != 126 {
		t.Fatalf("ended %d, want 126", got)
	}
	o, e := out()
	if o != "" || strings.Count(e, "\n") != 1 || !strings.HasPrefix(e, "/x/echo: a Mac program") || !strings.Contains(e, "not in a container or an isolated machine") {
		t.Errorf("said %q / %q", o, e)
	}
	if got := dispatch("fev-macexec", nil); got != 2 {
		t.Errorf("by hand: %d", got)
	}
}
