package daemon

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	api "fev/proto/daemon"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type sshFixture struct {
	*machinesFixture
	s    *SSH
	port int
}

func sshDaemon(t *testing.T) *sshFixture {
	t.Helper()
	port := freePort(t)
	s := &SSH{Fevom: "/usr/local/bin/fev-test"}
	mf := machinesDaemon(t, func(m *Machines) { s.Machines = m }, func(f *fixture) {
		os.MkdirAll(filepath.Dir(f.layout.ConfigFile()), 0o755)
		os.WriteFile(f.layout.ConfigFile(), []byte(fmt.Sprintf(`{"version":1,"values":{%q:%d}}`, api.KeySSHPort, port)), 0o600)
		f.opts.Modules = append(f.opts.Modules, s)
	})
	return &sshFixture{machinesFixture: mf, s: s, port: port}
}

func (f *sshFixture) clientConfig(user string) *ssh.ClientConfig {
	f.t.Helper()
	key, err := os.ReadFile(f.s.files.IdentityPath())
	if err != nil {
		f.t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		f.t.Fatal(err)
	}
	host, _ := f.s.files.HostSigner()
	return &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey()), Timeout: 10 * time.Second}
}

func (f *sshFixture) exec(user, cmd string) (string, error) {
	f.t.Helper()
	c, err := ssh.Dial("tcp", "127.0.0.1:"+strconv.Itoa(f.port), f.clientConfig(user))
	if err != nil {
		return "", err
	}
	defer c.Close()
	s, err := c.NewSession()
	if err != nil {
		return "", err
	}
	out, err := s.CombinedOutput(cmd)
	return string(out), err
}

