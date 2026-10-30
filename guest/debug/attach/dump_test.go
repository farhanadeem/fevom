package attach

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func fakeStack(layers ...[]Insn) func(int) ([]Insn, error) {
	return func(idx int) ([]Insn, error) {
		if idx < len(layers) {
			return layers[idx], nil
		}
		return nil, errNoIndex
	}
}

func layer(k uint32) []Insn { return []Insn{{Code: insnRETK, K: k}} }

func TestReadStackKeepsTheKernelsOrder(t *testing.T) {
	old, newest := layer(0x00050000|95), layer(0x00050000|30)
	got, err := readStack(fakeStack(old, newest), 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0][0].K != old[0].K || got[1][0].K != newest[0].K {
		t.Fatalf("stack %v, want the oldest first", got)
	}
}

func TestReadStackFailsClosed(t *testing.T) {
	cases := []struct {
		name          string
		get           func(int) ([]Insn, error)
		mode, filters int
		want          string
	}{
		{"unconfined", func(int) ([]Insn, error) { return nil, errNoFilter }, 0, 0, ""},
		{"index 0 EINVAL but the status says filters", func(int) ([]Insn, error) { return nil, errNoFilter }, 2, 1, "status says"},
		{"ESRCH at index 0 (a tracer thread lost)", func(int) ([]Insn, error) { return nil, syscall.ESRCH }, 2, 1, "no such process"},
		{"EACCES at index 0", func(int) ([]Insn, error) { return nil, syscall.EACCES }, 2, 1, "permission denied"},
		{"an error past index 0", func(i int) ([]Insn, error) {
			if i == 0 {
				return layer(1), nil
			}
			return nil, syscall.ESRCH
		}, 2, 2, "no such process"},
		{"fewer read than the status says", fakeStack(layer(1)), 2, 2, "read 1"},
		{"more read than the status says", fakeStack(layer(1), layer(2)), 2, 1, "read 2"},
		{"an empty filter", fakeStack([]Insn{}), 2, 1, "empty"},
		{"past the stage's bound", func(int) ([]Insn, error) { return layer(1), nil }, 2, 99, fmt.Sprintf("more than %d", MaxSeccompLayers)},
	}
	for _, c := range cases {
		got, err := readStack(c.get, c.mode, c.filters)
		if c.want == "" {
			if err != nil || got != nil {
				t.Errorf("%s: %v %v", c.name, got, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, %v; want an error with %q", c.name, len(got), err, c.want)
		}
	}
	if errors.Is(errNoIndex, errNoFilter) {
		t.Fatal("sentinels alias")
	}
}

func writeProc(t *testing.T, root string, pid int, pidns uint64, nspid string, start uint64) {
	t.Helper()
	d := filepath.Join(root, fmt.Sprint(pid))
	os.MkdirAll(filepath.Join(d, "ns"), 0o755)
	os.Symlink(fmt.Sprintf("pid:[%d]", pidns), filepath.Join(d, "ns", "pid"))
	os.WriteFile(filepath.Join(d, "status"), []byte("Name:\tx\nNSpid:\t"+nspid+"\nSeccomp:\t2\n"), 0o644)
	os.WriteFile(filepath.Join(d, "stat"), []byte(fmt.Sprintf("%d (a (b) c) S 1 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 0", pid, start)), 0o644)
}

func TestFindTargetMatchesTheNamespace(t *testing.T) {
	proc := t.TempDir()
	writeProc(t, proc, 100, 4026532001, "100 7", 50)
	writeProc(t, proc, 200, 4026532002, "200 7 1", 60)
	writeProc(t, proc, 300, 4026532003, "300 7 1", 70)
	writeProc(t, proc, 400, 4026532004, "400 9", 80)
	pid, err := findTarget(proc, 4026532003, 1, 70)
	if err != nil || pid != 300 {
		t.Fatalf("found %d, %v", pid, err)
	}
	if _, err := findTarget(proc, 4026532003, 1, 71); err == nil {
		t.Fatal("a process that started at another time was taken")
	}
	if _, err := findTarget(proc, 4026532099, 1, 70); err == nil {
		t.Fatal("found a target in a namespace no process is in")
	}
	writeProc(t, proc, 500, 4026532005, "500 42", 90)
	writeProc(t, proc, 501, 4026532005, "501 43", 91)
	if pid, err := findTarget(proc, 4026532005, 43, 91); err != nil || pid != 501 {
		t.Fatalf("a host-PID target: %d, %v", pid, err)
	}
}

func TestStatusParsers(t *testing.T) {
	if n, err := LastNSpid([]byte("NSpid:\t812\t1\n")); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if v, err := StartTime([]byte("12 (we ird) name) R 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 4242 0")); err != nil || v != 4242 {
		t.Fatalf("%d %v", v, err)
	}
	if m, f, err := seccompStatus([]byte("Seccomp:\t2\nSeccomp_filters:\t3\n")); err != nil || m != 2 || f != 3 {
		t.Fatalf("%d %d %v", m, f, err)
	}
}
