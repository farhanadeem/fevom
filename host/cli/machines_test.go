package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/relay"
)

type sessionRun func(p api.SessionStartParams, stream net.Conn, signals <-chan int) api.SessionExit

func fakeSessions(f *fake, run sessionRun) {
	type started struct {
		conn    *jsonrpc.Conn
		id      uint64
		p       api.SessionStartParams
		signals chan int
	}
	var mu sync.Mutex
	var next uint64
	byToken := map[string]*started{}
	byID := map[uint64]*started{}
	f.on(api.MethodSessionStart, func(c *jsonrpc.Conn, params json.RawMessage) (any, error) {
		p, err := jsonrpc.Bind[api.SessionStartParams](params)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		next++
		s := &started{conn: c, id: next, p: p, signals: make(chan int, 8)}
		token := fmt.Sprintf("%0*x", relay.TokenLen, next)
		byToken[token], byID[s.id] = s, s
		mu.Unlock()
		machine := p.Machine
		if machine == "" {
			machine = "dev"
		}
		return api.SessionStarted{ID: s.id, Token: token, Machine: machine, User: "ada"}, nil
	})
	f.on(api.MethodSessionSignal, func(c *jsonrpc.Conn, params json.RawMessage) (any, error) {
		p, _ := jsonrpc.Bind[api.SessionSignalParams](params)
		mu.Lock()
		s := byID[p.ID]
		mu.Unlock()
		if s != nil {
			s.signals <- p.Signal
		}
		return api.Empty{}, nil
	})
	f.on(api.MethodSessionResize, func(*jsonrpc.Conn, json.RawMessage) (any, error) { return api.Empty{}, nil })
	ln, err := net.Listen("unix", api.SessionSocketPath(f.home))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c *net.UnixConn) {
				defer c.Close()
				req, err := relay.ReadRequest(c)
				if err != nil {
					return
				}
				mu.Lock()
				s := byToken[req.Target]
				delete(byToken, req.Target)
				mu.Unlock()
				if s == nil {
					relay.WriteError(c, relay.CodeDenied, "no such session")
					return
				}
				fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
				if err != nil {
					return
				}
				theirs := os.NewFile(uintptr(fds[0]), "client")
				pf := os.NewFile(uintptr(fds[1]), "process")
				ours, _ := net.FileConn(pf)
				pf.Close()
				c.WriteMsgUnix([]byte("ok\n"), syscall.UnixRights(fds[0]), nil)
				var b [1]byte
				c.Read(b[:])
				theirs.Close()
				e := run(s.p, ours, s.signals)
				ours.Close()
				e.ID = s.id
				s.conn.Notify(api.NotifySessionExit, e)
			}(conn.(*net.UnixConn))
		}
	}()
}

func framedEcho(p api.SessionStartParams, stream net.Conn, _ <-chan int) api.SessionExit {
	line := strings.Join(p.Argv, " ")
	relay.WriteFrame(stream, relay.Stdout, []byte(line+"\n"))
	relay.WriteFrame(stream, relay.Stderr, []byte("to stderr\n"))
	buf := make([]byte, relay.MaxFrame)
	for {
		kind, n, err := relay.ReadFrame(stream, buf)
		if err != nil || kind == relay.Stdin && n == 0 {
			break
		}
		relay.WriteFrame(stream, relay.Stdout, buf[:n])
	}
	code := 0
	if _, after, ok := strings.Cut(line, "exit "); ok {
		fmt.Sscanf(after, "%d", &code)
	}
	switch {
	case strings.Contains(line, "killed"):
		return api.SessionExit{ExitCode: -1, Signal: 9}
	case strings.Contains(line, "unstartable"):
		return api.SessionExit{ExitCode: -1, Error: "the machine dev is not running (failed)"}
	}
	return api.SessionExit{ExitCode: code}
}

func noTerminalEnv(t *testing.T) {
	for _, name := range append([]string{"TERM", FevomEnvVar}, defaultEnvNames...) {
		t.Setenv(name, "")
	}
}