func TestSSHStartsTheVMOnlyAfterAuthentication(t *testing.T) {
	f := sshDaemon(t)
	f.create("u24", "ubuntu")
	if _, err := f.c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	mark := api.StopMarkPath(f.layout.Home)
	addr := "127.0.0.1:" + strconv.Itoa(f.port)
	for i := 0; i < 5; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
		}
	}
	if c, err := net.Dial("tcp", addr); err == nil {
		c.Write([]byte("SSH-2.0-probe\r\n\x00\x00\x00garbage"))
		c.SetReadDeadline(time.Now().Add(time.Second))
		io.Copy(io.Discard, c)
		c.Close()
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(priv)
	host, _ := f.s.files.HostSigner()
	for _, user := range []string{"default", "u24", "root@u24"} {
		if _, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(other), ssh.Password("pw")},
			HostKeyCallback: ssh.FixedHostKey(host.PublicKey()), Timeout: 5 * time.Second}); err == nil {
			t.Fatalf("%s logged in with a key nobody authorized", user)
		}
	}
	if fd, err := takeConn(api.SSHSocketPath(f.layout.Home)); err != nil {
		t.Errorf("the SSH socket: %v", err)
	} else {
		syscall.Close(fd)
	}
	time.Sleep(200 * time.Millisecond)
	if st := f.d.hub.Status(); st.State != api.StateStopped {
		t.Fatalf("the VM is %s after probes and refused keys", st.State)
	}
	if _, err := os.Stat(mark); err != nil {
		t.Fatalf("the stop mark went: %v", err)
	}
	out, err := f.exec("default", "echo logged in")
	if err != nil || out != "logged in\n" {
		t.Fatalf("an authenticated command: %q %v", out, err)
	}
	if st := f.d.hub.Status(); st.State == api.StateStopped || st.State == api.StateStopping {
		t.Errorf("the VM is %s after an authenticated session", st.State)
	}
	if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stop mark stayed after an authenticated session: %v", err)
	}
	c, err := ssh.Dial("tcp", addr, f.clientConfig("root@u24"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, _ := c.NewSession()
	out2, _ := s.Output("whoami")
	if string(out2) != "root\n" {
		t.Errorf("root@u24 is %q", out2)
	}
	s, _ = c.NewSession()
	var exit *ssh.ExitError
	if err := s.Run("exit 9"); !errors.As(err, &exit) || exit.ExitStatus() != 9 {
		t.Errorf("exit 9: %v", err)
	}
	s, _ = c.NewSession()
	var stderr bytes.Buffer
	s.Stderr = &stderr
	if err := s.Run("nosuchcommand"); !errors.As(err, &exit) || exit.ExitStatus() != 127 || !strings.Contains(stderr.String(), "not found") {
		t.Errorf("a command not found: %v %q", err, stderr.String())
	}
	if _, err := f.exec("x@nosuch", "true"); err == nil {
		t.Error("a machine that does not exist ran a command")
	}
}

func TestSSHInfo(t *testing.T) {
	f := sshDaemon(t)
	var raw json.RawMessage
	if err := f.raw().Call(bg, api.MethodSSHInfo, nil, &raw); err != nil {
		t.Fatal(err)
	}
	if err := api.Conforms(raw, api.SSHInfo{}); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	var i api.SSHInfo
	json.Unmarshal(raw, &i)
	if i.Host != "127.0.0.1" || i.Port != f.port || !i.Listening || i.ExposePort || i.Alias != "fev" || i.User != "default" ||
		i.Socket != api.SSHSocketPath(f.layout.Home) || i.ConfigFile != filepath.Join(f.layout.Home, "ssh", "config") ||
		!strings.HasPrefix(i.HostKey, "256 SHA256:") || i.AuthorizedKeysError != "" ||
		i.ProxyCommand != `"/usr/local/bin/fev-test" --home "`+f.layout.Home+`" ssh --proxy` {
		t.Errorf("ssh.info: %+v", i)
	}
	if st := f.d.hub.Status(); st.State != api.StateStopped {
		t.Errorf("ssh.info left the VM %s", st.State)
	}
	cfg, _ := os.ReadFile(i.ConfigFile)
	if !strings.Contains(string(cfg), "Port "+strconv.Itoa(f.port)+"\n") || !strings.Contains(string(cfg), i.ProxyCommand) {
		t.Errorf("the generated entry:\n%s", cfg)
	}
	port := freePort(t)
	if _, err := f.c.ConfigSet(bg, map[string]any{api.KeySSHPort: port}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the new port", func() bool {
		c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	if c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(f.port)); err == nil {
		c.Close()
		t.Error("the old port still answers")
	}
	cfg, _ = os.ReadFile(i.ConfigFile)
	if !strings.Contains(string(cfg), "Port "+strconv.Itoa(port)+"\n") {
		t.Errorf("the entry kept the old port:\n%s", cfg)
	}
}

func TestADevelopmentDaemonLeavesTheDefaultSSHPortAlone(t *testing.T) {
	s := &SSH{Fevom: "/usr/local/bin/fev-test", OwnPortOnly: true}
	f := &sshFixture{machinesFixture: machinesDaemon(t, func(m *Machines) { s.Machines = m }, func(f *fixture) {
		f.opts.Modules = append(f.opts.Modules, s)
	}), s: s}
	info := func() api.SSHInfo {
		t.Helper()
		var i api.SSHInfo
		if err := f.raw().Call(bg, api.MethodSSHInfo, nil, &i); err != nil {
			t.Fatal(err)
		}
		return i
	}
	if a := s.Addresses(); len(a) != 0 {
		t.Errorf("listening on %v with ssh.port not set", a)
	}
	if i := info(); i.Listening || i.Port != api.DefaultSSHPort {
		t.Errorf("ssh.info: %+v", i)
	}
	if st := f.d.hub.Status(); len(st.Warnings) != 0 {
		t.Errorf("warnings: %q", st.Warnings)
	}
	if c, err := net.Dial("unix", api.SSHSocketPath(f.layout.Home)); err != nil {
		t.Errorf("the Unix socket: %v", err)
	} else {
		c.Close()
	}
	port := freePort(t)
	if _, err := f.c.ConfigSet(bg, map[string]any{api.KeySSHPort: port}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the port set", func() bool { return info().Listening })
	if a := s.Addresses(); len(a) == 0 || !strings.HasSuffix(a[0], ":"+strconv.Itoa(port)) {
		t.Errorf("listening on %v, want port %d", a, port)
	}
	if _, err := f.c.ConfigReset(bg, api.KeySSHPort); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "no port again", func() bool { return !info().Listening })
	if c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
		c.Close()
		t.Error("the port set before still answers after its reset")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s did not come", what)
}

func lanAddress() string {
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				return n.IP.String()
			}
		}
	}
	return ""
}

