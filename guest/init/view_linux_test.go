package main

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"syscall"
	"testing"
)

func fakeProc(t *testing.T, proc, pid, comm, flags, start string) {
	t.Helper()
	dir := filepath.Join(proc, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := pid + " (" + comm + ") S 2 0 0 0 -1 " + flags + " 0 0 0 0 0 0 0 0 20 0 1 0 " + start + " 0 0\n"
	for name, data := range map[string]string{"comm": comm + "\n", "stat": stat} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTheViewsThreadsAreTheOnesItsStartMade(t *testing.T) {
	proc := t.TempDir()
	const kthread = "2129984"
	fakeProc(t, proc, "1", "init", "4194560", "1")
	fakeProc(t, proc, "300", "nfsd", kthread, "500")
	fakeProc(t, proc, "301", "nfsd", kthread, "500")
	fakeProc(t, proc, "302", "nfsd", kthread, "501")
	fakeProc(t, proc, "400", "nfsd", "4194560", "20")
	before := nfsdThreads(proc)
	if len(before) != 3 {
		t.Fatalf("before: %v", before)
	}
	if err := os.RemoveAll(filepath.Join(proc, "302")); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []string{"302", "900", "901", "902"} {
		fakeProc(t, proc, pid, "nfsd", kthread, "9000")
	}
	cg := filepath.Join(t.TempDir(), "fev.view")
	var moved []string
	old := moveToCgroup
	t.Cleanup(func() { moveToCgroup = old })
	moveToCgroup = func(cgroup, pid string) error {
		if cgroup != cg {
			t.Errorf("moved %s to %s", pid, cgroup)
		}
		if pid == "901" {
			return &os.PathError{Op: "write", Path: cgroup + "/cgroup.procs", Err: syscall.ESRCH}
		}
		moved = append(moved, pid)
		return nil
	}
	n, err := placeViewThreads(proc, cg, before)
	sort.Strings(moved)
	if err != nil || n != 3 || !slices.Equal(moved, []string{"302", "900", "902"}) {
		t.Errorf("moved %d (%v), err %v; want 302, 900 and 902, 901 having ended", n, moved, err)
	}
	if fi, err := os.Stat(cg); err != nil || !fi.IsDir() {
		t.Errorf("the cgroup not made: %v", err)
	}
	moved = nil
	moveToCgroup = func(cgroup, pid string) error {
		if pid == "900" {
			return &os.PathError{Op: "write", Path: cgroup + "/cgroup.procs", Err: syscall.EINVAL}
		}
		moved = append(moved, pid)
		return nil
	}
	n, err = placeViewThreads(proc, cg, before)
	sort.Strings(moved)
	if err == nil || n != 3 || !slices.Equal(moved, []string{"302", "901", "902"}) {
		t.Errorf("with one refused: moved %d (%v), err %v", n, moved, err)
	}
}