func TestRunIsASessionThroughTheLoginShell(t *testing.T) {
	noTerminalEnv(t)
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	t.Setenv(FevomEnvVar, "FOO:BAR::TERM_PROGRAM")
	t.Setenv("FOO", "1")
	f := newFake(t)
	fakeSessions(f, framedEcho)
	r := fev(t, f.home, "typed\n", "run", "-m", "dev", "-u", "root", "-w", "/srv", "echo", "a b", "$HOME", "it's", "exit", "3")
	if r.code != 3 {
		t.Fatalf("exit %d, want the command's 3; stderr %q", r.code, r.err)
	}
	if r.out != "echo 'a b' '$HOME' 'it'\\''s' exit 3\ntyped\n" || r.err != "to stderr\n" {
		t.Errorf("stdout %q, stderr %q", r.out, r.err)
	}
	if got := f.params(api.MethodSessionStart); got != `{"machine":"dev","user":"root","argv":["echo","'a b'","'$HOME'","'it'\\''s'","exit","3"],"env":["TERM_PROGRAM=Apple_Terminal","FOO=1"],"dir":"/srv","login":true,"shell":true}` {
		t.Errorf("session.start %s", got)
	}

	f2 := newFake(t)
	fakeSessions(f2, framedEcho)
	fev(t, f2.home, "", "run", "-s", "echo", "$HOME", "|", "wc").want(t, 0)
	fev(t, f2.home, "", "run", "if", "x=y").want(t, 0)
	var argvs []string
	for _, c := range f2.made() {
		if c.Method == api.MethodSessionStart {
			var p api.SessionStartParams
			json.Unmarshal([]byte(c.Params), &p)
			argvs = append(argvs, strings.Join(p.Argv, " "))
		}
	}
	if len(argvs) != 2 || argvs[0] != "echo $HOME | wc" || argvs[1] != "'if' 'x=y'" {
		t.Errorf("command lines %q", argvs)
	}

	f3 := newFake(t)
	fakeSessions(f3, framedEcho)
	fev(t, f3.home, "", "run").want(t, 0)
	var p api.SessionStartParams
	json.Unmarshal([]byte(f3.params(api.MethodSessionStart)), &p)
	if !p.Login || p.Shell || len(p.Argv) != 0 || p.Machine != "" || p.TTY || p.StdinTTY {
		t.Errorf("a login shell: %+v", p)
	}

	if r := fev(t, f3.home, "", "run", "killed"); r.code != signalExitCode || r.err != "to stderr\n" {
		t.Errorf("killed: %+v", r)
	}
	if r := fev(t, f3.home, "", "run", "unstartable"); r.code != ExitFailed || !strings.HasSuffix(r.err, "fev: the machine dev is not running (failed)\n") {
		t.Errorf("unstartable: %+v", r)
	}
	f3.on(api.MethodSessionStart, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return nil, jsonrpc.Errorf(api.CodeNoMachine, "there is no machine nosuch")
	})
	if r := fev(t, f3.home, "", "run", "-m", "nosuch", "true"); r.code != ExitFailed || r.err != "fev: there is no machine nosuch\n" {
		t.Errorf("no such machine: %+v", r)
	}
}

func TestTheWorkingDirectoryIsTheMacsAsTheMachineSeesIt(t *testing.T) {
	for mac, linux := range map[string]string{
		"/Users/ada/src/app":      "/Users/ada/src/app",
		"/Users":                  "/Users",
		"/Volumes/External/x":     "/Volumes/External/x",
		"/private/tmp":            "/private/tmp",
		"/Applications/Xcode.app": "/Applications/Xcode.app",
		"/Library/Developer":      "/Library/Developer",
		"/tmp":                    "/mnt/mac/tmp",
		"/opt/homebrew/bin":       "/mnt/mac/opt/homebrew/bin",
		"/":                       "/mnt/mac",
		"/Usersx/y":               "/mnt/mac/Usersx/y",
		"/Users/ada/../ada/./src": "/Users/ada/src",
	} {
		if got := linuxPath(mac); got != linux {
			t.Errorf("%s: %s, want %s", mac, got, linux)
		}
	}
	if len(agent.MachineSharePaths) != 5 {
		t.Errorf("the machines' same-path folders changed: %v", agent.MachineSharePaths)
	}

	noTerminalEnv(t)
	f := newFake(t)
	fakeSessions(f, framedEcho)
	t.Chdir("/usr/bin")
	fev(t, f.home, "", "run", "pwd").want(t, 0)
	var p api.SessionStartParams
	json.Unmarshal([]byte(f.params(api.MethodSessionStart)), &p)
	if p.Dir != "/mnt/mac/usr/bin" {
		t.Errorf("from /usr/bin the session starts in %q", p.Dir)
	}
}

