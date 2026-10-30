package nfsview

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"fev/proto/relay"
)

func TestTheMountHasTheReferencesOptions(t *testing.T) {
	got := strings.Join(MountArgs(Endpoint{Port: 50123}, "/u/Fevom", false), " ")
	want := "-o vers=4.0,soft,intr,nocallback,timeo=10,retrans=10,deadtimeout=0,mutejukebox,nfc," +
		"rsize=131072,wsize=131072,readahead=64,noatime,localhost,port=50123 Fevom:/ /u/Fevom"
	if got != want {
		t.Fatalf("mount_nfs %s\nwant      %s", got, want)
	}
	got = strings.Join(MountArgs(Endpoint{Socket: "/u/.fev/run/view.sock"}, "/u/Fevom", false), " ")
	want = "-o vers=4.0,soft,intr,nocallback,timeo=10,retrans=10,deadtimeout=0,mutejukebox,nfc," +
		"rsize=131072,wsize=131072,readahead=64,noatime </u/.fev/run/view.sock>:/ /u/Fevom"
	if got != want {
		t.Fatalf("mount_nfs %s\nwant      %s", got, want)
	}
	if got := MountArgs(Endpoint{Port: 1}, "/d", true)[1]; !strings.HasSuffix(got, ",nobrowse") {
		t.Fatalf("hidden: %s", got)
	}
}

func TestAViewsMountIsKnownByItsSource(t *testing.T) {
	for from, want := range map[string]string{
		"</u/.fev/run/view.sock>:/": "/u/.fev/run/view.sock",
		"127.0.0.1:/":               "",
		"<x>:/sub":                  "",
		"/dev/disk3s1":              "",
	} {
		if got := socketOf(from); got != want {
			t.Errorf("socketOf(%q) = %q, want %q", from, got, want)
		}
	}
	for m, want := range map[Mounted]bool{
		{Type: "nfs", From: "Fevom:/"}:    true,
		{Type: "nfs", From: "</s>:/"}:     true,
		{Type: "nfs", From: "Other:/"}:    false,
		{Type: "nfs", From: "Fevom:/sub"}: false,
		{Type: "smbfs", From: "</s>:/"}:   false,
		{Type: "smbfs", From: "Fevom:/"}:  false,
	} {
		if got := m.View(); got != want {
			t.Errorf("%+v: View() = %v, want %v", m, got, want)
		}
	}
}

func TestTheLoopbackPeerIsFoundWithItsUser(t *testing.T) {
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	a, err := ln.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	uid, err := peerUID(a.LocalAddr().(*net.TCPAddr), a.RemoteAddr().(*net.TCPAddr))
	if err != nil || uid != uint32(os.Getuid()) {
		t.Fatalf("peerUID = %d, %v; want %d", uid, err, os.Getuid())
	}
}

func TestOnlyTheKernelAndTheUserAreServed(t *testing.T) {
	me := uint32(os.Getuid())
	for uid, want := range map[uint32]bool{0: true, me: true, me + 1: false, 1: false, 4294967294: false} {
		if got := uidServed(uid); got != want {
			t.Errorf("uid %d served: %v, want %v", uid, got, want)
		}
	}
}

