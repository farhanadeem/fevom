package looptest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func Tools(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("FEV_TOOLS")
	if dir == "" || os.Geteuid() != 0 {
		t.Skip("needs root and FEV_TOOLS (run tests/run unit guest)")
	}
	if _, err := os.Stat(filepath.Join(dir, "mkfs.btrfs")); err != nil {
		t.Fatalf("FEV_TOOLS: %v", err)
	}
	return dir
}

func Device(t *testing.T, size int64) (dev, backing string) {
	t.Helper()
	backing = filepath.Join(t.TempDir(), "disk.img")
	f, err := os.Create(backing)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	ctl, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no loop devices here: %v", err)
	}
	defer ctl.Close()
	n, err := unix.IoctlRetInt(int(ctl.Fd()), unix.LOOP_CTL_GET_FREE)
	if err != nil {
		t.Fatal(err)
	}
	dev = fmt.Sprintf("/dev/loop%d", n)
	if _, err := os.Stat(dev); err != nil {
		if err := unix.Mknod(dev, unix.S_IFBLK|0o600, int(unix.Mkdev(7, uint32(n)))); err != nil {
			t.Fatal(err)
		}
	}
	loop, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = unix.IoctlSetInt(int(loop.Fd()), unix.LOOP_SET_FD, int(f.Fd()))
	loop.Close()
	if err != nil {
		t.Fatalf("attach %s: %v", dev, err)
	}
	t.Cleanup(func() {
		if loop, err := os.OpenFile(dev, os.O_RDONLY, 0); err == nil {
			unix.IoctlSetInt(int(loop.Fd()), unix.LOOP_CLR_FD, 0)
			loop.Close()
		}
	})
	return dev, backing
}

func Grow(t *testing.T, dev string) {
	t.Helper()
	loop, err := os.OpenFile(dev, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer loop.Close()
	if err := unix.IoctlSetInt(int(loop.Fd()), unix.LOOP_SET_CAPACITY, 0); err != nil {
		t.Fatal(err)
	}
}
