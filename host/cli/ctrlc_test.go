package cli

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const ctrlCHelperEnv = "FEV_TEST_CTRLC"

func ctrlCHelper() int {
	if os.Getenv(ctrlCHelperEnv) == "forward" {
		foreground = func() bool { return false }
	} else if !foreground() {
		os.Stderr.WriteString("helper: not in the terminal's foreground group\n")
		return 3
	}
	return Entry(Env{Name: "docker", Args: []string{"run", "--rm", "-p", "8080:80", "nginx"}, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
}

const counterC = `#include <errno.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <time.h>

static volatile sig_atomic_t n;

static void on_int(int sig) {
	struct timespec t = {0, 300 * 1000 * 1000};
	(void)sig;
	n++;
	while (nanosleep(&t, &t) == -1 && errno == EINTR) {
	}
}

int main(void) {
	struct sigaction sa;
	sigset_t block, old;
	struct timespec t = {1, 0};
	FILE *f;
	sa.sa_handler = on_int;
	sigemptyset(&sa.sa_mask);
	sa.sa_flags = 0;
	sigemptyset(&block);
	sigaddset(&block, SIGINT);
	if (sigaction(SIGINT, &sa, 0) != 0 || sigprocmask(SIG_BLOCK, &block, &old) != 0) {
		return 2;
	}
	if (!(f = fopen(getenv("CTRLC_READY"), "w"))) {
		return 2;
	}
	fclose(f);
	while (n == 0) {
		sigsuspend(&old);
	}
	sigprocmask(SIG_SETMASK, &old, 0);
	while (nanosleep(&t, &t) == -1 && errno == EINTR) {
	}
	if (!(f = fopen(getenv("CTRLC_COUNT"), "w"))) {
		return 2;
	}
	fprintf(f, "%d\n", (int)n);
	fclose(f);
	return 0;
}
`

func openPty(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	fd := master.Fd()
	var name [128]byte
	for _, req := range []struct {
		op  uintptr
		arg uintptr
	}{{unix.TIOCPTYGRANT, 0}, {unix.TIOCPTYUNLK, 0}, {unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))}} {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req.op, req.arg); errno != 0 {
			t.Fatalf("ioctl %#x on the terminal: %v", req.op, errno)
		}
	}
	path := string(name[:bytes.IndexByte(name[:], 0)])
	slave, err = os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	return master, slave
}

func typeCtrlC(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	ready, count := filepath.Join(dir, "ready"), filepath.Join(dir, "count")
	master, slave := openPty(t)
	helper := exec.Command(os.Args[0])
	helper.Env = append(os.Environ(), ctrlCHelperEnv+"="+mode, "CTRLC_READY="+ready, "CTRLC_COUNT="+count)
	helper.Stdin, helper.Stdout, helper.Stderr = slave, slave, slave
	helper.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()
	var out bytes.Buffer
	var copied sync.WaitGroup
	copied.Add(1)
	go func() { defer copied.Done(); io.Copy(&out, master) }()
	done := make(chan error, 1)
	go func() { done <- helper.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-done:
			master.Close()
			copied.Wait()
			t.Fatalf("%s: the entry point ended before docker was ready: %v\n%s", mode, err, out.String())
		default:
		}
		if time.Now().After(deadline) {
			helper.Process.Kill()
			t.Fatalf("%s: docker was never ready", mode)
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := master.Write([]byte{0x03}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			master.Close()
			copied.Wait()
			t.Fatalf("%s: the entry point: %v\n%s", mode, err, out.String())
		}
	case <-time.After(10 * time.Second):
		helper.Process.Kill()
		t.Fatalf("%s: docker did not end after the interrupt", mode)
	}
	data, err := os.ReadFile(count)
	if err != nil {
		t.Fatalf("%s: docker counted nothing: %v", mode, err)
	}
	return strings.TrimSpace(string(data))
}

func TestATypedCtrlCReachesDockerOnce(t *testing.T) {
	c := newEntryCase(t)
	newFakeIn(t, c.home)
	src := filepath.Join(t.TempDir(), "counter.c")
	os.WriteFile(src, []byte(counterC), 0o644)
	if out, err := exec.Command("/usr/bin/cc", "-o", filepath.Join(c.tools, "docker"), src).CombinedOutput(); err != nil {
		t.Fatalf("cc (which fevd's own build needs too): %v\n%s", err, out)
	}
	if got := typeCtrlC(t, "built"); got != "1" {
		t.Errorf("a typed Ctrl-C reached docker %s times through the entry point, want once", got)
	}
	if got := typeCtrlC(t, "forward"); got != "2" {
		t.Errorf("with every interrupt passed on, docker counted %s; the counter cannot tell two deliveries from one", got)
	}
}