func TestTheKernelsTableIsReadByAddressAndPort(t *testing.T) {
	rec := func(kind uint32, n int, fill func([]byte)) []byte {
		b := make([]byte, n)
		binary.LittleEndian.PutUint32(b, uint32(n))
		binary.LittleEndian.PutUint32(b[4:], kind)
		fill(b)
		for len(b)%8 != 0 {
			b = append(b, 0)
		}
		return b
	}
	lo := netip.MustParseAddr("127.0.0.1")
	lan := netip.MustParseAddr("172.16.30.211")
	conn := func(laddr netip.Addr, lport uint16, faddr netip.Addr, fport uint16, uid uint32) []byte {
		in := rec(xsoInpcb, 104, func(b []byte) {
			binary.BigEndian.PutUint16(b[inpcbFport:], fport)
			binary.BigEndian.PutUint16(b[inpcbLport:], lport)
			b[inpcbVflag] = inpIPv4
			l, f := laddr.As4(), faddr.As4()
			copy(b[inpcbLaddr4:], l[:])
			copy(b[inpcbFaddr4:], f[:])
		})
		so := rec(xsoSocket, 108, func(b []byte) { binary.LittleEndian.PutUint32(b[socketUID:], uid) })
		return append(append(in, so...), rec(0x020, 20, func([]byte) {})...)
	}
	tab := make([]byte, 24)
	binary.LittleEndian.PutUint32(tab, 24)
	tab = append(tab, conn(lo, 50001, lo, 2049, 0)...)
	tab = append(tab, conn(lo, 50000, lo, 2049, 503)...)
	tab = append(tab, conn(lan, 50001, lo, 2049, 504)...)
	tab = append(tab, conn(lo, 2049, lo, 50000, 501)...)
	tab = append(tab, make([]byte, 24)...)
	ap := netip.AddrPortFrom
	for _, c := range []struct {
		self, other netip.AddrPort
		uid         uint32
	}{
		{ap(lo, 50000), ap(lo, 2049), 503},
		{ap(lo, 50001), ap(lo, 2049), 0},
		{ap(lan, 50001), ap(lo, 2049), 504},
		{ap(lo, 2049), ap(lo, 50000), 501},
	} {
		if got, err := uidInTable(tab, c.self, c.other); err != nil || got != c.uid {
			t.Errorf("%v->%v: %d, %v; want %d", c.self, c.other, got, err, c.uid)
		}
	}
	for _, c := range [][2]netip.AddrPort{
		{ap(lo, 50002), ap(lo, 2049)},
		{ap(lan, 50000), ap(lo, 2049)},
		{ap(lo, 50001), ap(lan, 2049)},
	} {
		if _, err := uidInTable(tab, c[0], c[1]); err == nil {
			t.Errorf("%v->%v, not in the table, was found", c[0], c[1])
		}
	}
	twice := append(append(tab[:len(tab)-24:len(tab)-24], conn(lo, 50000, lo, 2049, 0)...), make([]byte, 24)...)
	if uid, err := uidInTable(twice, ap(lo, 50000), ap(lo, 2049)); err == nil {
		t.Errorf("a connection listed twice with uids 503 and 0 was found as %d", uid)
	}
}

func TestTheSamePortFromTwoAddressesIsToldApart(t *testing.T) {
	var others []net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			others = append(others, n.IP.To4())
		}
	}
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c1, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	a1, err := ln.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer a1.Close()
	port := c1.LocalAddr().(*net.TCPAddr).Port
	var c2 net.Conn
	for _, ip := range others {
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: ip, Port: port}, Timeout: time.Second}
		if c2, err = d.Dial("tcp4", ln.Addr().String()); err == nil {
			break
		}
		t.Logf("the same port from %v: %v", ip, err)
	}
	if c2 == nil {
		t.Skip("no address of the Mac's but loopback reaches 127.0.0.1")
	}
	defer c2.Close()
	a2, err := ln.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	if a2.RemoteAddr().(*net.TCPAddr).Port != port {
		t.Fatalf("the second connection came from %v, not port %d", a2.RemoteAddr(), port)
	}
	for _, a := range []*net.TCPConn{a1, a2} {
		uid, err := peerUID(a.LocalAddr().(*net.TCPAddr), a.RemoteAddr().(*net.TCPAddr))
		if err != nil || uid != uint32(os.Getuid()) {
			t.Errorf("from %v: peerUID = %d, %v; want %d", a.RemoteAddr(), uid, err, os.Getuid())
		}
	}
}

func opened(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Fevom")
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	return dir
}

func checkStopped(t *testing.T, dir string) {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o555 {
		t.Errorf("folder %v, want 0555", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"README.txt"}) {
		t.Errorf("folder holds %v", names)
	}
	data, err := os.ReadFile(filepath.Join(dir, "README.txt"))
	if err != nil || string(data) != StoppedREADME {
		t.Errorf("README.txt %q %v", data, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "README.txt")); err != nil || fi.Mode().Perm() != 0o444 {
		t.Errorf("README.txt %v %v", fi.Mode(), err)
	}
	if !strings.HasSuffix(string(data), "[fev is STOPPED. Files are NOT available.]\n") {
		t.Errorf("the README does not end as expected: %q", data)
	}
}

func TestPrepareMakesTheStoppedFolder(t *testing.T) {
	dir := opened(t)
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	checkStopped(t, dir)
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	checkStopped(t, dir)
	os.Chmod(dir, 0o755)
	os.Chmod(filepath.Join(dir, "README.txt"), 0o644)
	os.WriteFile(filepath.Join(dir, "README.txt"), []byte("old\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".DS_Store"), nil, 0o644)
	os.Chmod(dir, 0o700)
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o755)
	os.Remove(filepath.Join(dir, ".DS_Store"))
	os.Chmod(dir, 0o555)
	checkStopped(t, dir)
	os.Chmod(dir, 0o755)
	os.Remove(filepath.Join(dir, "README.txt"))
	os.Symlink("/etc/hosts", filepath.Join(dir, "README.txt"))
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}
	checkStopped(t, dir)
}