func TestASessionsEnvironmentFromTheMac(t *testing.T) {
	env := map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor", "TERM_PROGRAM": "ghostty", "TERM_PROGRAM_VERSION": "1.2", "LANG": "en_GB.UTF-8", "TZ": "Europe/London", "EDITOR": "vim", FevomEnvVar: "EDITOR:NOPE:A=B:"}
	get := func(name string) string { return env[name] }
	if got := strings.Join(sessionEnv(get, true), " "); got != "TERM=xterm-256color COLORTERM=truecolor TERM_PROGRAM=ghostty TERM_PROGRAM_VERSION=1.2 EDITOR=vim" {
		t.Errorf("with a terminal: %s", got)
	}
	if got := strings.Join(sessionEnv(get, false), " "); got != "COLORTERM=truecolor TERM_PROGRAM=ghostty TERM_PROGRAM_VERSION=1.2 EDITOR=vim" {
		t.Errorf("without: %s", got)
	}
}

func TestPathTranslationTakesOnlyCleanMacPaths(t *testing.T) {
	exists := map[string]bool{"/etc/hosts": true, "/dev/null": true, "/etc/passwd": true, "/opt/homebrew/etc/x.conf": true, "/usr": true, "/usr/include": true, "/a/b": true, "/opt/homebrew": true}
	lstat := func(p string) (os.FileInfo, error) {
		if exists[p] {
			return nil, nil
		}
		return nil, os.ErrNotExist
	}
	expr := func(p string) string {
		return `"$(if [ -e ` + p + ` ] || [ -h ` + p + ` ]; then printf %s ` + p + `; else printf %s /mnt/mac` + p + `; fi)"`
	}
	in := []string{"/opt/homebrew/bin/x", "/opt/homebrew/etc/x.conf", "--prefix=/usr", "/dev/null", "-I/usr/include",
		"https://example.com/a", "host:/a/b", "s/a/b/", "/^x$/", "//net/x", "/a/../b", "/a/b/", "/opt/*", "$HOME/x",
		"/Users/ada/f", "/mnt/mac/x", "/nonexistent", "--", "/etc/hosts"}
	out := translateArgs(in, lstat)
	want := map[int]string{1: expr("/opt/homebrew/etc/x.conf"), 2: `'--prefix='` + expr("/usr"), 3: expr("/dev/null"), 11: expr("/a/b/")}
	for i := range in {
		w, translated := want[i]
		got := out[i]
		if translated {
			if got != "\x00fev-p\x00"+w {
				t.Errorf("%q: %q, want %q", in[i], got, w)
			}
		} else if got != in[i] {
			t.Errorf("%q was translated: %q", in[i], got)
		}
	}
	line := strings.Join(commandLine(translateArgs([]string{"cat", "/etc/hosts", "a b"}, lstat), false), " ")
	if line != "cat "+expr("/etc/hosts")+" 'a b'" {
		t.Errorf("line %s", line)
	}
}

func TestTheFallThroughIsRunForAWordThatIsNoCommand(t *testing.T) {
	commands := map[string]bool{"list": true, "run": true, "help": true}
	for _, c := range []struct{ in, want string }{
		{"", "run"},
		{"ls -la", "run ls -la"},
		{"--home /h -m dev -w /x ls", "run --home /h -m dev -w /x ls"},
		{"-mdev --user=root -ps uname -a", "run -mdev --user=root -ps uname -a"},
		{"-q", "run -q"},
		{"-- -x", "run -- -x"},
		{"list", "list"},
		{"--home /h list -q", "--home /h list -q"},
		{"--help", "--help"},
		{"-h", "-h"},
		{"--nope ls", "--nope ls"},
		{"help run", "help run"},
	} {
		got := strings.Join(fallThrough(strings.Fields(c.in), commands), " ")
		if got != c.want {
			t.Errorf("%q: %q, want %q", c.in, got, c.want)
		}
	}

	noTerminalEnv(t)
	f := newFake(t)
	fakeSessions(f, framedEcho)
	if r := fev(t, f.home, "", "uname", "-a").want(t, 0); r.out != "uname -a\n" {
		t.Errorf("fev uname -a: %q", r.out)
	}
	var p api.SessionStartParams
	json.Unmarshal([]byte(f.params(api.MethodSessionStart)), &p)
	if strings.Join(p.Argv, " ") != "uname -a" || !p.Login || !p.Shell {
		t.Errorf("fev uname -a: %+v", p)
	}
	var out, errw lockedBuffer
	if code := Main(Env{Name: "fevctl", Args: []string{"--home", f.home, "uname"}, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errw}); code != ExitUsage {
		t.Errorf("fevctl uname: exit %d", code)
	}
}

