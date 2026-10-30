package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"fev/proto/bridge"
)

func TestTheEnginesAgentIsBoundAtTheMacsPath(t *testing.T) {
	mac := "/Users/me/Library/Group Containers/x/agent.sock"
	cmdline := "console=hvc0 quiet " + bridge.AgentPathWord(mac) + " fev.dirty_bytes=1"
	if got := macAgentPath(cmdline); got != mac {
		t.Fatalf("from %q: %q", cmdline, got)
	}
	if got := macAgentPath("console=hvc0 fev.ssh_auth_sock=relative"); got != "" {
		t.Errorf("a relative path was taken: %q", got)
	}
	if err := bindMacAgent("/nowhere", "/nowhere", ""); err != nil {
		t.Errorf("no path: %v", err)
	}

	root := t.TempDir()
	dir := filepath.Join(root, "Users/me/Library/Group Containers/x")
	os.MkdirAll(dir, 0o755)
	socket := filepath.Join(t.TempDir(), "ssh-auth.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	target := filepath.Join(dir, "agent.sock")
	ml, err := net.Listen("unix", target)
	if err != nil {
		t.Fatal(err)
	}
	defer ml.Close()

	os.MkdirAll(filepath.Join(root, "Users/me/elsewhere"), 0o755)
	os.Symlink("elsewhere", filepath.Join(root, "Users/me/link"))
	os.MkdirAll(filepath.Join(root, "Users/me/adir"), 0o755)
	for _, c := range []struct{ path, why string }{
		{"/Users/me/missing.sock", "has no"},
		{"/Users/me/link/agent.sock", "is a link"},
		{"/Users/me/adir", "is a directory"},
	} {
		if err := bindMacAgent(root, socket, c.path); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: %v, want %q", c.path, err, c.why)
		}
	}
	err = bindMacAgent(root, socket, mac)
	if errors.Is(err, unix.EPERM) || (err != nil && strings.Contains(err.Error(), "operation not permitted")) {
		t.Skipf("no mounts here: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(target, unix.MNT_DETACH)
	c, err := net.Dial("unix", target)
	if err != nil {
		t.Fatalf("the Mac's path does not lead to the agent's socket: %v", err)
	}
	c.Close()
	accepted := make(chan struct{})
	go func() {
		if a, err := l.Accept(); err == nil {
			a.Close()
			close(accepted)
		}
	}()
	c2, err := net.Dial("unix", target)
	if err != nil {
		t.Fatal(err)
	}
	c2.Close()
	<-accepted
}
