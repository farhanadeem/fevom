package sftpserver

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
)

type pipes struct {
	io.Reader
	io.WriteCloser
}

func TestTheServerStartsInTheHomeAndServesFiles(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "hello"), []byte("from the machine\n"), 0o644)
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(pipes{sr, sw}, home) }()
	c, err := sftp.NewClientPipe(cr, cw)
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
	w, err := c.Create("sub-made")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("from the Mac"))
	w.Close()
	if err := c.Rename("sub-made", "made"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(home, "made")); err != nil || string(b) != "from the Mac" {
		t.Errorf("wrote %q, %v", b, err)
	}
	list, err := c.ReadDir(".")
	if err != nil || len(list) != 2 {
		t.Errorf("listed %d (%v)", len(list), err)
	}
	c.Close()
	if err := <-done; err != nil {
		t.Errorf("the server ended with %v", err)
	}
}