func TestATerminalIsDecidedPerStream(t *testing.T) {
	noTerminalEnv(t)
	t.Setenv("TERM", "xterm-256color")
	master, slave := openPty(t)
	defer master.Close()
	defer slave.Close()
	unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 40, Col: 132})
	f := newFake(t)
	fakeSessions(f, func(p api.SessionStartParams, stream net.Conn, _ <-chan int) api.SessionExit {
		stream.Write([]byte("raw output\r\n"))
		return api.SessionExit{ExitCode: 0}
	})
	read := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		n, _ := master.Read(buf)
		read <- string(buf[:n])
	}()
	before, _ := unix.IoctlGetTermios(int(slave.Fd()), unix.TIOCGETA)
	code := Main(Env{Name: "fev", Args: []string{"--home", f.home, "run", "top"}, Stdin: slave, Stdout: slave, Stderr: slave})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := <-read; got != "raw output\r\n" {
		t.Errorf("the terminal got %q", got)
	}
	after, _ := unix.IoctlGetTermios(int(slave.Fd()), unix.TIOCGETA)
	if before.Lflag != after.Lflag || before.Iflag != after.Iflag || before.Oflag != after.Oflag {
		t.Error("the terminal was not put back as it was")
	}
	var p api.SessionStartParams
	json.Unmarshal([]byte(f.params(api.MethodSessionStart)), &p)
	if !p.TTY || p.StdinTTY || p.Rows != 40 || p.Cols != 132 || len(p.Env) != 1 || p.Env[0] != "TERM=xterm-256color" {
		t.Errorf("with a terminal: %+v", p)
	}

	f2 := newFake(t)
	got := make(chan int, 1)
	fakeSessions(f2, func(p api.SessionStartParams, stream net.Conn, signals <-chan int) api.SessionExit {
		syscall.Kill(os.Getpid(), syscall.SIGUSR1)
		select {
		case n := <-signals:
			got <- n
		case <-time.After(5 * time.Second):
			got <- 0
		}
		return api.SessionExit{ExitCode: -1, Signal: 10}
	})
	var out lockedBuffer
	code = Main(Env{Name: "fev", Args: []string{"--home", f2.home, "run", "cat"}, Stdin: slave, Stdout: &out, Stderr: &out})
	if code != signalExitCode {
		t.Errorf("exit %d, want %d", code, signalExitCode)
	}
	if n := <-got; n != 10 {
		t.Errorf("the session got signal %d, want 10", n)
	}
	p = api.SessionStartParams{}
	json.Unmarshal([]byte(f2.params(api.MethodSessionStart)), &p)
	if p.TTY || !p.StdinTTY || p.Rows != 40 {
		t.Errorf("with standard input alone a terminal: %+v", p)
	}
}

func TestListAndInfo(t *testing.T) {
	f := newFake(t)
	r := f.run("list").want(t, 0)
	golden(t, "list", r.out)
	sameJSON(t, f.run("list", "-f", "json").out, fixtureResult(t, "machines.list"))
	sameJSON(t, f.run("list", "--format", "json").out, fixtureResult(t, "machines.list"))
	if r := f.run("list", "-q").want(t, 0); r.out != "dev\nf43\nold\n" {
		t.Errorf("list -q: %q", r.out)
	}
	if r := f.run("list", "-rq").want(t, 0); r.out != "dev\n" {
		t.Errorf("list -rq: %q", r.out)
	}
	r = f.run("info", "dev").want(t, 0)
	golden(t, "info", r.out)
	sameJSON(t, f.run("info", "-f", "json", "dev").out, fixtureResult(t, "machines.info"))
	var info map[string]any
	if err := json.Unmarshal([]byte(f.run("info", "-f", "json", "dev").out), &info); err != nil {
		t.Fatal(err)
	}
	if _, ok := info["disk_size"]; !ok {
		t.Errorf("info -f json has no disk_size: %v", info)
	}
	if _, ok := info["disk_bytes"]; ok {
		t.Errorf("info -f json still says disk_bytes: %v", info)
	}
	f.run("info")
	if got := f.made(); got[len(got)-1].Params != `{"name":"dev"}` {
		t.Errorf("info with no name asked %v", got[len(got)-1])
	}
	f.on(api.MethodMachinesInfo, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return nil, jsonrpc.Errorf(api.CodeNoMachine, "there is no machine nosuch")
	})
	if r := f.run("info", "nosuch"); r.code != ExitFailed || r.err != "fev: there is no machine nosuch\n" {
		t.Errorf("info nosuch: %+v", r)
	}
	for _, c := range f.made() {
		if c.Method == api.MethodVMStart {
			t.Error("list or info started the VM")
		}
	}
}

