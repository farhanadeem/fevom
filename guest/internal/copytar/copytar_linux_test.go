package copytar

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func archive(t *testing.T, entries ...entry) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: e.typ, Mode: mode, Size: int64(len(e.body)), Linkname: e.link}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(e.body))
	}
	tw.Close()
	return &b
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return string(b)
}

func noWarn(t *testing.T) Warn {
	return func(format string, args ...any) { t.Errorf("warned: "+format, args...) }
}

func TestUnpackCopiesAsCpWould(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "proj", "old"), 0o755)
	os.WriteFile(filepath.Join(home, "one"), []byte("old one"), 0o600)
	outside := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(outside, []byte("untouched"), 0o644)
	os.Symlink(outside, filepath.Join(home, "proj", "a.txt"))
	a := archive(t,
		entry{name: "proj/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "proj/a.txt", typ: tar.TypeReg, body: "alpha"},
		entry{name: "proj/sub/", typ: tar.TypeDir, mode: 0o750},
		entry{name: "proj/sub/run", typ: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o4755},
		entry{name: "proj/link", typ: tar.TypeSymlink, link: "sub/run"},
		entry{name: "one", typ: tar.TypeReg, body: "new one"},
	)
	if err := Unpack(a, home, []string{"proj", "one"}, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(home, "proj", "a.txt")); got != "alpha" {
		t.Errorf("proj/a.txt is %q", got)
	}
	if got := read(t, outside); got != "untouched" {
		t.Errorf("a file was written through a link: %q", got)
	}
	if got := read(t, filepath.Join(home, "one")); got != "new one" {
		t.Errorf("one is %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, "proj", "old")); err != nil {
		t.Errorf("the folder that was there was not merged into: %v", err)
	}
	st, err := os.Stat(filepath.Join(home, "proj", "sub", "run"))
	if err != nil || st.Mode()&os.ModeSetuid != 0 || st.Mode().Perm()&0o700 != 0o700 {
		t.Errorf("proj/sub/run: %v %v (the set-user-ID bit must be stripped)", st.Mode(), err)
	}
	if l, _ := os.Readlink(filepath.Join(home, "proj", "link")); l != "sub/run" {
		t.Errorf("proj/link is a link to %q", l)
	}
	if left, _ := filepath.Glob(filepath.Join(home, "*", ".fev-copy-*")); len(left) != 0 {
		t.Errorf("scratch names left behind: %v", left)
	}

	if err := Unpack(archive(t, entry{name: "f", typ: tar.TypeReg, body: "F"}), filepath.Join(home, "renamed"), []string{"f"}, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(home, "renamed")); got != "F" {
		t.Errorf("renamed is %q", got)
	}
	os.Symlink(filepath.Join(home, "proj"), filepath.Join(home, "toproj"))
	if err := Unpack(archive(t, entry{name: "g", typ: tar.TypeReg, body: "G"}), filepath.Join(home, "toproj"), []string{"g"}, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(home, "proj", "g")); got != "G" {
		t.Errorf("proj/g is %q", got)
	}
	err = Unpack(archive(t, entry{name: "x", typ: tar.TypeReg}, entry{name: "y", typ: tar.TypeReg}), filepath.Join(home, "one"), []string{"x", "y"}, noWarn(t))
	if err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Errorf("two items onto a file: %v", err)
	}
	if err := Unpack(archive(t, entry{name: "proj", typ: tar.TypeReg}), home, []string{"proj"}, noWarn(t)); err == nil {
		t.Errorf("a file put where a folder is")
	}
	if err := Unpack(archive(t, entry{name: "one/", typ: tar.TypeDir}), home, []string{"one"}, noWarn(t)); err == nil {
		t.Errorf("a folder put where a file is")
	}
	var said []string
	if err := Unpack(archive(t, entry{name: "dev", typ: tar.TypeChar}), home, []string{"dev"}, func(f string, a ...any) { said = append(said, fmt.Sprintf(f, a...)) }); err != nil {
		t.Fatal(err)
	}
	if len(said) != 1 {
		t.Errorf("a device was not said to be left out: %q", said)
	}
	if _, err := os.Lstat(filepath.Join(home, "dev")); err == nil {
		t.Errorf("a device entry made something")
	}
}

