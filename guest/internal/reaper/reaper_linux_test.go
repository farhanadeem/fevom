package reaper

import (
	"os"
	"os/signal"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func reaping(t *testing.T) *Reaper {
	r := New()
	sig := make(chan os.Signal, 16)
	signal.Notify(sig, unix.SIGCHLD)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-sig:
				r.Reap()
			case <-done:
				return
			}
		}
	}()
	t.Cleanup(func() { signal.Stop(sig); close(done) })
	return r
}

func TestRunReturnsOutputAndStatus(t *testing.T) {
	r := reaping(t)
	out, err := r.Run(10*time.Second, "/bin/sh", "-c", "echo out; echo err >&2")
	if err != nil || string(out) != "out\nerr\n" {
		t.Fatalf("out %q err %v", out, err)
	}
	out, err = r.Run(10*time.Second, "/bin/sh", "-c", "echo no; exit 3")
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || string(out) != "no\n" {
		t.Fatalf("out %q err %v", out, err)
	}
	if _, err := r.Run(10*time.Second, "/bin/sh", "-c", "kill -9 $$"); err == nil || !strings.Contains(err.Error(), "signal 9") {
		t.Fatalf("err %v", err)
	}
	if _, err := r.Run(time.Second, "/no/such/program"); err == nil {
		t.Fatal("a missing program ran")
	}
	errs := make(chan error, 20)
	for i := range 20 {
		go func() {
			_, err := r.Run(10*time.Second, "/bin/sh", "-c", "exit "+string(rune('0'+i%5)))
			if (i%5 == 0) != (err == nil) {
				errs <- err
				return
			}
			errs <- nil
		}()
	}
	for range 20 {
		if err := <-errs; err != nil {
			t.Fatalf("wrong status: %v", err)
		}
	}
}

func TestRunKillsAProgramThatHangs(t *testing.T) {
	r := reaping(t)
	start := time.Now()
	_, err := r.Run(200*time.Millisecond, "/bin/sh", "-c", "exec /bin/sleep 60")
	if err == nil || !strings.Contains(err.Error(), "killed after") {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

func TestReapCollectsOrphans(t *testing.T) {
	r := New()
	const children = 5
	for range children {
		p, err := os.StartProcess("/proc/self/exe", []string{"test", "-test.run=^$"}, &os.ProcAttr{})
		if err != nil {
			t.Fatal(err)
		}
		p.Release()
	}
	reaped := 0
	deadline := time.Now().Add(10 * time.Second)
	for reaped < children {
		reaped += r.Reap()
		if time.Now().After(deadline) {
			t.Fatalf("reaped %d of %d children", reaped, children)
		}
		time.Sleep(time.Millisecond)
	}
	if extra := r.Reap(); extra != 0 {
		t.Fatalf("reaped %d children that were never started", extra)
	}
}
