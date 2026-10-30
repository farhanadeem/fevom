package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"
	"fev/proto/relay"
)

func TestMain(m *testing.M) {
	if IsEntry(os.Args) {
		EntryMain()
	}
	os.Exit(m.Run())
}

func framed(t *testing.T, m *Manager, p api.ExecStartParams, input string) (string, string, api.ExecExit) {
	t.Helper()
	exits := make(chan api.ExecExit, 1)
	started, err := m.Start(p, func(e api.ExecExit) { exits <- e })
	if err != nil {
		t.Fatal(err)
	}
	host, err := attachHost(t, m, started.Token)
	if err != nil {
		t.Fatal(err)
	}
	if input != "" {
		relay.WriteFrame(host, relay.Stdin, []byte(input))
	}
	relay.WriteFrame(host, relay.Stdin, nil)
	got := map[byte]string{}
	buf := make([]byte, relay.MaxFrame)
	for {
		stream, n, err := relay.ReadFrame(host, buf)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			break
		}
		got[stream] += string(buf[:n])
	}
	return got[relay.Stdout], got[relay.Stderr], exitOf(t, exits)
}

func TestASessionHasTheReferencesDescriptorLimit(t *testing.T) {
	m := manager(t)
	out, errOut, e := framed(t, m, api.ExecStartParams{Context: api.ExecInit, Argv: []string{"sh", "-c", "ulimit -n; ulimit -Hn"}, Env: []string{"PATH=/usr/bin:/bin"}}, "")
	if out != "1048576\n1048576\n" || e.ExitCode != 0 {
		t.Fatalf("limits %q (stderr %q), exit %+v", out, errOut, e)
	}
}

func cgroupDir(t *testing.T) string {
	t.Helper()
	var st unix.Statfs_t
	if unix.Statfs("/sys/fs/cgroup", &st) != nil || st.Type != unix.CGROUP2_SUPER_MAGIC {
		t.Skip("needs cgroup v2 at /sys/fs/cgroup")
	}
	dir := filepath.Join("/sys/fs/cgroup", fmt.Sprintf("session-test-%d", os.Getpid()))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Skipf("cannot make a cgroup: %v", err)
	}
	t.Cleanup(func() {
		entries, _ := os.ReadDir(filepath.Join(dir, "sessions"))
		for _, e := range entries {
			unix.Rmdir(filepath.Join(dir, "sessions", e.Name()))
		}
		unix.Rmdir(filepath.Join(dir, "sessions"))
		unix.Rmdir(dir)
	})
	return filepath.Join(dir, "sessions")
}

func TestASessionsLeftoversDieWithItInInitsContext(t *testing.T) {
	dir := cgroupDir(t)
	m := manager(t)
	m.contexts[api.ExecInit] = Groups{Dir: dir}
	pidFile := filepath.Join(t.TempDir(), "pid")
	script := "cat /proc/self/cgroup; nohup sleep 300 >/dev/null 2>&1 & echo $! >" + pidFile
	out, _, e := framed(t, m, api.ExecStartParams{Context: api.ExecInit, Argv: []string{"sh", "-c", script}, Env: []string{"PATH=/usr/bin:/bin"}}, "")
	if e.ExitCode != 0 || !strings.Contains(out, filepath.Base(filepath.Dir(dir))+"/sessions/") {
		t.Fatalf("cgroup %q, exit %+v", out, e)
	}
	b, _ := os.ReadFile(pidFile)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("pid %q", b)
	}
	if unix.Kill(pid, 0) == nil {
		if st, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil && !strings.Contains(string(st), ") Z ") {
			t.Fatalf("the leftover %d still runs: %s", pid, st)
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("a group left: %s", e.Name())
		}
	}
}

type entered struct{ e Entry }

func (entered) In(fn func(cgroup int) error) error { return fn(-1) }
func (c entered) Entry(p api.ExecStartParams) (*Entry, error) {
	e := c.e
	if e.Argv == nil {
		e.Argv = p.Argv
	}
	return &e, nil
}

