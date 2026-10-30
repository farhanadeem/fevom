package machines

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestUsersAreReadInsideTheMachinesRoot(t *testing.T) {
	top := t.TempDir()
	root := filepath.Join(top, "root")
	for _, d := range []string{"etc", "inner"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "inner", "passwd"), []byte("root:x:0:0::/root:/bin/sh\nu:x:1000:1000::/home/u:/bin/bash\n"), 0o644)
	if err := os.Symlink("/inner/passwd", filepath.Join(root, "etc", "passwd")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "etc", "group"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var u *User
	var err error
	go func() {
		u, err = lookupUser(root, "u")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("lookupUser waited on a FIFO at /etc/group")
	}
	if err != nil || u.UID != 1000 || u.Home != "/home/u" || u.Shell != "/bin/bash" {
		t.Fatalf("through an absolute link inside the root: %+v %v", u, err)
	}
	os.MkdirAll(filepath.Join(top, "outside"), 0o755)
	os.WriteFile(filepath.Join(top, "outside", "passwd"), []byte("evil:x:0:0::/:/bin/sh\n"), 0o644)
	os.Remove(filepath.Join(root, "etc", "passwd"))
	if err := os.Symlink("../../outside/passwd", filepath.Join(root, "etc", "passwd")); err != nil {
		t.Fatal(err)
	}
	if u, err := lookupUser(root, "evil"); err == nil {
		t.Fatalf("read a passwd outside the machine's root: %+v", u)
	}
	os.Remove(filepath.Join(root, "etc", "passwd"))
	unix.Mkfifo(filepath.Join(root, "etc", "passwd"), 0o644)
	t0 := time.Now()
	if _, err := lookupUser(root, "u"); err == nil || time.Since(t0) > 5*time.Second {
		t.Fatalf("a FIFO as /etc/passwd: %v after %v", err, time.Since(t0))
	}
}