func TestPrepareLeavesTheUsersFilesAlone(t *testing.T) {
	dir := opened(t)
	os.Mkdir(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o644)
	err := Prepare(dir)
	var ne *NotEmptyError
	if !errors.As(err, &ne) || !slices.Equal(ne.Names, []string{"notes.txt"}) {
		t.Fatalf("Prepare: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "notes.txt")); string(data) != "mine" {
		t.Fatal("the user's file changed")
	}
	if _, err := os.Stat(filepath.Join(dir, "README.txt")); err == nil {
		t.Fatal("a README was added beside the user's files")
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o755 {
		t.Fatalf("the user's folder's mode changed: %v", fi.Mode())
	}
	file := filepath.Join(t.TempDir(), "Fevom")
	os.WriteFile(file, nil, 0o644)
	if err := Prepare(file); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Fatalf("Prepare of a file: %v", err)
	}
	link := filepath.Join(t.TempDir(), "Fevom")
	os.Symlink(t.TempDir(), link)
	if err := Prepare(link); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Fatalf("Prepare of a link: %v", err)
	}
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "bv.nv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type pairOpener struct {
	serve func(c *net.UnixConn)
	err   error
	reqs  chan relay.Request
}

func (o *pairOpener) Open(ctx context.Context, req relay.Request) (*net.UnixConn, error) {
	if o.reqs != nil {
		o.reqs <- req
	}
	if o.err != nil {
		return nil, o.err
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	conn := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "pair")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			panic(err)
		}
		return c.(*net.UnixConn)
	}
	a, b := conn(fds[0]), conn(fds[1])
	go o.serve(b)
	return a, nil
}

func TestTheSocketCarriesEachConnectionToTheView(t *testing.T) {
	reqs := make(chan relay.Request, 4)
	o := &pairOpener{reqs: reqs, serve: func(c *net.UnixConn) {
		defer c.Close()
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				io.WriteString(c, "bye\n")
				c.CloseWrite()
				return
			}
			io.WriteString(c, strings.ToUpper(line))
		}
	}}
	s, err := Listen(filepath.Join(shortDir(t), "view.sock"), o, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := <-reqs; got != (relay.Request{Kind: relay.View, Target: relay.ViewNFS}) {
		t.Fatalf("relay request %+v", got)
	}
	io.WriteString(c, "compound\n")
	r := bufio.NewReader(c)
	if line, _ := r.ReadString('\n'); line != "COMPOUND\n" {
		t.Fatalf("got %q", line)
	}
	if s.Conns() != 1 {
		t.Fatalf("%d connections carried", s.Conns())
	}
	c.(*net.UnixConn).CloseWrite()
	rest, _ := io.ReadAll(r)
	if string(rest) != "bye\n" {
		t.Fatalf("after the half-close: %q", rest)
	}
	c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for s.Conns() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Conns() != 0 {
		t.Fatalf("%d connections left", s.Conns())
	}
}

func TestARefusedStreamClosesTheConnection(t *testing.T) {
	o := &pairOpener{err: &relay.Error{Code: relay.CodeRefused, Text: "the view is stopped"}}
	s, err := Listen(filepath.Join(shortDir(t), "view.sock"), o, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("read %d %v, want the end at once", n, err)
	}
}

func TestCloseEndsEveryConnectionAndTheSocket(t *testing.T) {
	held := make(chan struct{})
	o := &pairOpener{serve: func(c *net.UnixConn) {
		defer c.Close()
		io.Copy(io.Discard, c)
		close(held)
	}}
	s, err := Listen(filepath.Join(shortDir(t), "view.sock"), o, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "x")
	for s.Conns() != 1 {
		time.Sleep(time.Millisecond)
	}
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not end a carried connection")
	}
	<-held
	if _, err := os.Lstat(s.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the socket is still there: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the client's read after Close: %v", err)
	}
}

func TestListenGuardsItsDirectory(t *testing.T) {
	d := shortDir(t)
	os.Chmod(d, 0o750)
	if _, err := Listen(filepath.Join(d, "view.sock"), &pairOpener{}, nil); err == nil || !strings.Contains(err.Error(), "closed to others") {
		t.Fatalf("a group-readable directory: %v", err)
	}
	os.Chmod(d, 0o700)
	p := filepath.Join(d, "view.sock")
	os.WriteFile(p, []byte("x"), 0o600)
	if _, err := Listen(p, &pairOpener{}, nil); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("a file at the path: %v", err)
	}
	os.Remove(p)
	ln, _ := net.Listen("unix", p)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	s, err := Listen(p, &pairOpener{}, nil)
	if err != nil {
		t.Fatalf("over an old socket: %v", err)
	}
	s.Close()
}
