package machines

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRunitsMarksAreSetForAHaltInsideTheMachinesRoot(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "run/runit")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	mode := func(p string) os.FileMode {
		st, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return st.Mode().Perm()
	}
	write := func(p string, m os.FileMode) {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, m); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(run, "stopit"), 0)
	write(filepath.Join(run, "reboot"), 0o100)
	if err := runitMarksFor(root); err != nil {
		t.Fatal(err)
	}
	if m := mode(filepath.Join(run, "stopit")); m != 0o100 {
		t.Errorf("stopit %v, want 0100", m)
	}
	if m := mode(filepath.Join(run, "reboot")); m != 0 {
		t.Errorf("reboot %v, want 0000", m)
	}
	if err := runitMarksFor(root); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(run, "stopit"))
	os.Remove(filepath.Join(run, "reboot"))
	if err := runitMarksFor(root); err != nil {
		t.Fatal(err)
	}
	if m := mode(filepath.Join(run, "stopit")); m != 0o100 {
		t.Errorf("stopit made %v, want 0100", m)
	}
	os.Remove(filepath.Join(run, "stopit"))
	outside := filepath.Join(t.TempDir(), "victim")
	write(outside, 0o600)
	if err := os.Symlink(outside, filepath.Join(run, "stopit")); err != nil {
		t.Fatal(err)
	}
	if err := runitMarksFor(root); err == nil {
		t.Error("a link at stopit was taken")
	}
	if m := mode(outside); m != 0o600 {
		t.Errorf("the link's target became %v", m)
	}
	os.RemoveAll(filepath.Join(root, "run"))
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "run")); err != nil {
		t.Fatal(err)
	}
	if err := runitMarksFor(root); err == nil {
		t.Error("a /run that is a link was taken")
	}
	os.Remove(filepath.Join(root, "run"))
	os.MkdirAll(run, 0o755)
	if err := unix.Mkfifo(filepath.Join(run, "stopit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runitMarksFor(root); err == nil {
		t.Error("a FIFO at stopit was taken")
	}
}
