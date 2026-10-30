package sshfront

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	sshagent "golang.org/x/crypto/ssh/agent"
)

type rig struct {
	t       *testing.T
	dir     string
	files   *Files
	srv     *Server
	b       *fakeBackend
	port    int
	cfg     string
	userDir string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no OpenSSH client")
	}
	dir, err := os.MkdirTemp("/tmp", "bsf.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	r := &rig{t: t, dir: dir, files: &Files{Dir: filepath.Join(dir, "ssh")}, b: newFakeBackend(filepath.Join(dir, "machines"))}
	if err := r.files.Ensure(); err != nil {
		t.Fatal(err)
	}
	r.srv = &Server{Files: r.files, Backend: r.b, Version: "test-1"}
	if err := r.srv.Start(); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close(); r.srv.Close() })
	go r.srv.Serve(l)
	r.port = l.Addr().(*net.TCPAddr).Port
	if err := r.files.WriteClient(Client{Port: r.port, ProxyCommand: "false"}); err != nil {
		t.Fatal(err)
	}
	r.cfg = filepath.Join(dir, "client.cfg")
	r.userDir = filepath.Join(dir, "user")
	os.MkdirAll(filepath.Join(r.userDir, ".ssh"), 0o700)
	os.WriteFile(r.cfg, []byte(fmt.Sprintf(`Host fev
  HostName 127.0.0.1
  Port %d
  User default
  IdentityFile %s
  IdentitiesOnly yes
  UserKnownHostsFile %s
  StrictHostKeyChecking yes
  BatchMode yes
  ConnectTimeout 10
`, r.port, r.files.IdentityPath(), r.files.KnownHostsPath())), 0o600)
	return r
}

func (r *rig) cmd(prog string, args ...string) *exec.Cmd {
	c := exec.Command(prog, append([]string{"-F", r.cfg}, args...)...)
	c.Env = []string{"HOME=" + r.userDir, "PATH=/usr/bin:/bin", "LANG=en_US.UTF-8", "COLORTERM=truecolor", "TERM=xterm"}
	return c
}

func (r *rig) run(stdin string, prog string, args ...string) (string, string, int) {
	r.t.Helper()
	c := r.cmd(prog, args...)
	c.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	done := make(chan error, 1)
	if err := c.Start(); err != nil {
		r.t.Fatal(err)
	}
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		c.Process.Kill()
		<-done
		r.t.Fatalf("%s %q did not end; stderr: %s", prog, args, errb.String())
	}
	return out.String(), errb.String(), c.ProcessState.ExitCode()
}

func TestNothingReachesTheBackendBeforeAuthentication(t *testing.T) {
	r := newRig(t)
	addr := "127.0.0.1:" + strconv.Itoa(r.port)
	for i := 0; i < 5; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
		}
	}
	if c, err := net.Dial("tcp", addr); err == nil {
		c.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		io.ReadAll(io.LimitReader(c, 4096))
		c.Close()
	}
	if c, err := net.Dial("tcp", addr); err == nil {
		c.Write([]byte("SSH-2.0-probe\r\n"))
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 256)
		n, _ := c.Read(buf)
		if !strings.HasPrefix(string(buf[:n]), "SSH-2.0-fev_") {
			t.Errorf("the version string is %q", buf[:n])
		}
		c.Close()
	}
	os.WriteFile(filepath.Join(r.dir, "other"), nil, 0o600)
	other := filepath.Join(r.dir, "other_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", other).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v %s", err, out)
	}
	c := exec.Command("ssh", "-F", "/dev/null", "-p", strconv.Itoa(r.port), "-i", other, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
		"-o", "UserKnownHostsFile="+r.files.KnownHostsPath(), "default@127.0.0.1", "true")
	c.Env = []string{"HOME=" + r.userDir, "PATH=/usr/bin:/bin"}
	var errb bytes.Buffer
	c.Stderr = &errb
	c.Run()
	errOut, code := errb.String(), c.ProcessState.ExitCode()
	if code != 255 || !strings.Contains(errOut, "Permission denied (publickey)") {
		t.Errorf("a refused key: status %d, %q", code, errOut)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	if _, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{User: "u24", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer), ssh.Password("x")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second}); err == nil {
		t.Error("a key that is not authorized, or a password, logged in")
	}
	if n := r.b.calls(); n != 0 {
		t.Errorf("the backend was asked %d times before anyone authenticated", n)
	}
	if out, _, code := r.run("", "ssh", "fev", "echo in"); code != 0 || out != "in\n" {
		t.Errorf("the authorized key: status %d, %q", code, out)
	}
	if r.b.resolves.Load() == 0 || r.b.execs.Load() != 1 {
		t.Errorf("after one session: %d resolves, %d execs", r.b.resolves.Load(), r.b.execs.Load())
	}
}