func TestCreate(t *testing.T) {
	f := newFake(t)
	end := func(r api.MachineCreated) api.Operation {
		b, _ := json.Marshal(r)
		return api.Operation{State: api.OperationDone, Phase: "boot", Result: b}
	}
	m := api.Machine{Name: "dev", State: api.MachineRunning, Image: api.Image{Distro: "ubuntu", Release: "noble", Arch: "arm64", Variant: "cloud", Serial: "20261014_07:42"}, User: "ada", Default: true, Address: "198.19.0.10", DiskBytes: 1 << 30}
	operation(f, api.MethodMachinesCreate, end(api.MachineCreated{Machine: m}))
	data := filepath.Join(t.TempDir(), "user-data")
	os.WriteFile(data, []byte("#cloud-config\nruncmd: [true]\n"), 0o644)
	r := fev(t, f.home, "s3cret\n", "create", "-u", "dev", "-p", "-c", data, "ubuntu:noble", "dev")
	r.want(t, 0)
	golden(t, "create", r.out)
	var p api.MachinesCreateParams
	json.Unmarshal([]byte(f.params(api.MethodMachinesCreate)), &p)
	if p.Name != "dev" || p.Distro != "ubuntu" || p.Release != "noble" || p.User != "dev" || p.UserData != "#cloud-config\nruncmd: [true]\n" || p.Arch != "" {
		t.Errorf("create %+v", p)
	}
	if !strings.HasPrefix(p.PasswordHash, "$6$") || strings.Contains(p.PasswordHash, "s3cret") || sha512Crypt("s3cret", p.PasswordHash[3:19]) != p.PasswordHash {
		t.Errorf("the password went as %q", p.PasswordHash)
	}

	f2 := newFake(t)
	operation(f2, api.MethodMachinesCreate, end(api.MachineCreated{Machine: m}))
	f2.run("create", "--arch", "arm64", "arch").want(t, 0)
	if got := f2.params(api.MethodMachinesCreate); got != `{"name":"arch","distro":"archlinux"}` {
		t.Errorf("create arch: %s", got)
	}
	for _, a := range []string{"amd64", "x86_64", "AMD64"} {
		fa := newFake(t)
		operation(fa, api.MethodMachinesCreate, end(api.MachineCreated{Machine: m}))
		fa.run("create", "-a", a, "ubuntu", "u64").want(t, 0)
		if got := fa.params(api.MethodMachinesCreate); got != `{"name":"u64","distro":"ubuntu","arch":"amd64"}` {
			t.Errorf("create -a %s: %s", a, got)
		}
	}
	fr := newFake(t)
	if r := fr.run("create", "--arch", "riscv64", "ubuntu"); r.code != ExitUsage || !strings.Contains(r.err, `no architecture "riscv64"`) || fr.called(api.MethodMachinesCreate) {
		t.Errorf("create --arch riscv64: %+v", r)
	}
	for _, refused := range [][]string{{"--memory", "4G"}, {"--cpus", "2"}, {"--disk", "10G"}} {
		r := f2.run(append(append([]string{"create"}, refused...), "ubuntu")...)
		if r.code != ExitUsage || !strings.Contains(r.err, "not built yet") {
			t.Errorf("create %v: %+v", refused, r)
		}
	}
	for _, refused := range [][]string{{"--isolate-network"}, {"--forward-ssh-agent"}, {"--mount", "/Users"}} {
		r := f2.run(append(append([]string{"create"}, refused...), "ubuntu")...)
		if r.code != ExitUsage || !strings.Contains(r.err, "add --isolated") {
			t.Errorf("create %v: %+v", refused, r)
		}
	}
	fi := newFake(t)
	operation(fi, api.MethodMachinesCreate, end(api.MachineCreated{Machine: m}))
	fi.run("create", "--isolated", "--isolate-network", "--forward-ssh-agent", "--mount", "/Users/me/src:/src", "--mount", "rel", "ubuntu", "iso").want(t, 0)
	var ip api.MachinesCreateParams
	json.Unmarshal([]byte(fi.params(api.MethodMachinesCreate)), &ip)
	cwd, _ := os.Getwd()
	if !ip.Isolated || !ip.IsolateNetwork || !ip.ForwardSSHAgent || len(ip.Mounts) != 2 || ip.Mounts[0] != "/Users/me/src:/src" || ip.Mounts[1] != filepath.Join(cwd, "rel") {
		t.Errorf("create --isolated: %+v", ip)
	}
	fe := newFake(t)
	operation(fe, api.MethodMachinesCreate, end(api.MachineCreated{Machine: m}))
	fe.run("create", "--emulation", "fex", "--arch", "amd64", "ubuntu", "compat").want(t, 0)
	var ep api.MachinesCreateParams
	json.Unmarshal([]byte(fe.params(api.MethodMachinesCreate)), &ep)
	if ep.Emulation != "fex" {
		t.Fatalf("override lost: %+v", ep)
	}
	fe.run("create", "--emulation", "typo", "ubuntu").want(t, ExitUsage)
	if r := fe.run("create", "--emulation", "rosetta", "ubuntu", "native").want(t, ExitUsage); !strings.Contains(r.err, "amd64 machines") {
		t.Errorf("create --emulation for arm64: %q", r.err)
	}
	fe.run("create", "--emulation", "auto", "ubuntu", "native").want(t, 0)

	f3 := newFake(t)
	operation(f3, api.MethodMachinesCreate, end(api.MachineCreated{Machine: m, CloudInit: &api.CloudInit{Status: "error",
		Errors:     []string{"('scripts_user', RuntimeError('Runparts: 1 failures (runcmd) in 1 attempted commands'))"},
		OutputTail: []string{"+ false", "2026-10-15 09:12:41,203 - cc_scripts_user.py[WARNING]: Failed to run module scripts_user"}}}))
	r = f3.run("create", "-c", data, "ubuntu", "dev").want(t, ExitFailed)
	golden(t, "create.cloud-init-error", r.err)
	if !strings.Contains(r.out, "state: running") {
		t.Errorf("stdout %q", r.out)
	}

	f4 := newFake(t)
	degraded := &api.CloudInit{Status: "done", RecoverableErrors: []string{
		"ERROR: Invalid doas rule 'permit nopass alpine' for user 'ada', not writing any doas rules for user!",
		"WARNING: Failed generating key type rsa to file /etc/ssh/ssh_host_rsa_key"}}
	operation(f4, api.MethodMachinesCreate, end(api.MachineCreated{Machine: m, CloudInit: degraded}))
	r = f4.run("create", "-c", data, "alpine", "dev").want(t, 0)
	golden(t, "create.cloud-init-warning", r.err)
	if !strings.Contains(r.out, "state: running") {
		t.Errorf("stdout %q", r.out)
	}
	f5 := newFake(t)
	operation(f5, api.MethodMachinesCreate, end(api.MachineCreated{Machine: m, CloudInit: degraded}))
	r = f5.run("--format", "json", "create", "-c", data, "alpine", "dev").want(t, 0)
	if strings.Contains(r.err, "warning") || !strings.Contains(r.out, `"recoverable_errors"`) {
		t.Errorf("--format json: stdout %q stderr %q", r.out, r.err)
	}
	f6 := newFake(t)
	operation(f6, api.MethodMachinesCreate, end(api.MachineCreated{Machine: m, CloudInit: &api.CloudInit{Status: "done"}}))
	if r = f6.run("create", "-c", data, "ubuntu", "dev").want(t, 0); strings.Contains(r.err, "cloud-init") {
		t.Errorf("a clean run warned: %q", r.err)
	}
}

