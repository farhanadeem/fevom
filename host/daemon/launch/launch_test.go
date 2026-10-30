package launch

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"fev/host/daemon/home"
	api "fev/proto/daemon"
)

var bg = context.Background()

var fevd string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fevd-test.")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fevd = filepath.Join(dir, "fevd")
	if out, err := exec.Command("go", "build", "-o", fevd, "fev/host/cmd/fevd").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "cannot build fevd: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func scratch(t *testing.T, env ...string) Options {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bh.")
	if err != nil {
		t.Fatal(err)
	}
	layout, _ := home.Resolve(dir, "")
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	t.Cleanup(func() {
		if info, err := layout.ReadInfo(); err == nil {
			syscall.Kill(info.PID, syscall.SIGKILL)
			for i := 0; i < 2000 && syscall.Kill(info.PID, 0) == nil; i++ {
				time.Sleep(time.Millisecond)
			}
		}
		os.RemoveAll(dir)
	})
	return Options{Layout: layout, Daemon: fevd, Args: []string{"--home", dir, "--fake-vm"}, Client: "test"}
}

func gone(pid int) bool {
	if pid <= 0 {
		return true
	}
	for i := 0; i < 3000; i++ {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func TestDialStartsADaemonWhenThereIsNone(t *testing.T) {
	o := scratch(t)
	c, hello, err := Dial(bg, o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if hello.Home != o.Layout.Home || hello.PID == os.Getpid() || hello.PID <= 0 {
		t.Errorf("hello: %+v", hello)
	}
	st, err := c.VMStart(bg, api.StartParams{})
	if err != nil || st.State != api.StateRunning || !st.Fake {
		t.Fatalf("status: %+v %v", st, err)
	}
	tr, err := c.BootTrace(bg)
	if err != nil || tr.Origin != "client" || len(tr.Stages) < 5 || tr.Stages[0].From != "origin" || tr.Stages[0].MS <= 0 {
		t.Errorf("trace: %+v %v", tr, err)
	}
	c2, hello2, err := Dial(bg, o)
	if err != nil || hello2.PID != hello.PID {
		t.Fatalf("a second client: %+v %v", hello2, err)
	}
	c2.Close()
	if err := Ensure(o); err != nil {
		t.Errorf("Ensure with a daemon running: %v", err)
	}
	if pgid, _ := syscall.Getpgid(hello.PID); pgid == syscall.Getpgrp() {
		t.Error("the daemon shares the test's process group")
	}
}

func TestManyClientsAtOnceStartOneDaemon(t *testing.T) {
	o := scratch(t)
	const clients = 20
	pids := make([]int, clients)
	errs := make([]error, clients)
	var wg sync.WaitGroup
	for i := range pids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, hello, err := Dial(bg, o)
			if err != nil {
				errs[i] = err
				return
			}
			pids[i] = hello.PID
			_, errs[i] = c.Status(bg)
			c.Close()
		}(i)
	}
	wg.Wait()
	for i := range pids {
		if errs[i] != nil || pids[i] != pids[0] || pids[i] == 0 {
			t.Errorf("client %d: daemon pid %d (client 0 reached %d), %v", i, pids[i], pids[0], errs[i])
		}
	}
	out, _ := exec.Command("pgrep", "-f", "fevd --foreground.*--home "+o.Layout.Home).Output()
	if n := len(strings.Fields(string(out))); n != 1 {
		t.Errorf("%d daemons are running in the home:\n%s", n, out)
	}
}

func TestAClientRecoversFromAKilledDaemon(t *testing.T) {
	o := scratch(t)
	c, hello, err := Dial(bg, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	info, _ := o.Layout.ReadInfo()
	if info.PID != hello.PID || info.VMMPID <= 0 {
		t.Fatalf("fevd.json: %+v", info)
	}
	syscall.Kill(hello.PID, syscall.SIGKILL)
	if !gone(hello.PID) {
		t.Fatal("the daemon survived SIGKILL")
	}
	begun := time.Now()
	if !gone(info.VMMPID) || time.Since(begun) > 2500*time.Millisecond {
		t.Errorf("the VMM (pid %d) outlived its daemon by %v", info.VMMPID, time.Since(begun))
	}
	if _, err := os.Stat(o.Layout.Socket()); err != nil {
		t.Fatalf("the premise: kill -9 leaves the socket file behind: %v", err)
	}
	c2, hello2, err := Dial(bg, o)
	if err != nil || hello2.PID == hello.PID {
		t.Fatalf("after kill -9: %+v %v", hello2, err)
	}
	defer c2.Close()
	if st, err := c2.VMStart(bg, api.StartParams{}); err != nil || st.State != api.StateRunning {
		t.Errorf("the new daemon's VM: %+v %v", st, err)
	}
}

func TestDMNS9StopAndAnImmediateStartNeverFail(t *testing.T) {
	type kind struct {
		instructions string
		span         time.Duration
	}
	quick := kind{"stop-ms=0", 80 * time.Millisecond}
	short := kind{"stop-ms=300", 400 * time.Millisecond}
	five := kind{"stop-ms=5000", 5200 * time.Millisecond}
	seventeen := kind{"ignore-shutdown=1 ignore-lifeline=1", 17300 * time.Millisecond}
	schedule := []kind{quick, short, quick, short, quick, short}
	if n, _ := strconv.Atoi(os.Getenv("DMN_S9")); n > 0 {
		schedule = nil
		for i := 0; i < n; i++ {
			switch {
			case i%20 == 19:
				schedule = append(schedule, seventeen)
			case i%20 == 4 || i%20 == 9 || i%20 == 14:
				schedule = append(schedule, five)
			case i%2 == 0:
				schedule = append(schedule, quick)
			default:
				schedule = append(schedule, short)
			}
		}
	}
	o := scratch(t)
	rng := rand.New(rand.NewSource(1))
	var retried, served, newcomerOld int
	var longest time.Duration
	byKind := map[string]int{}
	for cycle, k := range schedule {
		byKind[k.instructions]++
		t.Setenv("FEV_FAKE_VM", k.instructions)
		c, hello, err := Dial(bg, o)
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		existing, _, err := Dial(bg, o)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Shutdown(bg, api.ShutdownParams{}); err != nil {
			t.Fatalf("cycle %d: shutdown: %v", cycle, err)
		}
		delay := time.Duration(rng.Int63n(int64(k.span)))
		time.Sleep(delay)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := existing.Status(bg); err == nil {
				served++
				return
			}
			again, hello2, err := Again(bg, o)
			if err != nil {
				t.Errorf("cycle %d (%s, %v in): the connected client's one retry: %v", cycle, k.instructions, delay, err)
				return
			}
			defer again.Close()
			if _, err := again.Status(bg); err != nil || hello2.PID == hello.PID {
				t.Errorf("cycle %d (%s, %v in): the retry reached pid %d (old %d): %v", cycle, k.instructions, delay, hello2.PID, hello.PID, err)
			}
			retried++
		}()
		go func() {
			defer wg.Done()
			begun := time.Now()
			fresh, hello3, err := Dial(bg, o)
			if err != nil {
				t.Errorf("cycle %d (%s, %v in): a new client: %v", cycle, k.instructions, delay, err)
				return
			}
			defer fresh.Close()
			if _, err := fresh.Status(bg); err != nil {
				if !api.IsShuttingDown(err) {
					t.Errorf("cycle %d (%s, %v in): the new client's call: %v", cycle, k.instructions, delay, err)
					return
				}
				again, _, err := Again(bg, o)
				if err != nil {
					t.Errorf("cycle %d (%s, %v in): the new client's one retry: %v", cycle, k.instructions, delay, err)
					return
				}
				defer again.Close()
				if _, err := again.Status(bg); err != nil {
					t.Errorf("cycle %d: after the retry: %v", cycle, err)
				}
				newcomerOld++
			} else if hello3.PID == hello.PID {
				newcomerOld++
			}
			if waited := time.Since(begun); waited > longest {
				longest = waited
			}
		}()
		wg.Wait()
		existing.Close()
		c.Close()
		if !gone(hello.PID) {
			t.Fatalf("cycle %d: the old daemon (pid %d) never went", cycle, hello.PID)
		}
		if info, err := o.Layout.ReadInfo(); err == nil {
			syscall.Kill(info.PID, syscall.SIGKILL)
			gone(info.PID)
			if info.VMMPID > 0 {
				syscall.Kill(info.VMMPID, syscall.SIGKILL)
				gone(info.VMMPID)
			}
		}
		if t.Failed() {
			t.FailNow()
		}
	}
	t.Logf("%d cycles %v: the connected client was answered by the old daemon %d times and retried once, successfully, %d times; the newcomer met the old daemon %d times; the longest a newcomer waited was %v",
		len(schedule), byKind, served, retried, newcomerOld, longest.Round(time.Millisecond))
}

func TestAStarterIsToldWhyADaemonDidNotStart(t *testing.T) {
	o := scratch(t)
	o.Daemon = "/nonexistent/fevd"
	if _, _, err := Dial(bg, o); err == nil || !strings.Contains(err.Error(), "cannot start /nonexistent/fevd") {
		t.Errorf("no such program: %v", err)
	}

	o = scratch(t)
	o.Daemon = "/usr/bin/false"
	if err := Ensure(o); err == nil || !strings.Contains(err.Error(), "exited before it was ready") {
		t.Errorf("a daemon that exits at once: %v", err)
	}

	o = scratch(t)
	os.MkdirAll(o.Layout.Home, 0o755)
	os.WriteFile(filepath.Join(o.Layout.Home, "run"), []byte("in the way"), 0o644)
	if err := Ensure(o); err == nil || !strings.Contains(err.Error(), "the daemon could not start") || !strings.Contains(err.Error(), "run") {
		t.Errorf("a home that cannot be used: %v", err)
	}

	o = scratch(t)
	script := filepath.Join(t.TempDir(), "hang")
	os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 3\n"), 0o755)
	o.Daemon = script
	o.ReadyTimeout = 200 * time.Millisecond
	begun := time.Now()
	err := Ensure(o)
	if err == nil || !strings.Contains(err.Error(), "did not report ready within 200ms") || time.Since(begun) > 2*time.Second {
		t.Errorf("a daemon that hangs: %v after %v", err, time.Since(begun))
	}
}

func TestPlainFevdStartsTheDaemonAndReturns(t *testing.T) {
	o := scratch(t)
	run := func() (string, error) {
		out, err := exec.Command(fevd, "--home", o.Layout.Home, "--fake-vm", "--no-autostart").CombinedOutput()
		return string(out), err
	}
	if out, err := run(); err != nil {
		t.Fatalf("fevd: %v\n%s", err, out)
	}
	c, hello, err := Dial(bg, Options{Layout: o.Layout, Daemon: "/nonexistent/so-it-must-be-running", Client: "test"})
	if err != nil {
		t.Fatalf("after fevd returned, connecting: %v", err)
	}
	defer c.Close()
	if st, _ := c.Status(bg); st.State != api.StateStopped {
		t.Errorf("status: %+v", st)
	}
	if out, err := run(); err != nil {
		t.Fatalf("fevd with a daemon running: %v\n%s", err, out)
	}
	if _, hello2, _ := Dial(bg, o); hello2 == nil || hello2.PID != hello.PID {
		t.Errorf("a second fevd replaced the daemon: %+v", hello2)
	}
	syscall.Kill(hello.PID, syscall.SIGTERM)
	if !gone(hello.PID) {
		t.Error("the daemon did not stop on SIGTERM")
	}
	if _, err := os.Stat(o.Layout.Socket()); err == nil {
		t.Error("an orderly shutdown left the socket behind")
	}
}