func TestUnpackNeverLeavesTheDestination(t *testing.T) {
	outside := t.TempDir()
	cases := map[string]struct {
		names   []string
		entries []entry
		planted bool
	}{
		"climbing":            {[]string{"x"}, []entry{{name: "x/../../evil", typ: tar.TypeReg, body: "pwned"}}, false},
		"dot dot":             {[]string{"x"}, []entry{{name: "../evil", typ: tar.TypeReg, body: "pwned"}}, false},
		"absolute":            {[]string{"x"}, []entry{{name: filepath.Join(outside, "evil"), typ: tar.TypeReg, body: "pwned"}}, false},
		"another name":        {[]string{"x"}, []entry{{name: "evil", typ: tar.TypeReg, body: "pwned"}}, false},
		"through its link":    {[]string{"x"}, []entry{{name: "x", typ: tar.TypeSymlink, link: outside}, {name: "x/evil", typ: tar.TypeReg, body: "pwned"}}, false},
		"through a planted":   {[]string{"d"}, []entry{{name: "d/", typ: tar.TypeDir}, {name: "d/sub/evil", typ: tar.TypeReg, body: "pwned"}}, true},
		"a folder via a link": {[]string{"d"}, []entry{{name: "d/", typ: tar.TypeDir}, {name: "d/sub/inner/", typ: tar.TypeDir}}, true},
	}
	for name, c := range cases {
		dest := t.TempDir()
		if c.planted {
			os.Mkdir(filepath.Join(dest, "d"), 0o755)
			os.Symlink(outside, filepath.Join(dest, "d", "sub"))
		}
		err := Unpack(archive(t, c.entries...), dest, c.names, noWarn(t))
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		left, _ := os.ReadDir(outside)
		if len(left) != 0 {
			t.Fatalf("%s: something was written outside: %v", name, left)
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(dest), "evil")); err == nil {
			t.Fatalf("%s: a file was written beside the destination", name)
		}
	}
}

func TestPackSendsWhatCpWouldCopy(t *testing.T) {
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("not sent"), 0o644)
	os.MkdirAll(filepath.Join(home, "made", "inner"), 0o755)
	os.WriteFile(filepath.Join(home, "made", "inner", "h"), []byte("four"), 0o644)
	os.WriteFile(filepath.Join(home, "made", "suid"), []byte("x"), 0o4755)
	os.Symlink(outside, filepath.Join(home, "made", "out"))
	syscall.Mkfifo(filepath.Join(home, "made", "pipe"), 0o644)
	os.WriteFile(filepath.Join(home, "e"), []byte("five"), 0o600)
	os.Symlink(outside, filepath.Join(home, "toplink"))
	wd, _ := os.Getwd()
	os.Chdir(home)
	defer os.Chdir(wd)

	var b bytes.Buffer
	var said []string
	if err := Pack(&b, []string{"made", "e", "toplink"}, func(f string, a ...any) { said = append(said, fmt.Sprintf(f, a...)) }); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	tr := tar.NewReader(&b)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(tr)
		got[h.Name] = fmt.Sprintf("%c %o %s%s %d:%d", h.Typeflag, h.Mode, h.Linkname, body, h.Uid, h.Gid)
	}
	var names []string
	for n, v := range got {
		names = append(names, n+"="+v)
	}
	sort.Strings(names)
	want := []string{
		"e=0 600 five 0:0",
		"made/=5 755  0:0",
		"made/inner/=5 755  0:0",
		"made/inner/h=0 644 four 0:0",
		"made/out=2 777 " + outside + " 0:0",
		"made/suid=0 755 x 0:0",
		"toplink=2 777 " + outside + " 0:0",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}
	if len(said) != 1 || !strings.Contains(said[0], "pipe") {
		t.Errorf("the pipe was not said to be left out: %q", said)
	}

	b.Reset()
	if err := Pack(&b, []string{"e", "nothing-here"}, noWarn(t)); err == nil || !strings.Contains(err.Error(), "nothing-here") {
		t.Errorf("a path that is not there: %v", err)
	}
	if b.Len() != 0 {
		t.Errorf("%d bytes sent before the missing path was found", b.Len())
	}
	if err := Pack(&b, []string{"e", "made/../e"}, noWarn(t)); err == nil {
		t.Errorf("two paths of one name were sent")
	}
	b.Reset()
	if err := Pack(&b, []string{"."}, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	h, err := tar.NewReader(&b).Next()
	if err != nil || h.Name != filepath.Base(home)+"/" {
		t.Errorf("the home's first entry: %v %v", h, err)
	}
}

func TestARoundTrip(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "tree", "a", "b"), 0o755)
	os.WriteFile(filepath.Join(src, "tree", "a", "b", "f"), bytes.Repeat([]byte("x"), 1<<20+7), 0o640)
	os.Symlink("a/b/f", filepath.Join(src, "tree", "l"))
	wd, _ := os.Getwd()
	os.Chdir(src)
	defer os.Chdir(wd)
	var b bytes.Buffer
	if err := Pack(&b, []string{"tree"}, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := Unpack(&b, dest, []string{"tree"}, noWarn(t)); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dest, "tree", "a", "b", "f"))
	if err != nil || st.Size() != 1<<20+7 {
		t.Errorf("tree/a/b/f: %v %v", st, err)
	}
	if l, _ := os.Readlink(filepath.Join(dest, "tree", "l")); l != "a/b/f" {
		t.Errorf("tree/l is a link to %q", l)
	}
}