func operation(f *fake, method string, end api.Operation) {
	f.on(method, func(c *jsonrpc.Conn, params json.RawMessage) (any, error) {
		op := api.Operation{ID: "op-1", Method: method, Target: "dev", State: api.OperationRunning, Started: "2026-10-15T09:00:00Z"}
		end.ID, end.Method, end.Target, end.Started, end.Ended = op.ID, op.Method, op.Target, op.Started, "2026-10-15T09:00:09Z"
		go func() {
			moving := op
			moving.Phase = "download"
			c.Notify(api.NotifyOperationProgress, api.OperationProgress{Subscription: 2, Operation: moving})
			c.Notify(api.NotifyOperationProgress, api.OperationProgress{Subscription: 2, Operation: end})
		}()
		return op, nil
	})
}

func TestMachineCommandsAreTheirCalls(t *testing.T) {
	f := newFake(t)
	for _, c := range []struct {
		argv   []string
		method string
		params string
	}{
		{[]string{"start", "dev"}, api.MethodMachinesStart, `{"name":"dev"}`},
		{[]string{"stop", "dev", "-f", "--timeout", "3"}, api.MethodMachinesStop, `{"name":"dev","grace_ms":3000,"force":true}`},
		{[]string{"restart", "dev"}, api.MethodMachinesRestart, `{"name":"dev"}`},
		{[]string{"delete", "-f", "dev"}, api.MethodMachinesDelete, `{"name":"dev"}`},
		{[]string{"clone", "dev", "dev2"}, api.MethodMachinesClone, `{"name":"dev","new_name":"dev2"}`},
		{[]string{"rename", "dev", "work"}, api.MethodMachinesRename, `{"name":"dev","new_name":"work"}`},
		{[]string{"default", "f43"}, api.MethodMachinesSetDefault, `{"name":"f43"}`},
	} {
		f.run(c.argv...).want(t, 0)
		if got := f.params(c.method); got != c.params {
			t.Errorf("%v: %s %s, want %s", c.argv, c.method, got, c.params)
		}
	}
	if r := f.run("default").want(t, 0); r.out != "dev\n" {
		t.Errorf("default: %q", r.out)
	}
	if _, err := os.Stat(api.StopMarkPath(f.home)); err == nil {
		t.Error("stop MACHINE left the mark that stops fev")
	}
	for _, c := range f.made() {
		if c.Method == api.MethodVMStop || c.Method == api.MethodShutdown {
			t.Errorf("a machine command asked for %s", c.Method)
		}
	}
	f2 := newFake(t)
	f2.run("stop", "-a").want(t, 0)
	f2.run("start", "-a").want(t, 0)
	var stopped, started []string
	for _, c := range f2.made() {
		var n api.MachineName
		json.Unmarshal([]byte(c.Params), &n)
		switch c.Method {
		case api.MethodMachinesStop:
			stopped = append(stopped, n.Name)
		case api.MethodMachinesStart:
			started = append(started, n.Name)
		}
	}
	if strings.Join(stopped, " ") != "dev old" || strings.Join(started, " ") != "f43 old" {
		t.Errorf("stop -a %v, start -a %v", stopped, started)
	}
	if r := f2.run("delete", "dev"); r.code != ExitUsage || !strings.Contains(r.err, "-f") {
		t.Errorf("delete with no terminal and no -f: %+v", r)
	}
	if f2.called(api.MethodMachinesDelete) {
		t.Error("deleted without asking")
	}
	if r := f2.run("delete", "-a", "dev"); r.code != ExitUsage {
		t.Errorf("delete -a NAME: %+v", r)
	}

	f3 := newFake(t)
	exported, _ := json.Marshal(api.MachineExported{Path: "/tmp/dev.tar.zst", Bytes: 8 << 20})
	operation(f3, api.MethodMachinesExport, api.Operation{State: api.OperationDone, Result: exported})
	r := f3.run("export", "dev", "rel.tar.zst").want(t, 0)
	wd, _ := os.Getwd()
	if got := f3.params(api.MethodMachinesExport); got != `{"name":"dev","path":"`+filepath.Join(wd, "rel.tar.zst")+`"}` {
		t.Errorf("export %s", got)
	}
	if r.out != "archive: /tmp/dev.tar.zst\nsize: 8.0 MiB\n" {
		t.Errorf("export: %q", r.out)
	}
	archive := filepath.Join(t.TempDir(), "dev.tar.zst")
	os.WriteFile(archive, []byte("x"), 0o644)
	imported, _ := json.Marshal(api.Machine{Name: "dev2", State: api.MachineStopped, User: "ada"})
	operation(f3, api.MethodMachinesImport, api.Operation{State: api.OperationDone, Result: imported})
	f3.run("import", "-n", "dev2", archive).want(t, 0)
	if got := f3.params(api.MethodMachinesImport); got != `{"path":"`+archive+`","name":"dev2"}` {
		t.Errorf("import %s", got)
	}
	operation(f3, api.MethodMachinesExport, api.Operation{State: api.OperationFailed, Error: "/tmp/dev.tar.zst exists"})
	if r := f3.run("export", "dev", "/tmp/dev.tar.zst"); r.code != ExitFailed || !strings.HasSuffix(r.err, "\nfev: export dev: /tmp/dev.tar.zst exists\n") {
		t.Errorf("a failed export: %+v", r)
	}
}

