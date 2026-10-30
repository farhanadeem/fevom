package machines

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func inPrivateMounts(t *testing.T, fn func()) {
	t.Helper()
	if os.Getenv("FEV_TEST_OWN_MOUNTS") == t.Name() {
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
			t.Fatalf("make / private: %v", err)
		}
		fn()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), "FEV_TEST_OWN_MOUNTS="+t.Name())
	cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
	out, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(err.Error(), "operation not permitted") {
		t.Skipf("no mount namespace of its own (unprivileged): %v", err)
	}
	if err != nil || !strings.Contains(string(out), "--- PASS") {
		t.Fatalf("in a mount namespace of its own: %v\n%s", err, out)
	}
}

func TestTheAgentIsBoundAtTheMacsPathAndMoves(t *testing.T) {
	top := t.TempDir()
	sock := filepath.Join(top, "agent.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("agent"))
			c.Close()
		}
	}()
	root := filepath.Join(top, "root")
	for _, d := range []string{"private/tmp/L1", "private/tmp/L2", "private/tmp/dir/Listeners", "real"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"private/tmp/L1/Listeners", "private/tmp/L2/Listeners", "real/Listeners"} {
		os.WriteFile(filepath.Join(root, f), nil, 0o600)
	}
	os.Symlink("../real", filepath.Join(root, "private/tmp/link"))
	answers := func(p string) bool {
		c, err := net.Dial("unix", p)
		if err != nil {
			return false
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		return string(b) == "agent"
	}
	mounts := func(p string) int {
		b, _ := os.ReadFile("/proc/thread-self/mountinfo")
		n := 0
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) > 4 && f[4] == p {
				n++
			}
		}
		return n
	}
	inPrivateMounts(t, func() {
		if err := BindAgentAt(root, sock, "/private/tmp/L1/Listeners"); err != nil {
			t.Error(err)
			return
		}
		l1, l2 := filepath.Join(root, "private/tmp/L1/Listeners"), filepath.Join(root, "private/tmp/L2/Listeners")
		if !answers(l1) {
			t.Error("the agent does not answer at the Mac's path")
			return
		}
		for p, want := range map[string]string{
			"/private/tmp/none/Listeners": "no such file",
			"/private/tmp/link/Listeners": "is a link",
			"/private/tmp/dir/Listeners":  "is a directory",
			"/private/tmp/L1/Listeners/x": "not a directory",
		} {
			err := BindAgentAt(root, sock, p)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %v, want %q", p, err, want)
			}
		}
		if err := BindAgentAt(root, sock, "/private/tmp/none/Listeners"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a path the tree has not: %v", err)
		}
		if answers(filepath.Join(root, "real/Listeners")) {
			t.Error("bound through a link")
		}

		at, err := MoveAgent(sock, l1, l2)
		if err != nil || !at {
			t.Errorf("moved: %v %v", at, err)
			return
		}
		if answers(l1) || !answers(l2) {
			t.Errorf("after the move: old answers %v, new %v", answers(l1), answers(l2))
		}
		if at, err := MoveAgent(sock, l2, l2); err != nil || !at || mounts(l2) != 1 {
			t.Errorf("again: %v %v, %d mounts", at, err, mounts(l2))
		}
		other := filepath.Join(top, "other")
		os.WriteFile(other, []byte("theirs"), 0o600)
		if err := unix.Mount(other, l1, "", unix.MS_BIND, ""); err != nil {
			t.Error(err)
			return
		}
		if _, err := MoveAgent(sock, l1, l2); err != nil {
			t.Error(err)
			return
		}
		if b, _ := os.ReadFile(l1); string(b) != "theirs" {
			t.Errorf("another's mount at the old path went: %q", b)
		}
		unix.Unmount(l1, unix.MNT_DETACH)
		if at, err := MoveAgent(sock, l2, ""); err != nil || at || answers(l2) {
			t.Errorf("to none: %v %v, answers %v", at, err, answers(l2))
		}
	})
}
