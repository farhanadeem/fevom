package emulation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestInspectionRebasesAbsoluteLinksAndDoesNotBlockOnSpecialFiles(t *testing.T) {
	r := t.TempDir()
	for _, dir := range []string{"etc", "usr/lib", "bin", "usr/bin"} {
		if err := os.MkdirAll(filepath.Join(r, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(r, "usr/lib/os-release"), []byte("ID=ubuntu\nVERSION_ID=26.04\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/lib/os-release", filepath.Join(r, "etc/os-release")); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 20)
	copy(header, "\x7fELF")
	header[5], header[18] = 1, 62
	if err := os.WriteFile(filepath.Join(r, "bin/sh"), header, 0755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(r, "usr/bin/nix"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mknod(filepath.Join(r, "usr/bin/pacman"), unix.S_IFCHR|0600, int(unix.Mkdev(1, 3))); err != nil {
		t.Logf("no device node (%v): the FIFO alone", err)
	}
	in, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(in)
	if _, err := unix.InotifyAddWatch(in, filepath.Join(r, "usr/bin"), unix.IN_OPEN); err != nil {
		t.Fatal(err)
	}
	facts, err := Inspect(r, []string{"sh"}, nil)
	if err != nil || facts.Distro != "ubuntu" || facts.Version != "26.04" || !facts.AMD64 || facts.Sandbox != "" {
		t.Fatalf("%+v %v", facts, err)
	}
	buf := make([]byte, 4096)
	if n, _ := unix.Read(in, buf); n > 0 {
		for at := 0; at+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[at]))
			name := strings.TrimRight(string(buf[at+unix.SizeofInotifyEvent:at+unix.SizeofInotifyEvent+int(ev.Len)]), "\x00")
			t.Errorf("the special file %s was opened", name)
			at += unix.SizeofInotifyEvent + int(ev.Len)
		}
	}
	if err := os.Remove(filepath.Join(r, "etc/os-release")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(r, "etc/os-release")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r, "usr/lib/os-release")); err != nil {
		t.Fatal(err)
	}
	facts, err = Inspect(r, nil, nil)
	if err != nil || facts.Distro != "" {
		t.Fatalf("escaped root: %+v %v", facts, err)
	}
}
