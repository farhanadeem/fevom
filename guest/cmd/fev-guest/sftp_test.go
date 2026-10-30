//go:build linux

package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
)

func TestMain(m *testing.M) {
	if args := os.Getenv("FEV_GUEST_TEST_ARGS"); args != "" {
		os.Exit(dispatch("fev-guest", filepath.SplitList(args)))
	}
	os.Exit(m.Run())
}

func TestFevomGuestServesSFTP(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "hello"), []byte("from the machine\n"), 0o644)
	cmd := exec.Command(os.Args[0])
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "FEV_GUEST_TEST_ARGS=sftp")
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c, err := sftp.NewClientPipe(out, in)
	if err != nil {
		t.Fatal(err)
	}
	if wd, err := c.Getwd(); err != nil || wd != home {
		t.Errorf("starts in %q (%v), want %s", wd, err, home)
	}
	f, err := c.Open("hello")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "from the machine\n" {
		t.Errorf("read %q", b)
	}
	c.Close()
	if err := cmd.Wait(); err != nil {
		t.Errorf("fev-guest sftp ended with %v", err)
	}
	if code := dispatch("fev-guest", []string{"sftp", "extra"}); code != 2 {
		t.Errorf("fev-guest sftp extra: %d", code)
	}
}