func TestMachineLogsPrintTheBacklogAndEnd(t *testing.T) {
	noTerminalEnv(t)
	f := newFake(t)
	var subscribed []string
	var mu sync.Mutex
	f.on(api.MethodLogSubscribe, func(c *jsonrpc.Conn, params json.RawMessage) (any, error) {
		p, _ := jsonrpc.Bind[api.LogSubscribeParams](params)
		mu.Lock()
		subscribed = append(subscribed, p.Sources...)
		id := uint64(len(subscribed) + 10)
		mu.Unlock()
		return api.Subscription{Subscription: id}, nil
	})
	f.then(api.MethodLogSubscribe, func(c *jsonrpc.Conn, params json.RawMessage) {
		p, _ := jsonrpc.Bind[api.LogSubscribeParams](params)
		mu.Lock()
		id := uint64(len(subscribed) + 10)
		mu.Unlock()
		for i := 1; i <= 2; i++ {
			c.Notify(api.NotifyLogEntry, api.LogEntry{Subscription: id, Source: p.Sources[0], Time: "2026-10-15T09:00:00Z", Text: fmt.Sprintf("%s line %d", p.Sources[0], i)})
		}
		c.Notify(api.NotifyLogBacklogEnd, api.Subscription{Subscription: id})
		c.Notify(api.NotifyLogEntry, api.LogEntry{Subscription: id, Source: p.Sources[0], Time: "2026-10-15T09:00:01Z", Text: "new"})
	})
	fakeSessions(f, func(p api.SessionStartParams, stream net.Conn, _ <-chan int) api.SessionExit {
		relay.WriteFrame(stream, relay.Stdout, []byte("journal of "+p.User+"\n"))
		return api.SessionExit{}
	})
	r := f.run("logs", "dev").want(t, 0)
	if r.out != "machine:dev line 1\nmachine:dev line 2\n" {
		t.Errorf("logs dev: %q", r.out)
	}
	r = f.run("logs", "-a", "dev").want(t, 0)
	if r.out != "machine:dev:runtime line 1\nmachine:dev:runtime line 2\nmachine:dev line 1\nmachine:dev line 2\njournal of root\n" {
		t.Errorf("logs -a dev: %q", r.out)
	}
	var p api.SessionStartParams
	json.Unmarshal([]byte(f.params(api.MethodSessionStart)), &p)
	if p.Machine != "dev" || p.User != "root" || !strings.Contains(strings.Join(p.Argv, " "), "journalctl -b --no-pager") {
		t.Errorf("the journal's session: %+v", p)
	}
	f.on(api.MethodMachinesInfo, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return api.Machine{Name: "f43", State: api.MachineStopped}, nil
	})
	f.run("logs", "-a", "f43").want(t, 0)
	if n := strings.Count(fmt.Sprint(f.made()), api.MethodSessionStart); n != 1 {
		t.Errorf("%d sessions for the journal, want 1 (dev's)", n)
	}
	mu.Lock()
	if strings.Join(subscribed, " ") != "machine:dev machine:dev:runtime machine:dev machine:f43:runtime machine:f43" {
		t.Errorf("subscribed %q", subscribed)
	}
	mu.Unlock()
	f.on(api.MethodMachinesInfo, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return nil, jsonrpc.Errorf(api.CodeNoMachine, "there is no machine nosuch")
	})
	if r := f.run("logs", "nosuch"); r.code != ExitFailed {
		t.Errorf("logs nosuch: %+v", r)
	}
	for _, argv := range [][]string{{"logs", "-a"}, {"logs", "--files", "dev"}, {"logs", "--source", "init", "dev"}, {"logs", "dev", "f43"}} {
		if r := f.run(argv...); r.code != ExitUsage {
			t.Errorf("%v: %+v", argv, r)
		}
	}
}