func TestSSHExposePort(t *testing.T) {
	lan := lanAddress()
	if lan == "" {
		t.Skip("this Mac has no address but loopback")
	}
	f := sshDaemon(t)
	at := func(host string) bool {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(f.port)), 2*time.Second)
		if err != nil {
			return false
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 64)
		n, _ := c.Read(buf)
		return strings.HasPrefix(string(buf[:n]), "SSH-2.0-fev_")
	}
	if at(lan) || !at("127.0.0.1") || !at("::1") {
		t.Fatalf("off: LAN %v, 127.0.0.1 %v, ::1 %v; listening on %v", at(lan), at("127.0.0.1"), at("::1"), f.s.Addresses())
	}
	if _, err := f.c.ConfigSet(bg, map[string]any{api.KeySSHExposePort: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the LAN address", func() bool { return at(lan) })
	if !at("127.0.0.1") {
		t.Error("exposed, the loopback does not answer")
	}
	var i api.SSHInfo
	if err := f.raw().Call(bg, api.MethodSSHInfo, nil, &i); err != nil || i.Host != "0.0.0.0" || !i.ExposePort || !i.Listening {
		t.Errorf("ssh.info exposed: %+v %v", i, err)
	}
	if _, err := f.c.ConfigSet(bg, map[string]any{api.KeySSHExposePort: false}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the LAN address refused", func() bool { return !at(lan) })
	if !at("127.0.0.1") {
		t.Error("back to loopback, the loopback does not answer")
	}
}

func takeConn(path string) (int, error) {
	c, err := net.Dial("unix", path)
	if err != nil {
		return -1, err
	}
	uc := c.(*net.UnixConn)
	defer uc.Close()
	uc.SetDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 64)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, flags, _, err := uc.ReadMsgUnix(buf, oob)
	if err != nil {
		return -1, err
	}
	if flags&syscall.MSG_CTRUNC != 0 || string(buf[:n]) != "ok\n" {
		return -1, fmt.Errorf("reply %q, flags %#x", buf[:n], flags)
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		return -1, fmt.Errorf("control: %v", err)
	}
	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		return -1, fmt.Errorf("rights: %v", err)
	}
	return fds[0], nil
}

func TestTheSSHSocketLosesNoConnectionUnderChurn(t *testing.T) {
	f := sshDaemon(t)
	path := api.SSHSocketPath(f.layout.Home)
	stop := make(chan struct{})
	var churn sync.WaitGroup
	for i := 0; i < 2; i++ {
		churn.Add(1)
		go func() {
			defer churn.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if c, err := net.Dial("unix", f.layout.Socket()); err == nil {
					c.Close()
				}
			}
		}()
	}
	const n = 200
	errs := make(chan error, n)
	var wg sync.WaitGroup
	slots := make(chan struct{}, 32)
	for i := 0; i < n; i++ {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			fd, err := takeConn(path)
			if err != nil {
				errs <- err
				return
			}
			fh := os.NewFile(uintptr(fd), "ssh")
			c, err := net.FileConn(fh)
			fh.Close()
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			buf := make([]byte, 64)
			k, err := c.Read(buf)
			if !strings.HasPrefix(string(buf[:k]), "SSH-2.0-fev_") {
				errs <- fmt.Errorf("the connection said %q (%v)", buf[:k], err)
			}
		}()
	}
	wg.Wait()
	close(stop)
	churn.Wait()
	close(errs)
	lost := 0
	for err := range errs {
		lost++
		if lost <= 3 {
			t.Error(err)
		}
	}
	if lost > 0 {
		t.Errorf("%d of %d connections lost", lost, n)
	}
}
