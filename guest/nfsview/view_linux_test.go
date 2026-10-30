package nfsview

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	api "fev/proto/agent"

	"golang.org/x/sys/unix"
)

func init() { runtime.LockOSThread() }

func needNFSD(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := os.Stat("/proc/fs/nfsd"); err != nil {
		t.Skip("no nfsd in this kernel")
	}
}

func nsIno(path string) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, err
	}
	return st.Ino, nil
}

func mounted(dir string) bool {
	b, err := os.ReadFile("/proc/thread-self/mountinfo")
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 4 && f[4] == dir {
			return true
		}
	}
	return false
}

func TestNamespaces(t *testing.T) {
	needNFSD(t)
	shared := filepath.Join(t.TempDir(), "shared")
	os.Mkdir(shared, 0o755)
	if err := unix.Mount("tmpfs", shared, "tmpfs", 0, "size=1m"); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(shared, unix.MNT_DETACH)
	if err := unix.Mount("", shared, "", unix.MS_SHARED, ""); err != nil {
		t.Fatal(err)
	}
	ns, err := NewNamespaces()
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	initNet, _ := nsIno("/proc/thread-self/ns/net")
	err = ns.Do(func() error {
		ifs, err := net.Interfaces()
		if err != nil {
			return err
		}
		lo := false
		for _, i := range ifs {
			if i.Name == "lo" {
				lo = i.Flags&net.FlagUp != 0
			} else if i.Flags&net.FlagUp != 0 {
				return fmt.Errorf("%s is up in the view's namespace", i.Name)
			}
		}
		if !lo {
			return fmt.Errorf("lo is not up in the view's namespace: %+v", ifs)
		}
		mine, _ := nsIno("/proc/thread-self/ns/net")
		leader, _ := nsIno("/proc/self/ns/net")
		if mine == initNet || leader != initNet {
			return fmt.Errorf("the thread's namespace %d, the leader's %d, init's %d", mine, leader, initNet)
		}
		rpc, err := nsIno(RPCDir)
		if err != nil {
			return err
		}
		leaders, err := nsIno("/proc/self/net/rpc")
		if err != nil {
			return err
		}
		if rpc == leaders {
			return errors.New("RPCDir is the thread group leader's")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	inView := filepath.Join(t.TempDir(), "in-view")
	inInit := filepath.Join(shared, "in-init")
	os.Mkdir(inView, 0o755)
	os.Mkdir(inInit, 0o755)
	if err := ns.Do(func() error { return unix.Mount("tmpfs", inView, "tmpfs", 0, "size=1m") }); err != nil {
		t.Fatal(err)
	}
	if mounted(inView) {
		t.Error("the view's mount is in init's namespace")
	}
	if err := unix.Mount("tmpfs", inInit, "tmpfs", 0, "size=1m"); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(inInit, 0)
	var seen bool
	ns.Do(func() error { seen = mounted(inInit); return nil })
	if !seen {
		t.Error("a mount init made in its shared mount after the view's namespaces is not in the view")
	}
}

type xdr struct{ b []byte }

func (x *xdr) u32(v uint32) { x.b = binary.BigEndian.AppendUint32(x.b, v) }
func (x *xdr) opaque(s []byte) {
	x.u32(uint32(len(s)))
	x.b = append(x.b, s...)
	for len(x.b)%4 != 0 {
		x.b = append(x.b, 0)
	}
}

const (
	nfsOK        = 0
	nfsErrAccess = 13
	opGetattr    = 9
	opLookup     = 15
	opPutrootfh  = 24
)

func compound(c net.Conn, xid, uid, gid uint32, name string) (uint32, error) {
	var x xdr
	x.u32(xid)
	x.u32(0)
	x.u32(2)
	x.u32(100003)
	x.u32(4)
	x.u32(1)
	var cred xdr
	cred.u32(0)
	cred.opaque([]byte("test"))
	cred.u32(uid)
	cred.u32(gid)
	cred.u32(0)
	x.u32(1)
	x.opaque(cred.b)
	x.u32(0)
	x.u32(0)
	x.opaque(nil)
	x.u32(0)
	x.u32(3)
	x.u32(opPutrootfh)
	x.u32(opLookup)
	x.opaque([]byte(name))
	x.u32(opGetattr)
	x.u32(1)
	x.u32(1 << 1)
	rec := binary.BigEndian.AppendUint32(nil, 0x80000000|uint32(len(x.b)))
	c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := c.Write(append(rec, x.b...)); err != nil {
		return 0, err
	}
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return 0, err
	}
	body := make([]byte, binary.BigEndian.Uint32(hdr[:])&0x7fffffff)
	if _, err := io.ReadFull(c, body); err != nil {
		return 0, err
	}
	w := func(i int) uint32 { return binary.BigEndian.Uint32(body[4*i:]) }
	if len(body) < 28 || w(0) != xid || w(1) != 1 || w(2) != 0 {
		return 0, fmt.Errorf("not an accepted reply: % x", body[:min(len(body), 32)])
	}
	vlen := int(w(4)+3) &^ 3
	i := 5 + vlen/4
	if w(i) != 0 {
		return 0, fmt.Errorf("accept_stat %d", w(i))
	}
	return w(i + 1), nil
}

func TestViewServes(t *testing.T) {
	needNFSD(t)
	root := filepath.Join(t.TempDir(), "tree")
	told := make(chan api.ViewStatus, 16)
	v := NewView(&LinuxStarter{Root: root, Logf: t.Logf}, func(s api.ViewStatus) { told <- s })
	wait := func(want string) {
		t.Helper()
		for {
			select {
			case s := <-told:
				if err := api.CheckViewStatus(s); err != nil {
					t.Fatalf("%+v: %v", s, err)
				}
				if s.State == want {
					return
				}
				if s.State == api.ViewFailed {
					t.Fatalf("the view failed: %s", s.Error)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("not %s in 30 s", want)
			}
		}
	}
	for round := 1; round <= 2; round++ {
		t0 := time.Now()
		v.Start(api.ViewStartParams{UID: 501, GID: 20})
		wait(api.ViewReady)
		t.Logf("round %d: ready in %v", round, time.Since(t0))
		if c, err := net.DialTimeout("tcp", NFSAddress, time.Second); err == nil {
			c.Close()
			t.Fatalf("%s answers outside the view", NFSAddress)
		}
		c, err := v.Dial(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st, err := compound(c, 1, 501, 20, "README.txt"); err != nil || st != nfsOK {
			t.Fatalf("round %d: the view's user's LOOKUP README.txt: status %d, %v", round, st, err)
		}
		if st, err := compound(c, 2, 502, 20, "README.txt"); err != nil || st != nfsErrAccess {
			t.Fatalf("round %d: another uid's LOOKUP: status %d, %v (want %d)", round, st, err, nfsErrAccess)
		}
		if st, err := compound(c, 3, 0, 0, "README.txt"); err != nil || st != nfsOK {
			t.Fatalf("round %d: uid 0's LOOKUP: status %d, %v (want %d)", round, st, err, nfsOK)
		}
		c.Close()
		if b, err := os.ReadFile(OwnerChangeParam); err != nil || strings.TrimSpace(string(b)) != "Y" {
			t.Errorf("round %d: %s reads %q, %v; want Y", round, OwnerChangeParam, b, err)
		}
		r := v.run.(*linuxRun)
		var grace string
		r.ns.Do(func() error {
			b, err := os.ReadFile(filepath.Join(NFSDDir, "v4_end_grace"))
			grace = strings.TrimSpace(string(b))
			return err
		})
		if grace != "Y" {
			t.Errorf("round %d: v4_end_grace reads %q, want Y (the grace skipped)", round, grace)
		}
		if s, err := v.Stop(); err != nil || s.State != api.ViewStopped {
			t.Fatalf("stop: %+v %v", s, err)
		}
		wait(api.ViewStopped)
		if _, err := v.Dial(context.Background()); !errors.Is(err, ErrViewStopped) {
			t.Fatalf("dial after the stop: %v", err)
		}
	}
}

func TestAKernelWithoutTheOwnerSwitchIsRefused(t *testing.T) {
	needNFSD(t)
	was := OwnerChangeParam
	OwnerChangeParam = filepath.Join(t.TempDir(), "fev_refuse_owner_change")
	defer func() { OwnerChangeParam = was }()
	root := filepath.Join(t.TempDir(), "tree")
	told := make(chan api.ViewStatus, 16)
	v := NewView(&LinuxStarter{Root: root, Logf: t.Logf}, func(s api.ViewStatus) { told <- s })
	v.Start(api.ViewStartParams{UID: 501, GID: 20})
	for {
		select {
		case s := <-told:
			if s.State == api.ViewReady {
				t.Fatal("the view is ready on a kernel without patch 9010")
			}
			if s.State != api.ViewFailed {
				continue
			}
			if !strings.Contains(s.Error, "patch 9010") {
				t.Fatalf("failed, but not for the switch: %q", s.Error)
			}
			if _, err := os.Stat(root); err == nil {
				t.Error("the tree's root was made before the refusal")
			}
			return
		case <-time.After(30 * time.Second):
			t.Fatal("neither failed nor ready in 30 s")
		}
	}
}

func TestNfsdTakesTheHostOnItsVsockPort(t *testing.T) {
	needNFSD(t)
	root := filepath.Join(t.TempDir(), "tree")
	told := make(chan api.ViewStatus, 16)
	v := NewView(&LinuxStarter{Root: root, Logf: t.Logf, VsockPort: api.ViewVsockPort}, func(s api.ViewStatus) { told <- s })
	for round := 1; round <= 2; round++ {
		v.Start(api.ViewStartParams{UID: 501, GID: 20})
		var ready api.ViewStatus
	wait:
		for {
			select {
			case s := <-told:
				if s.State == api.ViewFailed {
					t.Fatalf("the view failed: %s", s.Error)
				}
				if s.State == api.ViewReady {
					ready = s
					break wait
				}
			case <-time.After(30 * time.Second):
				t.Fatal("not ready in 30 s")
			}
		}
		_, verr := v.run.(*linuxRun).srv.Vsock()
		switch {
		case verr != nil:
			t.Logf("round %d: this kernel has no patch 9011: %v", round, verr)
			if ready.VsockPort != 0 {
				t.Fatalf("round %d: the status names vsock port %d with no listener", round, ready.VsockPort)
			}
		case ready.VsockPort != api.ViewVsockPort:
			t.Fatalf("round %d: the status names vsock port %d, want %d", round, ready.VsockPort, api.ViewVsockPort)
		default:
			fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			answered := false
			if unix.Connect(fd, &unix.SockaddrVM{CID: 1, Port: api.ViewVsockPort}) == nil {
				f := os.NewFile(uintptr(fd), "vsock")
				c, err := net.FileConn(f)
				f.Close()
				if err == nil {
					c.SetDeadline(time.Now().Add(2 * time.Second))
					_, err = nullCall(c, 7)
					answered = err == nil
					c.Close()
				}
			} else {
				unix.Close(fd)
			}
			if answered {
				t.Fatalf("round %d: nfsd answered the guest's own vsock connection", round)
			}
		}
		if s, err := v.Stop(); err != nil || s.State != api.ViewStopped {
			t.Fatalf("stop: %+v %v", s, err)
		}
	}
}

func nullCall(c net.Conn, xid uint32) (uint32, error) {
	b := make([]byte, 44)
	binary.BigEndian.PutUint32(b, 0x80000000|40)
	for i, v := range []uint32{xid, 0, 2, 100003, 4, 0, 0, 0, 0, 0} {
		binary.BigEndian.PutUint32(b[4+4*i:], v)
	}
	if _, err := c.Write(b); err != nil {
		return 0, err
	}
	head := make([]byte, 8)
	if _, err := io.ReadFull(c, head); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(head[4:]), nil
}
