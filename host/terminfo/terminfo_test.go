package terminfo

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"fev/proto/agent"
)

func entry(names, tail string) []byte {
	n := len(names) + 1
	b := []byte{0x1e, 0x02, byte(n), byte(n >> 8), 0, 0, 0, 0, 0, 0, 0, 0}
	b = append(b, names...)
	b = append(b, 0)
	return append(b, tail...)
}

func put(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDirsAreNcursesOrder(t *testing.T) {
	got := Env{Terminfo: "/Applications/Ghostty.app/Contents/Resources/terminfo", TerminfoDirs: "/opt/a::rel:/opt/b/", Home: "/Users/u"}.Dirs()
	want := []string{"/Applications/Ghostty.app/Contents/Resources/terminfo", "/Users/u/.terminfo", "/opt/a", System, "/opt/b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := (Env{}).Dirs(); !reflect.DeepEqual(got, []string{System}) {
		t.Errorf("nothing set: %q", got)
	}
}

func TestFindReadsBothLayouts(t *testing.T) {
	dir := t.TempDir()
	hex := entry("xterm-ghostty|ghostty|Ghostty", "hex")
	put(t, filepath.Join(dir, "mac", "78", "xterm-ghostty"), hex)
	letter := entry("foot|foot", "letter")
	put(t, filepath.Join(dir, "linux", "f", "foot"), letter)
	dirs := []string{filepath.Join(dir, "mac"), filepath.Join(dir, "linux")}
	if got := Find("xterm-ghostty", dirs); !bytes.Equal(got, hex) {
		t.Errorf("hex layout: %q", got)
	}
	if got := Find("foot", dirs); !bytes.Equal(got, letter) {
		t.Errorf("letter layout: %q", got)
	}
	if got := Find("kitty", dirs); got != nil {
		t.Errorf("a name nowhere: %q", got)
	}
}

func TestFindTakesTheFirstPlaceAndPassesOverWhatIsNotAnEntry(t *testing.T) {
	dir := t.TempDir()
	env := Env{
		Terminfo:     filepath.Join(dir, "terminfo"),
		Home:         filepath.Join(dir, "home"),
		TerminfoDirs: filepath.Join(dir, "d1") + ":" + filepath.Join(dir, "d2"),
	}
	name := "xterm-fevtest"
	at := func(d string) string { return filepath.Join(d, "x", name) }
	put(t, at(filepath.Join(dir, "d2")), entry(name, "d2"))
	if got := Find(name, env.Dirs()); !bytes.Equal(got, entry(name, "d2")) {
		t.Fatalf("TERMINFO_DIRS's second: %q", got)
	}
	put(t, at(filepath.Join(dir, "d1")), entry(name, "d1"))
	if got := Find(name, env.Dirs()); !bytes.Equal(got, entry(name, "d1")) {
		t.Fatalf("TERMINFO_DIRS's first: %q", got)
	}
	put(t, at(filepath.Join(dir, "home", ".terminfo")), entry(name, "home"))
	if got := Find(name, env.Dirs()); !bytes.Equal(got, entry(name, "home")) {
		t.Fatalf("~/.terminfo: %q", got)
	}
	put(t, at(env.Terminfo), entry(name, "terminfo"))
	if got := Find(name, env.Dirs()); !bytes.Equal(got, entry(name, "terminfo")) {
		t.Fatalf("TERMINFO: %q", got)
	}
	put(t, at(env.Terminfo), []byte("#!/bin/sh\n"))
	put(t, at(filepath.Join(dir, "home", ".terminfo")), append(entry(name, ""), make([]byte, agent.MaxTerminfo)...))
	if got := Find(name, env.Dirs()); !bytes.Equal(got, entry(name, "d1")) {
		t.Fatalf("past a script and an oversized entry: %q", got)
	}
	put(t, at(filepath.Join(dir, "d1")), entry("xterm-other", "d1"))
	if got := Find(name, env.Dirs()); !bytes.Equal(got, entry(name, "d2")) {
		t.Fatalf("past another terminal's entry: %q", got)
	}
}

func TestFindSendsNothingForDumbOrANameThatIsNotAFileName(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, "d", "dumb"), entry("dumb", ""))
	put(t, filepath.Join(dir, "x"), entry("x", ""))
	for _, name := range []string{"", "dumb", "../x", ".x", "x/../../x"} {
		if got := Find(name, []string{filepath.Join(dir), filepath.Join(dir, "d")}); got != nil {
			t.Errorf("%q: %d bytes", name, len(got))
		}
	}
}

func TestFindPassesOverAFIFOWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	name := "xterm-fevtest"
	if err := os.MkdirAll(filepath.Join(dir, "a", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "a", "x", name), 0o644); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(dir, "b", "78", name), entry(name, "b"))
	got := make(chan []byte, 1)
	go func() { got <- Find(name, []string{filepath.Join(dir, "a"), filepath.Join(dir, "b")}) }()
	select {
	case b := <-got:
		if !bytes.Equal(b, entry(name, "b")) {
			t.Errorf("past the FIFO: %q", b)
		}
	case <-time.After(5 * time.Second):
		if f, err := os.OpenFile(filepath.Join(dir, "a", "x", name), os.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
		t.Fatal("Find waited on a FIFO")
	}
}

func TestFromEnvFindsTheMacsOwnEntries(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the Mac's /usr/share/terminfo")
	}
	env := map[string]string{"TERM": "xterm-256color"}
	b := FromEnv(func(k string) string { return env[k] })
	if err := agent.CheckTerminfo("xterm-256color", b); err != nil {
		t.Fatalf("the Mac's xterm-256color: %v", err)
	}
	dir := t.TempDir()
	put(t, filepath.Join(dir, "78", "xterm-fevtest"), entry("xterm-fevtest", "own"))
	env = map[string]string{"TERM": "xterm-fevtest", "TERMINFO": dir}
	if got := FromEnv(func(k string) string { return env[k] }); !bytes.Equal(got, entry("xterm-fevtest", "own")) {
		t.Errorf("TERMINFO's: %q", got)
	}
}
