package cli

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fev/host/daemon"
	"fev/host/daemon/home"
	"fev/host/storage"
)

func attach(t *testing.T, fs string) (string, func()) {
	t.Helper()
	if _, err := exec.LookPath("hdiutil"); err != nil {
		t.Skip("no hdiutil")
	}
	dir := t.TempDir()
	img, mnt := filepath.Join(dir, "v.dmg"), filepath.Join(dir, "m")
	if out, err := exec.Command("hdiutil", "create", "-size", "96m", "-fs", fs, "-volname", "FEVT", "-type", "UDIF", "-quiet", img).CombinedOutput(); err != nil {
		t.Fatalf("hdiutil create: %v %s", err, out)
	}
	if out, err := exec.Command("hdiutil", "attach", img, "-nobrowse", "-mountpoint", mnt, "-quiet").CombinedOutput(); err != nil {
		t.Fatalf("hdiutil attach: %v %s", err, out)
	}
	detached := false
	detach := func() {
		if !detached {
			detached = true
			exec.Command("hdiutil", "detach", mnt, "-force", "-quiet").Run()
		}
	}
	t.Cleanup(detach)
	return mnt, detach
}

func TestTheDataImageIsMovedAndShown(t *testing.T) {
	h := scratchHome(t)
	l, err := home.Resolve(h, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Ensure(l.DataImage(), storage.Options{}); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 8<<20)
	rand.Read(data)
	f, _ := os.OpenFile(l.DataImage(), os.O_RDWR, 0)
	f.WriteAt(data, 50<<30)
	f.Close()
	os.MkdirAll(filepath.Dir(l.DataImageStamp()), 0o755)
	os.WriteFile(l.DataImageStamp(), []byte(l.DataImage()+"\n"), 0o600)

	var info DataInfo
	r := fev(t, h, "", "data", "show", "--format", "json").want(t, 0)
	json.Unmarshal([]byte(r.out), &info)
	if info.Dir != l.DataDir || info.Placed || !info.Exists || info.Volume != "apfs" {
		t.Fatalf("show: %s", r.out)
	}

	l.Create()
	lock, _, err := home.Lock(l.DaemonLock(), false)
	if err != nil {
		t.Fatal(err)
	}
	mnt, detach := attach(t, "APFS")
	ext := filepath.Join(mnt, "fev")
	r = fev(t, h, "", "data", "move", ext, "--margin-mib", "1").want(t, ExitFailed)
	if !strings.Contains(r.err, "fev is running") {
		t.Fatalf("running: %q", r.err)
	}
	lock.Close()

	exfat, _ := attach(t, "ExFAT")
	r = fev(t, h, "", "data", "move", filepath.Join(exfat, "fev"), "--margin-mib", "1").want(t, ExitFailed)
	if !strings.Contains(r.err, "(exfat)") {
		t.Fatalf("exfat: %q", r.err)
	}

	r = fev(t, h, "", "data", "move", ext, "--margin-mib", "1").want(t, 0)
	if !strings.Contains(r.out, "copied 8.0 MiB") && !strings.Contains(r.out, "copied 8 MiB") {
		t.Logf("move said: %s", r.out)
	}
	r = fev(t, h, "", "data", "show", "--format", "json").want(t, 0)
	json.Unmarshal([]byte(r.out), &info)
	if info.Dir != ext || !info.Placed || info.Image != filepath.Join(ext, storage.ImageName) || info.Default != l.DataDir || info.AllocatedBytes > 9<<20 {
		t.Fatalf("show after the move: %s", r.out)
	}
	if b, _ := os.ReadFile(l.DataImageStamp()); strings.TrimSpace(string(b)) != info.Image {
		t.Errorf("the daemon's note says %q", b)
	}
	if moved, _ := home.Resolve(h, ""); moved.DataImage() != info.Image {
		t.Errorf("a daemon would use %s", moved.DataImage())
	}
	if _, err := os.Stat(l.DataImage()); !os.IsNotExist(err) {
		t.Errorf("the old image is there: %v", err)
	}

	fev(t, h, "", "data", "move", l.DataDir, "--margin-mib", "1").want(t, 0)
	got := make([]byte, len(data))
	f, err = os.Open(l.DataImage())
	if err != nil {
		t.Fatal(err)
	}
	f.ReadAt(got, 50<<30)
	f.Close()
	if string(got) != string(data) {
		t.Error("the image's data differs after two moves")
	}
	if _, err := os.Stat(l.PlacementFile()); !os.IsNotExist(err) {
		t.Errorf("the note is still there")
	}

	fev(t, h, "", "data", "move", ext, "--margin-mib", "1").want(t, 0)
	detach()
	r = fev(t, h, "", "data", "move", l.DataDir, "--margin-mib", "1").want(t, ExitFailed)
	if !strings.Contains(r.err, "is not there") {
		t.Fatalf("gone: %q", r.err)
	}
	fev(t, h, "", "data", "move", l.DataDir, "--leave").want(t, 0)
	if back, _ := home.Resolve(h, ""); back.Placed || back.DataDir != l.DataDir {
		t.Errorf("after --leave: %+v", back)
	}
}

func TestTheMoveKeepsTheDaemonsMargin(t *testing.T) {
	if moveMargin != daemon.LowDiskBytes {
		t.Fatalf("moveMargin %d, LowDiskBytes %d", moveMargin, daemon.LowDiskBytes)
	}
}

func TestTheMoveRefusesFevomsOwnDirectories(t *testing.T) {
	h := scratchHome(t)
	l, err := home.Resolve(h, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Ensure(l.DataImage(), storage.Options{}); err != nil {
		t.Fatal(err)
	}
	for _, dest := range []string{
		filepath.Join(l.Home, "completions"),
		filepath.Join(l.Home, "run"),
		filepath.Join(l.Home, "config", "data"),
		l.Home,
		filepath.Join(l.Home, "data-elsewhere"),
	} {
		r := fev(t, h, "", "data", "move", dest, "--margin-mib", "1").want(t, ExitUsage)
		if !strings.Contains(r.err, "inside fev's home") {
			t.Errorf("%s: %q", dest, r.err)
		}
		if _, err := os.Stat(l.DataImage()); err != nil {
			t.Fatalf("%s: the image moved: %v", dest, err)
		}
	}
	link := filepath.Join(t.TempDir(), "via")
	os.Symlink(l.Home, link)
	fev(t, h, "", "data", "move", filepath.Join(link, "completions"), "--margin-mib", "1").want(t, ExitUsage)
	out := filepath.Join(t.TempDir(), "fevdata")
	fev(t, h, "", "data", "move", out, "--margin-mib", "1").want(t, 0)
	fev(t, h, "", "data", "move", l.DataDir, "--margin-mib", "1").want(t, 0)
}
