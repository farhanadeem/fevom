package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestTheSupervisorsReportIsRead(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("dockerd 7\nexit 2304\n"))
	var r supervisorReport
	if ok, err := readReport(br, &r, nil); !ok || err != nil || r.dockerd != 7 || r.exited {
		t.Fatalf("first line: %v %v %+v", ok, err, r)
	}
	if ok, err := readReport(br, &r, nil); !ok || err != nil || !r.exited || r.status.ExitStatus() != 9 {
		t.Fatalf("second line: %v %v %+v", ok, err, r)
	}
	if ok, _ := readReport(br, &r, nil); ok {
		t.Fatal("a line read past the end")
	}
	r = supervisorReport{}
	readReport(bufio.NewReader(strings.NewReader("exit 9\n")), &r, nil)
	if !r.exited || !r.status.Signaled() || r.status.Signal() != unix.SIGKILL {
		t.Fatalf("signal: %+v", r)
	}
	r = supervisorReport{}
	if ok, err := readReport(bufio.NewReader(strings.NewReader("error mount the engine's /proc: EPERM\n")), &r, nil); !ok || err != nil || r.err == nil || !strings.Contains(r.err.Error(), "EPERM") {
		t.Fatalf("error: %v %v %+v", ok, err, r)
	}
	for _, bad := range []string{"dockerd x\n", "dockerd 0\n", "exit -1\n", "hello\n"} {
		r = supervisorReport{}
		if ok, err := readReport(bufio.NewReader(strings.NewReader(bad)), &r, nil); !ok || err == nil {
			t.Errorf("%q: %v %v", bad, ok, err)
		}
	}
}

func TestTheSupervisorExitsWithDockerdsStatus(t *testing.T) {
	if got := exitCode(unix.WaitStatus(3 << 8)); got != 3 {
		t.Errorf("exit 3: %d", got)
	}
	if got := exitCode(unix.WaitStatus(unix.SIGKILL)); got != 137 {
		t.Errorf("signal 9: %d", got)
	}
}

func TestDockerdsPidIsFoundInInitsNumbering(t *testing.T) {
	proc := t.TempDir()
	write := func(path, content string) {
		p := filepath.Join(proc, path)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	write("500/task/500/children", "601 ")
	write("500/task/503/children", "640 ")
	write("601/status", "Name:\tsleep\nNSpid:\t601\t12\n")
	write("640/status", "Name:\tdockerd\nNSpid:\t640\t2\n")
	if got := outerPid(proc, 500, 2); got != 640 {
		t.Fatalf("dockerd is %d, want 640", got)
	}
	if got := outerPid(proc, 500, 3); got != 0 {
		t.Fatalf("a pid nobody has: %d", got)
	}
	if got := outerPid(proc, 501, 2); got != 0 {
		t.Fatalf("a supervisor that does not exist: %d", got)
	}
	if got := nsPidOf("/proc", os.Getpid()); got != os.Getpid() {
		t.Fatalf("NSpid of this process: %d", got)
	}
}

func TestTheSupervisorReapsOrphansAndDockerd(t *testing.T) {
	start := func(code string) int {
		pid, err := syscall.ForkExec("/bin/sh", []string{"sh", "-c", "exit " + code}, &syscall.ProcAttr{})
		if err != nil {
			t.Skipf("no /bin/sh to start: %v", err)
		}
		return pid
	}
	other := start("0")
	dockerd := start("5")
	var ws unix.WaitStatus
	for _, pid := range []int{other, dockerd} {
		var info unix.Siginfo
		if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil); err != nil {
			t.Fatal(err)
		}
	}
	ws, ok := reapAll(dockerd)
	if !ok || ws.ExitStatus() != 5 {
		t.Fatalf("dockerd's end: %v %v", ok, ws)
	}
	if err := unix.Kill(other, 0); err == nil {
		t.Fatal("the other child was not reaped")
	}
	if _, ok := reapAll(dockerd); ok {
		t.Fatal("dockerd's end reported twice")
	}
}

func TestTheEnginesOSReleaseNamesFevomAndTheBuild(t *testing.T) {
	got := osRelease("021b79a-dirty.0b75cdf4")
	for _, line := range []string{`NAME="fev"`, "ID=fev", "VERSION_ID=021b79a-dirty.0b75cdf4", `PRETTY_NAME="fev"`} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("no %s in %q", line, got)
		}
	}
}