func TestTheOpenSSHClientRunsCommands(t *testing.T) {
	r := newRig(t)
	out, errOut, code := r.run("", "ssh", "fev", "echo to-out; echo to-err >&2; exit 7")
	if out != "to-out\n" || errOut != "to-err\n" || code != 7 {
		t.Errorf("streams and status: %q %q %d", out, errOut, code)
	}
	data := strings.Repeat("fev front end\n", 20000)
	out, _, code = r.run(data, "ssh", "fev", "cat")
	if out != data || code != 0 {
		t.Errorf("cat gave %d bytes of %d, status %d", len(out), len(data), code)
	}
	for login, want := range map[string]string{"fev": "u24", "default@fev": "u24", "f43@fev": "f43", "root@f43@fev": "f43", "default@f43@fev": "f43"} {
		user, host, _ := strings.Cut(login, "@")
		args := []string{"-l", user, host}
		if host == "" {
			args = []string{login}
		} else if strings.Contains(host, "@") {
			args = []string{login}
		}
		out, _, code := r.run("", "ssh", append(args, "basename $HOME")...)
		if strings.TrimSpace(out) != want || code != 0 {
			t.Errorf("%s landed in %q (status %d), want %s", login, out, code, want)
		}
	}
	_, errOut, code = r.run("", "ssh", "nobody@nosuch@fev", "true")
	if code != 1 || !strings.Contains(errOut, `no machine "nosuch"`) {
		t.Errorf("an unknown machine: %d %q", code, errOut)
	}
	_, errOut, code = r.run("", "ssh", "nosuchuser@fev", "true")
	if code != 1 || !strings.Contains(errOut, `has no user "nosuchuser"`) {
		t.Errorf("an unknown user: %d %q", code, errOut)
	}
	out, _, _ = r.run("", "ssh", "-o", "SendEnv=LANG COLORTERM", "fev", "echo $COLORTERM.$LANG.$SSH_CONNECTION")
	if !strings.HasPrefix(out, "truecolor..127.0.0.1 ") {
		t.Errorf("the environment: %q", out)
	}
}

func TestSignalsAndAgentForwarding(t *testing.T) {
	r := newRig(t)
	_, errOut, code := r.run("", "ssh", "-v", "fev", "kill -TERM $$")
	if code != 255 || !strings.Contains(errOut, "exit-signal") {
		t.Errorf("killed by TERM: status %d; %s", code, lastLines(errOut, 8))
	}
	agent := filepath.Join(r.dir, "agent.sock")
	l, err := net.Listen("unix", agent)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		keyring := sshagent.NewKeyring()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go sshagent.ServeAgent(keyring, c)
		}
	}()
	c := r.cmd("ssh", "-A", "fev", "echo agent")
	c.Env = append(c.Env, "SSH_AUTH_SOCK="+agent)
	out, err := c.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "agent" {
		t.Errorf("ssh -A: %v %q", err, out)
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func TestScpAndSftp(t *testing.T) {
	r := newRig(t)
	src := filepath.Join(r.dir, "src.bin")
	data := bytes.Repeat([]byte("0123456789abcdef"), 1<<16)
	os.WriteFile(src, data, 0o644)
	home := r.b.homes["u24"]
	for _, mode := range [][]string{nil, {"-O"}} {
		name := "up" + strings.Join(mode, "")
		if _, errOut, code := r.run("", "scp", append(mode, src, "fev:"+name)...); code != 0 {
			t.Fatalf("scp %v up: %d %s", mode, code, errOut)
		}
		if got, _ := os.ReadFile(filepath.Join(home, name)); !bytes.Equal(got, data) {
			t.Errorf("scp %v up: %d bytes", mode, len(got))
		}
		back := filepath.Join(r.dir, "back"+strings.Join(mode, ""))
		if _, errOut, code := r.run("", "scp", append(mode, "fev:"+name, back)...); code != 0 {
			t.Fatalf("scp %v down: %d %s", mode, code, errOut)
		}
		if got, _ := os.ReadFile(back); !bytes.Equal(got, data) {
			t.Errorf("scp %v down: %d bytes", mode, len(got))
		}
	}
	os.WriteFile(filepath.Join(home, "there"), []byte("in the machine\n"), 0o644)
	got := filepath.Join(r.dir, "got")
	out, errOut, code := r.run(fmt.Sprintf("pwd\nput %s put-file\nget there %s\nls\n", src, got), "sftp", "-b", "-", "fev")
	if code != 0 || !strings.Contains(out, "Remote working directory: "+home) || !strings.Contains(out, "put-file") {
		t.Errorf("sftp: %d %q %q", code, out, errOut)
	}
	if b, _ := os.ReadFile(got); string(b) != "in the machine\n" {
		t.Errorf("sftp get: %q", b)
	}
	if req := r.b.lastRequest(); req.Subsystem != "sftp" || req.TTY || len(req.Argv) != 0 {
		t.Errorf("the SFTP session was asked as %+v", req)
	}
}

func TestForwarding(t *testing.T) {
	r := newRig(t)
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func() {
				b, _ := io.ReadAll(c)
				c.Write(bytes.ToUpper(b))
				c.Close()
			}()
		}
	}()
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	local := free.Addr().(*net.TCPAddr).Port
	free.Close()
	fw := r.cmd("ssh", "-N", "-o", "ExitOnForwardFailure=yes", "-L", fmt.Sprintf("%d:localhost:%d", local, target.Addr().(*net.TCPAddr).Port), "fev")
	var fwErr bytes.Buffer
	fw.Stderr = &fwErr
	if err := fw.Start(); err != nil {
		t.Fatal(err)
	}
	defer fw.Process.Kill()
	var c net.Conn
	for i := 0; i < 100; i++ {
		if c, err = net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", local)); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if c == nil {
		t.Fatalf("-L did not listen: %v %s", err, fwErr.String())
	}
	c.Write([]byte("through the machine"))
	c.(*net.TCPConn).CloseWrite()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, _ := io.ReadAll(c)
	if string(got) != "THROUGH THE MACHINE" {
		t.Errorf("-L carried %q", got)
	}
	if _, err := ForwardAddress("example.com", 80); err == nil {
		t.Error("a name was taken as an address")
	}
	if a, err := ForwardAddress("localhost", 8080); err != nil || a.String() != "127.0.0.1:8080" {
		t.Errorf("localhost is %v %v", a, err)
	}
	_, errOut, code := r.run("", "ssh", "-N", "-o", "ExitOnForwardFailure=yes", "-R", "0:localhost:22", "fev")
	if code != 255 || !strings.Contains(errOut, "remote port forwarding failed") {
		t.Errorf("-R: %d %q", code, errOut)
	}
}

