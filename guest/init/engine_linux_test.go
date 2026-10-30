package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"fev/guest/internal/logbuf"
	"fev/guest/internal/reaper"
)

func TestRCUIsExpeditedWhileHeldAndNoLonger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rcu_expedited")
	os.WriteFile(path, []byte("0"), 0o644)
	r := &rcuHolds{path: path, logf: t.Errorf}
	value := func() string {
		data, _ := os.ReadFile(path)
		return string(data)
	}
	boot := r.hold()
	engine := r.hold()
	if value() != "1" {
		t.Fatalf("held twice: %q", value())
	}
	boot()
	boot()
	if value() != "1" {
		t.Fatalf("one hold left, released the other twice: %q", value())
	}
	engine()
	if value() != "0" {
		t.Fatalf("no holds left: %q", value())
	}
	r.hold()()
	if value() != "0" {
		t.Fatalf("after a hold taken and released: %q", value())
	}
}

func TestPopulatedReadsTheFlagFromCgroupEvents(t *testing.T) {
	dir := t.TempDir()
	for content, want := range map[string]bool{
		"populated 1\nfrozen 0\n": true,
		"populated 0\nfrozen 0\n": false,
		"frozen 0\npopulated 1\n": true,
	} {
		path := filepath.Join(dir, "cgroup.events")
		os.WriteFile(path, []byte(content), 0o644)
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := populated(f)
		f.Close()
		if err != nil || got != want {
			t.Errorf("%q: %v, %v", content, got, err)
		}
	}
	path := filepath.Join(dir, "empty")
	os.WriteFile(path, []byte("frozen 0\n"), 0o644)
	f, _ := os.Open(path)
	defer f.Close()
	if _, err := populated(f); err == nil {
		t.Error("a file with no populated line was accepted")
	}
	if !waitEmpty(filepath.Join(dir, "gone"), 0) {
		t.Error("a missing cgroup was not taken for empty")
	}
}

func TestOnlyReadyFromDockerdItselfIsReadiness(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	if err := unix.SetsockoptInt(fds[1], unix.SOL_SOCKET, unix.SO_PASSCRED, 1); err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fds[1]), "notify")
	defer f.Close()
	conn, err := net.FileConn(f)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	unix.Write(fds[0], []byte("READY=1"))
	buf, oob := make([]byte, 64), make([]byte, unix.CmsgSpace(unix.SizeofUcred))
	n, oobn, _, _, err := conn.(*net.UnixConn).ReadMsgUnix(buf, oob)
	if err != nil || string(buf[:n]) != "READY=1" {
		t.Fatalf("read %q, %v", buf[:n], err)
	}
	if pid := senderPid(oob[:oobn]); pid != os.Getpid() {
		t.Fatalf("sender is %d, we are %d", pid, os.Getpid())
	}
	if senderPid(nil) != 0 {
		t.Fatal("a datagram with no credentials has a sender")
	}

	sys := &system{log: logbuf.New(8, func() float64 { return 0 }, nil)}
	e := &engine{sys: sys}
	run := &dockerdRun{child: &reaper.Child{Pid: 4240}, dockerd: 4242, said: make(chan struct{})}
	e.cur = run
	said := func() bool {
		select {
		case <-run.said:
			return true
		default:
			return false
		}
	}
	e.notified([]byte("READY=1"), 4243)
	e.notified([]byte("READY=1"), 4240)
	if said() {
		t.Fatal("another process's READY=1, or the supervisor's, was taken for dockerd's")
	}
	e.notified([]byte("STOPPING=1"), 4242)
	e.notified([]byte("RELOADING=1\nMONOTONIC_USEC=1"), 4242)
	e.notified([]byte("READY=10"), 4242)
	if said() {
		t.Fatal("a state other than READY=1 was taken for readiness")
	}
	e.notified([]byte("STATUS=up\nREADY=1\n"), 4242)
	if !said() {
		t.Fatal("READY=1 from dockerd was not taken")
	}
	e.notified([]byte("READY=1"), 4242)
}