func TestASessionRunsAsTheUserAndExitsAsAShellWould(t *testing.T) {
	m := manager(t)
	home := t.TempDir()
	os.Chmod(home, 0o755)
	os.Chmod(filepath.Dir(home), 0o755)
	noexec := filepath.Join(home, "noexec")
	os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o644)
	ctx := entered{Entry{
		Find: true, Home: home, Name: "fev",
		Env:  []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "HOME=" + home},
		Cred: &Cred{UID: 65534, GID: 65534, Groups: []uint32{65534, 4}},
	}}
	m.contexts["machine"] = ctx
	p := func(argv ...string) api.ExecStartParams {
		return api.ExecStartParams{Context: "machine", Argv: argv, Env: []string{"LANG=en_GB.UTF-8", "EXTRA=1"}}
	}
	out, _, e := framed(t, m, p("sh", "-c", `id -u; id -g; id -G; echo "$LANG $EXTRA $HOME"; pwd`), "")
	if want := fmt.Sprintf("65534\n65534\n65534 4\nen_GB.UTF-8 1 %s\n%s\n", home, home); out != want || e.ExitCode != 0 {
		t.Errorf("as the user: %q, want %q; exit %+v", out, want, e)
	}
	q := p("pwd")
	q.Dir = "/no/such/dir"
	out, errOut, e := framed(t, m, q, "")
	if out != home+"\n" || !strings.Contains(errOut, "/no/such/dir") || strings.Count(errOut, "\n") != 1 || e.ExitCode != 0 {
		t.Errorf("a directory not there: %q, %q, %+v", out, errOut, e)
	}
	_, errOut, e = framed(t, m, p("nosuchcmd"), "")
	if e.ExitCode != 127 || !strings.Contains(errOut, "nosuchcmd: command not found") {
		t.Errorf("not found: %q, %+v", errOut, e)
	}
	_, errOut, e = framed(t, m, p(noexec), "")
	if e.ExitCode != 126 || !strings.Contains(errOut, "permission denied") {
		t.Errorf("not executable: %q, %+v", errOut, e)
	}
	_, _, e = framed(t, m, p("sh", "-c", "exit 200"), "")
	if e.ExitCode != 200 || e.Signal != 0 {
		t.Errorf("exit 200: %+v", e)
	}
	_, _, e = framed(t, m, p("sh", "-c", "kill -9 $$"), "")
	if e.ExitCode != -1 || e.Signal != 9 {
		t.Errorf("killed: %+v", e)
	}
}

func TestEverySignalReachesTheSession(t *testing.T) {
	m := manager(t)
	for _, sig := range []unix.Signal{unix.SIGHUP, unix.SIGINT, unix.SIGQUIT, unix.SIGTERM, unix.SIGUSR1, unix.SIGUSR2, unix.SIGKILL} {
		exits := make(chan api.ExecExit, 1)
		started, err := m.Start(api.ExecStartParams{Context: api.ExecInit, Argv: []string{"sh", "-c", "echo up; exec sleep 100"}, Env: []string{"PATH=/usr/bin:/bin"}},
			func(e api.ExecExit) { exits <- e })
		if err != nil {
			t.Fatal(err)
		}
		host, err := attachHost(t, m, started.Token)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, relay.MaxFrame)
		if _, _, err := relay.ReadFrame(host, buf); err != nil {
			t.Fatal(err)
		}
		if err := m.Signal(started.ID, int(sig)); err != nil {
			t.Fatal(err)
		}
		if e := exitOf(t, exits); e.Signal != int(sig) || e.ExitCode != -1 {
			t.Errorf("%v: exit %+v", sig, e)
		}
	}
}

func TestStandardInputAloneATerminal(t *testing.T) {
	m := manager(t)
	exits := make(chan api.ExecExit, 1)
	script := `test -t 0 && echo in=tty; test -t 1 || echo out=pipe; test -t 2 || echo err=pipe >&2; stty size; cat; echo done`
	started, err := m.Start(api.ExecStartParams{Context: api.ExecInit, Argv: []string{"sh", "-c", script}, Env: []string{"PATH=/usr/bin:/bin"},
		StdinTTY: true, Rows: 30, Cols: 100}, func(e api.ExecExit) { exits <- e })
	if err != nil {
		t.Fatal(err)
	}
	host, err := attachHost(t, m, started.Token)
	if err != nil {
		t.Fatal(err)
	}
	relay.WriteFrame(host, relay.Stdin, []byte("typed\n"))
	relay.WriteFrame(host, relay.Stdin, nil)
	got := map[byte]string{}
	buf := make([]byte, relay.MaxFrame)
	for {
		stream, n, err := relay.ReadFrame(host, buf)
		if err != nil {
			break
		}
		got[stream] += string(buf[:n])
	}
	if got[relay.Stdout] != "in=tty\nout=pipe\n30 100\ntyped\ndone\n" || got[relay.Stderr] != "err=pipe\n" {
		t.Errorf("stdout %q, stderr %q", got[relay.Stdout], got[relay.Stderr])
	}
	if e := exitOf(t, exits); e.ExitCode != 0 {
		t.Errorf("exit %+v", e)
	}
	_ = bufio.NewReader
	_ = time.Second
}