func TestATerminalAndItsResize(t *testing.T) {
	r := newRig(t)
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 for a terminal")
	}
	script := `import os, pty, sys, time, fcntl, termios, struct, select, signal
pid, fd = pty.fork()
if pid == 0:
    os.execvp(sys.argv[1], sys.argv[1:])
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 33, 101, 0, 0))
out = b""
resized = False
deadline = time.time() + 30
while time.time() < deadline:
    r, _, _ = select.select([fd], [], [], 0.2)
    if r:
        try:
            b = os.read(fd, 4096)
        except OSError:
            break
        if not b:
            break
        out += b
    if not resized and b"33 101" in out:
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 44, 120, 0, 0))
        os.kill(pid, signal.SIGWINCH)
        resized = True
_, status = os.waitpid(pid, 0)
sys.stdout.write(out.decode(errors="replace"))
sys.exit(os.waitstatus_to_exitcode(status))
`
	c := r.cmd(py, "-c", script, "ssh", "-F", r.cfg, "-t", "fev", "fev-size")
	c.Args = []string{py, "-c", script, "ssh", "-F", r.cfg, "-t", "fev", "fev-size"}
	out, err := c.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "33 101") || !strings.Contains(string(out), "44 120") {
		t.Errorf("the terminal's size: %v %q", err, out)
	}
	if req := r.b.lastRequest(); !req.TTY || !strings.Contains(strings.Join(req.Env, " "), "TERM=") {
		t.Errorf("the terminal was asked as %+v", req)
	}
}

func TestGlobalRequests(t *testing.T) {
	r := newRig(t)
	signer, err := ssh.ParsePrivateKey(mustRead(t, r.files.IdentityPath()))
	if err != nil {
		t.Fatal(err)
	}
	host, _ := r.files.HostSigner()
	c, err := ssh.Dial("tcp", "127.0.0.1:"+strconv.Itoa(r.port), &ssh.ClientConfig{User: "default", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(host.PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if ok, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil || !ok {
		t.Errorf("keepalive: %v %v", ok, err)
	}
	if _, err := c.Listen("tcp", "127.0.0.1:0"); err == nil {
		t.Error("tcpip-forward was granted")
	}
	if _, _, err := c.OpenChannel("direct-streamlocal@openssh.com", nil); err == nil {
		t.Error("a streamlocal channel was opened")
	}
	if r.b.execs.Load() != 0 || r.b.dials.Load() != 0 {
		t.Error("the backend was asked to run or dial something")
	}
}

func TestATerminalNameWithANULIsRefused(t *testing.T) {
	r := newRig(t)
	signer, err := ssh.ParsePrivateKey(mustRead(t, r.files.IdentityPath()))
	if err != nil {
		t.Fatal(err)
	}
	host, _ := r.files.HostSigner()
	c, err := ssh.Dial("tcp", "127.0.0.1:"+strconv.Itoa(r.port), &ssh.ClientConfig{User: "default", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(host.PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RequestPty("xterm\x00evil", 24, 80, ssh.TerminalModes{}); err == nil {
		t.Error("a terminal name with a NUL was taken")
	}
	if err := s.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Errorf("a plain terminal name after it: %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
