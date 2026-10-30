package webctl

import (
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const probeEnv = "WEBCTL_PROBE_FDS"

func TestTheHeldListenersAreNotInherited(t *testing.T) {
	h, err := Hold(netip.MustParseAddr("127.0.0.1"), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var fds []string
	for _, f := range h.Files() {
		fd := int(f.Fd())
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil {
			t.Fatal(err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			t.Errorf("listener %d is not close-on-exec", fd)
		}
		fds = append(fds, strconv.Itoa(fd))
	}
	probe := func(files []*os.File, look string) string {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestHeldProbe$")
		cmd.Env = append(os.Environ(), probeEnv+"="+look)
		cmd.ExtraFiles = files
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("the probe: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := probe(nil, strings.Join(fds, ",")); got != "" {
		t.Errorf("a process started with nothing passed has init's listeners: %s", got)
	}
	if got := probe(h.Files(), "3,4"); got != "open 3\nopen 4" {
		t.Errorf("a process passed the listeners sees %q, want both", got)
	}
}

func TestHeldProbe(t *testing.T) {
	look := os.Getenv(probeEnv)
	if look == "" {
		t.Skip("run by TestTheHeldListenersAreNotInherited")
	}
	for _, s := range strings.Split(look, ",") {
		n, err := strconv.Atoi(s)
		if err != nil {
			os.Exit(2)
		}
		if _, err := unix.FcntlInt(uintptr(n), unix.F_GETFD, 0); err == nil {
			fmt.Printf("open %d\n", n)
		}
	}
	os.Exit(0)
}
