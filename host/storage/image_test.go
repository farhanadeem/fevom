package storage

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestANewImageIsSparseAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", ImageName)
	created, err := Ensure(path, Options{})
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != DefaultSize {
		t.Fatalf("apparent size %d, want %d", st.Size(), int64(DefaultSize))
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", st.Mode().Perm())
	}
	if n, err := Allocated(path); err != nil || n != 0 {
		t.Fatalf("allocated %d bytes (err %v), want 0", n, err)
	}
	if _, err := os.Stat(path + ".new"); err == nil {
		t.Fatal("the temporary file was left behind")
	}
}

func TestAllocatedCountsWhatIsWrittenNotTheSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), ImageName)
	if _, err := Ensure(path, Options{Size: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(bytes.Repeat([]byte{1}, 1<<20), 512<<20); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	n, err := Allocated(path)
	if err != nil || n < 1<<20 || n > 64<<20 {
		t.Fatalf("allocated %d after writing 1 MiB (err %v)", n, err)
	}
}

func TestEnsureKeepsAnExistingImageAndGrowsASmallOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), ImageName)
	if _, err := Ensure(path, Options{Size: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("guest data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 1<<20); err != nil {
		t.Fatal(err)
	}
	created, err := Ensure(path, Options{Size: 2 << 20})
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	data, _ := os.ReadFile(path)
	if len(data) != 2<<20 || !bytes.HasPrefix(data, []byte("guest data")) {
		t.Fatalf("after growing: %d bytes, starts %q", len(data), data[:10])
	}
	if _, err := Ensure(path, Options{Size: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Size() != 2<<20 {
		t.Fatalf("shrunk to %d", st.Size())
	}
	for _, bad := range []int64{-4096, 1000} {
		if _, err := Ensure(filepath.Join(t.TempDir(), "x"), Options{Size: bad}); err == nil {
			t.Fatalf("size %d accepted", bad)
		}
	}
	if _, err := Ensure(t.TempDir(), Options{}); err == nil {
		t.Fatal("a directory was accepted as the image")
	}
}

func TestResetRemovesTheImageAndNothingElse(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "config.json")
	if err := os.WriteFile(config, []byte(`{"cpus":4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := ImagePath(home)
	if _, err := Ensure(path, Options{Size: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{7}, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Reset(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the image is still there")
	}
	if data, err := os.ReadFile(config); err != nil || string(data) != `{"cpus":4}` {
		t.Fatalf("configuration changed: %q %v", data, err)
	}
	if err := Reset(path); err != nil {
		t.Fatal(err)
	}
	created, err := Ensure(path, Options{Size: 1 << 30})
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if n, _ := Allocated(path); n != 0 {
		t.Fatalf("the new image occupies %d bytes", n)
	}
	if ex, _ := BackupExcluded(path); !ex {
		t.Fatal("the new image is not excluded from backups")
	}
}

func TestBackupExclusionIsSetReadBackAndFollowsTheSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), ImageName)
	if _, err := Ensure(path, Options{Size: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	n, err := unix.Getxattr(path, "com.apple.metadata:com_apple_backup_excludeItem", buf)
	if err != nil {
		t.Fatalf("the attribute is not on a new image: %v", err)
	}
	if n != 61 || !bytes.HasPrefix(buf[:n], []byte("bplist00")) || !bytes.Contains(buf[:n], []byte("com.apple.backupd")) {
		t.Fatalf("attribute value is %d bytes: %x", n, buf[:n])
	}
	if ex, err := BackupExcluded(path); err != nil || !ex {
		t.Fatalf("excluded=%v err=%v", ex, err)
	}

	if _, err := Ensure(path, Options{Size: 1 << 20, AllowBackup: true}); err != nil {
		t.Fatal(err)
	}
	if ex, err := BackupExcluded(path); err != nil || ex {
		t.Fatalf("with backups allowed: excluded=%v err=%v", ex, err)
	}
	if _, err := Ensure(path, Options{Size: 1 << 20, AllowBackup: true}); err != nil {
		t.Fatal(err)
	}

	if _, err := Ensure(path, Options{Size: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Removexattr(path, backupAttr); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(path, Options{Size: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if ex, _ := BackupExcluded(path); !ex {
		t.Fatal("the attribute was not put back")
	}

	other := filepath.Join(t.TempDir(), ImageName)
	if _, err := Ensure(other, Options{Size: 1 << 20, AllowBackup: true}); err != nil {
		t.Fatal(err)
	}
	if ex, _ := BackupExcluded(other); ex {
		t.Fatal("excluded although backups are allowed")
	}
}

func TestBackupExclusionAgreesWithTimeMachine(t *testing.T) {
	tmutil, err := exec.LookPath("tmutil")
	if err != nil {
		t.Skip("no tmutil on this system")
	}
	dir, err := os.MkdirTemp(".", ".tm-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	query := func(path string) string {
		abs, _ := filepath.Abs(path)
		out, err := exec.Command(tmutil, "isexcluded", abs).CombinedOutput()
		if err != nil {
			t.Fatalf("tmutil isexcluded: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	ours := filepath.Join(dir, ImageName)
	if err := os.WriteFile(ours, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if before := query(ours); !strings.HasPrefix(before, "[Included]") {
		t.Skipf("this checkout is not in a backed-up place, so the check says nothing: %s", before)
	}
	if err := SetBackupExcluded(ours, true); err != nil {
		t.Fatal(err)
	}
	if got := query(ours); !strings.HasPrefix(got, "[Excluded]") {
		t.Fatalf("after excluding, Time Machine says %s", got)
	}

	theirs := filepath.Join(dir, "tmutil.img")
	if err := os.WriteFile(theirs, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(tmutil, "addexclusion", theirs).CombinedOutput(); err != nil {
		t.Fatalf("tmutil addexclusion: %v: %s", err, out)
	}
	buf := make([]byte, 256)
	n, err := unix.Getxattr(theirs, backupAttr, buf)
	if err != nil {
		t.Fatalf("tmutil set no attribute: %v", err)
	}
	if !bytes.Equal(buf[:n], backupValue) {
		t.Fatalf("tmutil writes\n%x\nand we write\n%x", buf[:n], backupValue)
	}

	if err := SetBackupExcluded(ours, false); err != nil {
		t.Fatal(err)
	}
	if got := query(ours); !strings.HasPrefix(got, "[Included]") {
		t.Fatalf("after including, Time Machine says %s", got)
	}
}
