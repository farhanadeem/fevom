package debugremote_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"fev/host/debugremote"
	"fev/host/debugremote/fakeengine"
)

func pair(t *testing.T) (*os.File, *os.File) {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	return os.NewFile(uintptr(fds[0]), "a"), os.NewFile(uintptr(fds[1]), "b")
}

func TestRunEchoesAgainstFakeEngine(t *testing.T) {
	sockDir, _ := os.MkdirTemp("/tmp", "bv.")
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "e.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fe := fakeengine.New("aarch64", ln)
	defer fe.Close()
	fe.AddContainer("api", "img:1", true)

	agentDir := t.TempDir()
	d := filepath.Join(agentDir, "arm64")
	os.MkdirAll(d, 0o755)
	for n, b := range map[string]string{
		"fev-debug-agent": "x", "fev-debug-stage": "x", "fev-debug": "x",
		"defaultset.lock": "# set (aarch64-linux)\n/nix/store/a-zsh\n", "stamp": "s\n",
	} {
		os.WriteFile(filepath.Join(d, n), []byte(b), 0o644)
	}

	daemonEnd, appEnd := pair(t)
	resize := make(chan [2]int, 4)
	resize <- [2]int{24, 80}
	o := debugremote.Options{
		Ctx:    debugremote.Context{Host: "unix://" + sock},
		Target: "api", Kind: "container", TTY: true, Stdin: true,
		AgentDir: agentDir, BuildID: "testbuild",
		In: daemonEnd, Out: daemonEnd, Err: daemonEnd, Resize: resize,
	}
	done := make(chan int, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		code, err := o.Run(context.Background())
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- code
	}()
	appEnd.SetDeadline(time.Now().Add(15 * time.Second))
	appEnd.Write([]byte("marco\n"))
	var b strings.Builder
	buf := make([]byte, 4096)
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		appEnd.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := appEnd.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
			if strings.Contains(b.String(), "marco") {
				break
			}
		}
		_ = err
	}
	t.Logf("stream so far: %q", b.String())
	if !strings.Contains(b.String(), "debug shell ready") {
		t.Errorf("no banner")
	}
	if !strings.Contains(b.String(), "marco") {
		t.Errorf("no echo")
	}
	appEnd.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	wg.Wait()
}