func TestPushAndPullCopyInTheMachine(t *testing.T) {
	noTerminalEnv(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644)
	t.Chdir("/usr/bin")
	f := newFake(t)
	fakeSessions(f, func(api.SessionStartParams, net.Conn, <-chan int) api.SessionExit { return api.SessionExit{} })
	lines := func() []string {
		var out []string
		for _, c := range f.made() {
			if c.Method == api.MethodSessionStart {
				var p api.SessionStartParams
				json.Unmarshal([]byte(c.Params), &p)
				if !p.Login || !p.Shell || p.User != "" || len(p.Argv) != 1 {
					t.Errorf("a copy's session: %+v", p)
				}
				out = append(out, p.Machine+": "+p.Argv[0])
			}
		}
		return out
	}
	src := filepath.Join(dir, "f")
	f.run("push", src).want(t, 0)
	f.run("push", "-m", "f43", src, "/srv/").want(t, 0)
	home, _ := os.UserHomeDir()
	f.run("pull", "~/f").want(t, 0)
	f.run("pull", "-m", "f43", home+"/d", "/etc/os-release", dir).want(t, 0)
	want := []string{
		": cp -R -- " + linuxPath(src) + ` "$HOME"/`,
		"f43: cp -R -- " + linuxPath(src) + " /srv/",
		`: cp -R -- "$HOME"/f /mnt/mac/usr/bin`,
		`f43: cp -R -- "$HOME"/d /etc/os-release ` + linuxPath(dir),
	}
	if got := lines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("copies:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r := f.run("push", filepath.Join(dir, "nonexistent")); r.code != ExitFailed {
		t.Errorf("push of nothing: %+v", r)
	}
	f.on(api.MethodConfigGet, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return api.ConfigValues{Values: map[string]api.ConfigValue{api.KeyFilesShare: {Value: false}}}, nil
	})
	if r := f.run("push", src); r.code != ExitFailed || !strings.Contains(r.err, "file sharing is off") {
		t.Errorf("push with sharing off: %+v", r)
	}
}
