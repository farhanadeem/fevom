package nfsview

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"fev/proto/relay"
)

var standinBuild struct {
	once sync.Once
	path string
	err  error
}

func standin(t *testing.T, args ...string) *os.Process {
	t.Helper()
	standinBuild.once.Do(func() {
		dir, err := os.MkdirTemp("", "nfsstandin")
		if err != nil {
			standinBuild.err = err
			return
		}
		standinBuild.path = filepath.Join(dir, "standin")
		cmd := exec.Command("go", "build", "-o", standinBuild.path, ".")
		cmd.Dir = filepath.Join("..", "..", "tests", "fixtures", "nfsstandin")
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly")
		if out, err := cmd.CombinedOutput(); err != nil {
			standinBuild.err = errors.New(string(out))
		}
	})
	if standinBuild.err != nil {
		t.Fatalf("building tests/fixtures/nfsstandin: %v", standinBuild.err)
	}
	cmd := exec.Command(standinBuild.path, append([]string{"-lifeline"}, args...)...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	line, _ := bufio.NewReader(out).ReadString('\n')
	if !strings.HasPrefix(line, "listening ") {
		t.Fatalf("the stand-in said %q", line)
	}
	return cmd.Process
}

type dialOpener struct{ path string }

func (o dialOpener) Open(ctx context.Context, req relay.Request) (*net.UnixConn, error) {
	if req != Request {
		return nil, &relay.Error{Code: relay.CodeBadRequest, Text: req.String()}
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", o.path)
	if err != nil {
		return nil, err
	}
	return c.(*net.UnixConn), nil
}

type view struct {
	dir    string
	server *Server
	proc   *os.Process
}

func newView(t *testing.T) *view {
	t.Helper()
	sd := shortDir(t)
	proc := standin(t, "-unix", filepath.Join(sd, "standin.sock"))
	s, err := ListenLoopback(dialOpener{filepath.Join(sd, "standin.sock")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := opened(t)
	t.Cleanup(func() { ForceUnmount(dir); s.Close() })
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	return &view{dir: dir, server: s, proc: proc}
}

func (v *view) mount(t *testing.T, hidden bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Mount(ctx, v.server.Endpoint(), v.dir, hidden); err != nil {
		t.Fatal(err)
	}
}

func TestTheViewMountsAndUnmountsAsTheUser(t *testing.T) {
	v := newView(t)
	t0 := time.Now()
	v.mount(t, false)
	t.Logf("mounted in %v", time.Since(t0))
	m, ok, err := Find(v.dir)
	if err != nil || !ok {
		t.Fatalf("Find after the mount: %v %v", ok, err)
	}
	if m.From != ServerName+":/" || !m.View() || m.Type != "nfs" || m.Owner != uint32(os.Getuid()) || m.Hidden() {
		t.Fatalf("the mount: %+v", m)
	}
	if m.Flags&unix.MNT_NOATIME == 0 || m.Flags&unix.MNT_NOSUID == 0 || m.Flags&unix.MNT_NODEV == 0 {
		t.Fatalf("flags %#x: want noatime, nosuid, nodev", m.Flags)
	}
	if data, err := os.ReadFile(filepath.Join(v.dir, "README.txt")); err != nil || string(data) != "fev view stand-in\n" {
		t.Fatalf("the view's README: %q %v", data, err)
	}
	if v.server.Conns() != 1 {
		t.Fatalf("%d connections through the socket", v.server.Conns())
	}
	forced, err := Unmount(v.dir)
	if err != nil || forced {
		t.Fatalf("Unmount of an idle view: forced %v, %v", forced, err)
	}
	if _, ok, _ := Find(v.dir); ok {
		t.Fatal("still mounted")
	}
	checkStopped(t, v.dir)
	if forced, err := Unmount(v.dir); err != nil || forced {
		t.Fatalf("Unmount with nothing mounted: %v %v", forced, err)
	}
	if err := ForceUnmount(v.dir); err != nil {
		t.Fatal(err)
	}
	v.mount(t, false)
	if _, ok, _ := Find(v.dir); !ok {
		t.Fatal("a second mount is not there")
	}
}

func TestHiddenChangesOnTheLiveMount(t *testing.T) {
	v := newView(t)
	v.mount(t, true)
	if m, _, _ := Find(v.dir); !m.Hidden() {
		t.Fatalf("mounted hidden: flags %#x", m.Flags)
	}
	f, err := os.Open(filepath.Join(v.dir, "README.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx := context.Background()
	for _, hidden := range []bool{false, true, false} {
		if err := SetHidden(ctx, v.server.Endpoint(), v.dir, hidden); err != nil {
			t.Fatal(err)
		}
		m, ok, _ := Find(v.dir)
		if !ok || m.Hidden() != hidden || m.Flags&unix.MNT_NOATIME == 0 {
			t.Fatalf("hidden %v: mounted %v, flags %#x", hidden, ok, m.Flags)
		}
	}
	buf := make([]byte, 3)
	if n, err := f.ReadAt(buf, 0); n != 3 || string(buf) != "fev" {
		t.Fatalf("the open file after the changes: %q %v", buf[:n], err)
	}
}

func TestABusyViewIsUnmountedByForce(t *testing.T) {
	v := newView(t)
	v.mount(t, false)
	f, err := os.Open(filepath.Join(v.dir, "README.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	t0 := time.Now()
	forced, err := Unmount(v.dir)
	if err != nil || !forced {
		t.Fatalf("Unmount with a file open: forced %v, %v", forced, err)
	}
	t.Logf("unmounted by force in %v", time.Since(t0))
	if _, ok, _ := Find(v.dir); ok {
		t.Fatal("still mounted")
	}
	if _, err := f.ReadAt(make([]byte, 1), 0); err == nil {
		t.Fatal("the open file still reads after the forced unmount")
	}
	checkStopped(t, v.dir)
}

func TestRecoverRemovesADeadDaemonsMount(t *testing.T) {
	v := newView(t)
	v.mount(t, false)
	v.server.Close()
	v.proc.Kill()
	t0 := time.Now()
	removed, err := Recover(v.dir)
	if err != nil || !removed {
		t.Fatalf("Recover: %v %v", removed, err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("Recover took %v", d)
	} else {
		t.Logf("Recover took %v", d)
	}
	if _, ok, _ := Find(v.dir); ok {
		t.Fatal("still mounted")
	}
	checkStopped(t, v.dir)
	if removed, err := Recover(v.dir); err != nil || removed {
		t.Fatalf("Recover with nothing mounted: %v %v", removed, err)
	}
}

func TestRecoverLeavesAnotherMountAlone(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	ln.Close()
	standin(t, "-tcp", addr.String())
	dir := opened(t)
	os.Mkdir(dir, 0o755)
	t.Cleanup(func() { ForceUnmount(dir) })
	out, err := exec.Command(mountNFS, "-o", "vers=4.0,soft,timeo=10,retrans=2,port="+
		strings.TrimPrefix(strings.TrimPrefix(addr.String(), "127.0.0.1"), ":"), "127.0.0.1:/", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("mount_nfs over TCP: %v %s", err, out)
	}
	removed, err := Recover(dir)
	if !errors.Is(err, ErrForeignMount) || removed {
		t.Fatalf("Recover: %v %v", removed, err)
	}
	if _, ok, _ := Find(dir); !ok {
		t.Fatal("the other mount was removed")
	}
}

func TestFindDoesNotWaitOnADeadServer(t *testing.T) {
	v := newView(t)
	v.mount(t, false)
	v.proc.Kill()
	v.server.Close()
	t0 := time.Now()
	m, ok, err := Find(v.dir)
	if err != nil || !ok || !m.View() {
		t.Fatalf("Find: %+v %v %v", m, ok, err)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("Find took %v with the server gone", d)
	}
	t0 = time.Now()
	if err := ForceUnmount(v.dir); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("ForceUnmount took %v with the server gone", d)
	}
	t.Logf("Find and ForceUnmount with the server gone: %v", time.Since(t0))
}
