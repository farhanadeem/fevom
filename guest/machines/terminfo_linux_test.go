package machines

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var macEntry, _ = base64.StdEncoding.DecodeString("GgEgAAIAAwAGAAoAeHRlcm0tZmV2dGVzdHxmZXYgdGVzdCB0ZXJtaW5hbAAAAVAA//8YAP//AAD///////8CAAcAG1tIG1sySgA=")

const testTerm = "xterm-fevtest"

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasPrefix(info.Name(), ".fev-") {
			found = append(found, p)
		}
		return nil
	})
	return found
}

func TestAnEntryTheMachineLacksIsFiledInEtcTerminfo(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc/terminfo"), 0o755)
	filed, err := installTerminfo(root, "/home/u", testTerm, macEntry)
	if err != nil || filed != terminfoTarget {
		t.Fatalf("filed %q, %v", filed, err)
	}
	p := filepath.Join(root, "etc/terminfo/x", testTerm)
	b, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(b, macEntry) {
		t.Fatalf("%s: %v, %d bytes", p, err, len(b))
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", st.Mode())
	}
	if st, _ := os.Stat(filepath.Dir(p)); st == nil || st.Mode().Perm() != 0o755 {
		t.Errorf("its directory: %v", st)
	}
	if l := leftovers(t, root); len(l) != 0 {
		t.Errorf("left behind: %q", l)
	}
	if filed, err := installTerminfo(root, "/home/u", testTerm, macEntry); err != nil || filed != "" {
		t.Errorf("again: filed %q, %v", filed, err)
	}
}

func TestAnEntryGoesToUsrShareTerminfoWhereThereIsNoEtcTerminfo(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	filed, err := installTerminfo(root, "/home/u", testTerm, macEntry)
	if err != nil || filed != terminfoFallback {
		t.Fatalf("filed %q, %v", filed, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "usr/share/terminfo/x", testTerm)); !bytes.Equal(b, macEntry) {
		t.Error("not in /usr/share/terminfo")
	}
	if _, err := os.Stat(filepath.Join(root, "etc/terminfo")); err == nil {
		t.Error("an /etc/terminfo was made")
	}
}

func TestAnEntryTheMachineHasIsLeftAsItIs(t *testing.T) {
	theirs := []byte("the machine's own")
	for _, at := range []string{"etc/terminfo", "lib/terminfo", "usr/share/terminfo", "home/u/.terminfo"} {
		t.Run(at, func(t *testing.T) {
			root := t.TempDir()
			p := filepath.Join(root, at, "x", testTerm)
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, theirs, 0o644)
			filed, err := installTerminfo(root, "/home/u", testTerm, macEntry)
			if err != nil || filed != "" {
				t.Fatalf("filed %q, %v", filed, err)
			}
			if b, _ := os.ReadFile(p); !bytes.Equal(b, theirs) {
				t.Errorf("the machine's entry was changed: %q", b)
			}
			if at != "etc/terminfo" {
				if _, err := os.Stat(filepath.Join(root, "etc/terminfo/x", testTerm)); err == nil {
					t.Error("filed beside the machine's own")
				}
			}
		})
	}
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "usr/lib/terminfo/x"), 0o755)
	os.WriteFile(filepath.Join(root, "usr/lib/terminfo/x", testTerm), theirs, 0o644)
	os.Symlink("usr/lib", filepath.Join(root, "lib"))
	if filed, err := installTerminfo(root, "/root", testTerm, macEntry); err != nil || filed != "" {
		t.Errorf("through /lib -> usr/lib: filed %q, %v", filed, err)
	}
}

func TestAFileOfTheNameIsNeverReplacedNorALinkFollowedOut(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	dir := filepath.Join(root, "etc/terminfo/x")
	os.MkdirAll(dir, 0o755)
	os.Symlink(filepath.Join(outside, "evil"), filepath.Join(dir, testTerm))
	if filed, err := installTerminfo(root, "", testTerm, macEntry); filed != "" {
		t.Fatalf("filed over a link: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "evil")); err == nil {
		t.Fatal("written outside the root through a link")
	}
	if l := leftovers(t, root); len(l) != 0 {
		t.Errorf("left behind: %q", l)
	}
	root2 := t.TempDir()
	os.MkdirAll(filepath.Join(root2, "etc"), 0o755)
	os.MkdirAll(filepath.Join(root2, outside), 0o755)
	os.Symlink(outside, filepath.Join(root2, "etc/terminfo"))
	filed, err := installTerminfo(root2, "", testTerm, macEntry)
	if err != nil || filed != terminfoTarget {
		t.Fatalf("under a link: filed %q, %v", filed, err)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("written outside the root: %v", ents)
	}
	if b, _ := os.ReadFile(filepath.Join(root2, outside, "x", testTerm)); !bytes.Equal(b, macEntry) {
		t.Errorf("not at the link's target inside the root")
	}
	root3 := t.TempDir()
	os.MkdirAll(filepath.Join(root3, "etc/terminfo"), 0o755)
	os.MkdirAll(filepath.Join(root3, outside), 0o755)
	os.Symlink(strings.Repeat("../", 32)+strings.TrimPrefix(outside, "/"), filepath.Join(root3, "etc/terminfo/x"))
	if filed, err := installTerminfo(root3, "", testTerm, macEntry); err != nil || filed != terminfoTarget {
		t.Fatalf("under a climbing link: filed %q, %v", filed, err)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("written outside the root: %v", ents)
	}
	if b, _ := os.ReadFile(filepath.Join(root3, outside, testTerm)); !bytes.Equal(b, macEntry) {
		t.Errorf("not at the link's target inside the root")
	}
}

func TestWhatIsNotAnEntryOrAFileNameIsRefused(t *testing.T) {
	for _, c := range []struct {
		why, name string
		b         []byte
	}{
		{"a script", testTerm, []byte("#!/bin/sh\nrm -rf /\n")},
		{"another terminal's", "xterm-ghostty", macEntry},
		{"cut short", testTerm, macEntry[:20]},
		{"a path", "../../tmp/x", macEntry},
		{"a slash", "x/y", macEntry},
		{"hidden", ".x", macEntry},
		{"too long", strings.Repeat("x", 129), macEntry},
		{"no name", "", macEntry},
		{"a newline", "x\nfake log line", macEntry},
	} {
		root := t.TempDir()
		if filed, err := installTerminfo(root, "/root", c.name, c.b); err == nil || filed != "" {
			t.Errorf("%s: filed %q, %v", c.why, filed, err)
		}
		if ents, _ := os.ReadDir(root); len(ents) != 0 {
			t.Errorf("%s: wrote %v", c.why, ents)
		}
	}
}
