package spawn

import (
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestASpawnedProgramGetsWhatItIsGiven(t *testing.T) {
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	if err := syscall.Dup2(int(inR.Fd()), 57); err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(57)
	dir := t.TempDir()
	script := `read line; echo "$line $V $(pwd -P)"; echo err >&2; ` +
		`[ "$(ps -o pgid= -p $$ | tr -d ' ')" = "$$" ] && echo own-group; ` +
		`/bin/ls /dev/fd/ | grep -qx 57 && echo leaked; echo; exit 3`
	p, err := Start("/bin/sh", []string{"sh", "-c", script}, []string{"V=v1", "PATH=/usr/bin:/bin"}, dir, [3]*os.File{inR, outW, errW})
	if err != nil {
		t.Fatal(err)
	}
	inR.Close()
	outW.Close()
	errW.Close()
	io.WriteString(inW, "hello\n")
	inW.Close()
	out, _ := io.ReadAll(outR)
	errs, _ := io.ReadAll(errR)
	st, err := p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	real, _ := os.Readlink(dir)
	if real == "" {
		real = dir
	}
	lines := strings.Split(string(out), "\n")
	if !strings.HasPrefix(lines[0], "hello v1 ") || !strings.HasSuffix(lines[0], strings.TrimPrefix(dir, "/private")) {
		t.Errorf("first line %q (dir %s)", lines[0], dir)
	}
	if len(lines) < 3 || lines[1] != "own-group" {
		t.Errorf("output %q", out)
	}
	if strings.Contains(string(out), "leaked") {
		t.Error("the child had the daemon's descriptor 57")
	}
	if string(errs) != "err\n" || st.ExitCode() != 3 {
		t.Errorf("stderr %q, status %v", errs, st)
	}
}

func TestASpawnThatCannotStartSaysWhy(t *testing.T) {
	null, _ := os.Open("/dev/null")
	defer null.Close()
	files := [3]*os.File{null, null, null}
	if _, err := Start("/no/such/program", []string{"x"}, nil, "/", files); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("missing: %v", err)
	}
	if _, err := Start("/etc/hosts", []string{"x"}, nil, "/", files); !errors.Is(err, syscall.EACCES) {
		t.Errorf("not executable: %v", err)
	}
	if _, err := Start("/bin/sh", []string{"sh"}, nil, "/no/such/dir", files); err == nil {
		t.Error("a missing directory was taken")
	}
}
