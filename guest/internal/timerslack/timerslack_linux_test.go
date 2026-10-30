package timerslack

import (
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestAProgramStartedWithASlackHasItAndTheStarterKeepsItsOwn(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	was, err := Get()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	pid, err := StartWith(Cluster, func() (int, error) {
		return syscall.ForkExec("/bin/sh", []string{"sh", "-c", "cat /proc/self/timerslack_ns; sh -c 'cat /proc/self/timerslack_ns'"},
			&syscall.ProcAttr{Files: []uintptr{0, w.Fd(), 2}})
	})
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	var ws syscall.WaitStatus
	syscall.Wait4(pid, &ws, 0, nil)
	b, _ := io.ReadAll(r)
	got := strings.Fields(string(b))
	want := strconv.Itoa(Cluster)
	if len(got) != 2 || got[0] != want || got[1] != want {
		t.Errorf("the program and its child have a slack of %q, not %s", got, want)
	}
	if now, _ := Get(); now != was {
		t.Errorf("the starting thread's slack is %d after, not its own %d", now, was)
	}
}
