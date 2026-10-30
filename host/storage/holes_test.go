package storage

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func volume(t *testing.T, fs string) string {
	t.Helper()
	return volumeOf(t, fs, "64m")
}

func volumeOf(t *testing.T, fs, size string) string {
	t.Helper()
	if _, err := exec.LookPath("hdiutil"); err != nil {
		t.Skip("no hdiutil")
	}
	dir := t.TempDir()
	img := filepath.Join(dir, "v.dmg")
	mnt := filepath.Join(dir, "m")
	if out, err := exec.Command("hdiutil", "create", "-size", size, "-fs", fs, "-volname", "FEVT", "-type", "UDIF", "-quiet", img).CombinedOutput(); err != nil {
		t.Fatalf("hdiutil create %s: %v %s", fs, err, out)
	}
	if out, err := exec.Command("hdiutil", "attach", img, "-nobrowse", "-mountpoint", mnt, "-quiet").CombinedOutput(); err != nil {
		t.Fatalf("hdiutil attach: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("hdiutil", "detach", mnt, "-force", "-quiet").Run() })
	return mnt
}

func TestTheImageIsMadeOnlyWhereHolesAreKept(t *testing.T) {
	for _, c := range []struct {
		fs, kind string
		ok       bool
	}{{"APFS", "apfs", true}, {"HFS+", "hfs", false}, {"ExFAT", "exfat", false}} {
		t.Run(c.fs, func(t *testing.T) {
			mnt := volume(t, c.fs)
			path := filepath.Join(mnt, "data", ImageName)
			created, err := Ensure(path, Options{})
			if c.ok {
				if err != nil || !created {
					t.Fatalf("Ensure on %s: %v %v", c.fs, created, err)
				}
				if a, _ := Allocated(path); a > 1<<20 {
					t.Errorf("%d bytes allocated", a)
				}
				return
			}
			if !errors.Is(err, ErrNoHoles) || !strings.Contains(err.Error(), "("+c.kind+")") {
				t.Fatalf("Ensure on %s: %v", c.fs, err)
			}
			entries, _ := os.ReadDir(filepath.Join(mnt, "data"))
			for _, e := range entries {
				if !strings.HasPrefix(e.Name(), "._") {
					t.Errorf("left behind: %s", e.Name())
				}
			}
		})
	}
}
