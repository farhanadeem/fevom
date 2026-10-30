package machnet

import (
	"bufio"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fev/guest/internal/streams"
	"fev/guest/machines"
	api "fev/proto/agent"
	"fev/proto/bridge"
)

type hostRig struct {
	t     *testing.T
	dir   string
	root  string
	run   string
	links string
	asked chan api.StreamRequest
	n     *Network
}

func newHostRig(t *testing.T) *hostRig {
	t.Helper()
	dir := t.TempDir()
	r := &hostRig{t: t, dir: dir, root: filepath.Join(dir, "m"), run: filepath.Join(dir, "run"), links: filepath.Join(dir, "links"), asked: make(chan api.StreamRequest, 8)}
	for _, d := range []string{"etc", "usr/local/bin"} {
		if err := os.MkdirAll(filepath.Join(r.root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(r.root, "etc/passwd"), []byte("root:x:0:0::/root:/bin/sh\ntester:x:1234:1234::/home/tester:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(r.run, 0o755)
	if err := EnsureLinks(r.links); err != nil {
		t.Fatal(err)
	}
	req := &streams.Requests{Ask: func(q api.StreamRequest) bool { r.asked <- q; return true }, Wait: 5 * time.Second}
	r.n = New(Options{Requests: req, Links: r.links})
	return r
}

func (r *hostRig) plug() (bridgeSock, agentSock *hostSocket) {
	r.t.Helper()
	b, a := r.n.hostSockets(machines.Plug{Name: "m", User: "tester", Root: r.root, Run: r.run})
	if b == nil || a == nil {
		r.t.Fatal("the sockets were not made")
	}
	r.t.Cleanup(func() { b.close(); a.close() })
	return b, a
}

func pair(t *testing.T, dir string) (client, server *net.UnixConn) {
	t.Helper()
	p := filepath.Join(dir, "pair.sock")
	os.Remove(p)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := l.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	return c, s
}

func (r *hostRig) answer(b *hostSocket, uid uint32, h *bridge.Header) error {
	r.t.Helper()
	c, s := pair(r.t, r.dir)
	go b.serveBridge(s, uid)
	if err := bridge.WriteHeader(c, h); err != nil {
		r.t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	return bridge.ReadReply(bufio.NewReader(c))
}

func TestTheBridgeAsksForAStreamNamingTheMachineAndTheUID(t *testing.T) {
	r := newHostRig(t)
	r.plug()
	sock := filepath.Join(r.run, MacSocket)
	if st, err := os.Stat(sock); err != nil || st.Mode().Perm() != 0o666 {
		t.Fatalf("socket: %v %v", st, err)
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h := &bridge.Header{Verb: bridge.VerbRun, Argv: []string{"uname", "-s"}}
	bridge.WriteHeader(c, h)
	c.Write([]byte("typed ahead"))
	var q api.StreamRequest
	select {
	case q = <-r.asked:
	case <-time.After(10 * time.Second):
		t.Fatal("no stream.request")
	}
	if q.Service != api.StreamMac || q.Machine != "m" || q.UID == nil || *q.UID != os.Getuid() {
		t.Fatalf("request %+v", q)
	}
	host, mine := net.Pipe()
	go r.n.o.Requests.Attach(q.Token, host)
	br := bufio.NewReader(mine)
	if line, _ := br.ReadString('\n'); line != "ok\n" {
		t.Fatalf("relay line %q", line)
	}
	got, _, err := bridge.ReadHeader(br)
	if err != nil || strings.Join(got.Argv, " ") != "uname -s" {
		t.Fatalf("header %+v %v", got, err)
	}
	ahead := make([]byte, len("typed ahead"))
	if _, err := io.ReadFull(br, ahead); err != nil || string(ahead) != "typed ahead" {
		t.Fatalf("what followed the header: %q %v", ahead, err)
	}
	bridge.WriteOK(mine)
	if err := bridge.ReadReply(bufio.NewReader(c)); err != nil {
		t.Fatalf("the helper's answer: %v", err)
	}
}

func TestTheSocketsServeRootAndTheDefaultUserOnly(t *testing.T) {
	r := newHostRig(t)
	b, a := r.plug()
	for uid, want := range map[uint32]bool{0: true, 1234: true, 1235: false, 65534: false} {
		if b.admits(uid) != want || a.admits(uid) != want {
			t.Errorf("uid %d admitted: %v", uid, !want)
		}
	}
	err := r.answer(b, 65534, &bridge.Header{Verb: bridge.VerbRun, Argv: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "uid 65534 may not use the Mac's commands from this machine; its socket serves root and tester") {
		t.Errorf("nobody: %v", err)
	}
	select {
	case q := <-r.asked:
		t.Errorf("a refused caller was asked for: %+v", q)
	default:
	}
	c, s := pair(t, r.dir)
	_ = s
	go func() {
		a.handleAs(s, 65534)
	}()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Errorf("the agent's refusal: %d %v", n, err)
	}
}

func TestLinkAndUnlinkAreRootsAndSharedByEveryMachine(t *testing.T) {
	r := newHostRig(t)
	b, _ := r.plug()
	if err := r.answer(b, 1234, &bridge.Header{Verb: bridge.VerbLink, Names: []string{"sw_vers"}}); err == nil || !strings.Contains(err.Error(), "needs root") {
		t.Errorf("the default user's link: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := r.answer(b, 0, &bridge.Header{Verb: bridge.VerbLink, Names: []string{"sw_vers", "pbcopy"}}); err != nil {
			t.Fatalf("link (%d): %v", i, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(r.links, "sw_vers")); err != nil || target != bridge.HelperPath {
		t.Errorf("sw_vers: %q %v", target, err)
	}
	if err := r.answer(b, 0, &bridge.Header{Verb: bridge.VerbUnlink, Names: []string{"open"}}); err == nil || !strings.Contains(err.Error(), "fev's own") {
		t.Errorf("unlink open: %v", err)
	}
	if err := r.answer(b, 0, &bridge.Header{Verb: bridge.VerbUnlink, Names: []string{"sw_vers"}}); err != nil {
		t.Errorf("unlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(r.links, "sw_vers")); !os.IsNotExist(err) {
		t.Errorf("sw_vers is still there: %v", err)
	}
	if err := r.answer(b, 0, &bridge.Header{Verb: bridge.VerbUnlink, Names: []string{"sw_vers"}}); err == nil || !strings.Contains(err.Error(), "not linked") {
		t.Errorf("unlink twice: %v", err)
	}
	os.WriteFile(filepath.Join(r.links, "mine"), []byte("x"), 0o755)
	if err := r.answer(b, 0, &bridge.Header{Verb: bridge.VerbLink, Names: []string{"mine"}}); err == nil {
		t.Error("a link was made over a file")
	}
	if err := r.answer(b, 0, &bridge.Header{Verb: bridge.VerbLink, Names: []string{"../escape"}}); err == nil {
		t.Error("a name with a / was linked")
	}
	select {
	case q := <-r.asked:
		t.Errorf("a link went to the daemon: %+v", q)
	default:
	}
}

func TestTheBuiltInLinksAreMadeAtEachStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "links")
	if err := EnsureLinks(dir); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "open"))
	os.WriteFile(filepath.Join(dir, "open"), []byte("not a link"), 0o755)
	os.Symlink(bridge.HelperPath, filepath.Join(dir, "sw_vers"))
	if err := EnsureLinks(dir); err != nil {
		t.Fatal(err)
	}
	for _, n := range append(append([]string{"sw_vers"}, bridge.Own...), bridge.Defaults...) {
		if target, err := os.Readlink(filepath.Join(dir, n)); err != nil || target != bridge.HelperPath {
			t.Errorf("%s: %q %v", n, target, err)
		}
	}
}

func TestMacIsLinkedFromUsrLocalBin(t *testing.T) {
	r := newHostRig(t)
	os.WriteFile(filepath.Join(r.root, "usr/local/bin/macctl"), []byte("the user's"), 0o755)
	r.plug()
	if target, err := os.Readlink(filepath.Join(r.root, "usr/local/bin/mac")); err != nil || target != bridge.BinDir+"/mac" {
		t.Errorf("mac: %q %v", target, err)
	}
	if b, _ := os.ReadFile(filepath.Join(r.root, "usr/local/bin/macctl")); string(b) != "the user's" {
		t.Errorf("the user's macctl was replaced: %q", b)
	}
	bare := filepath.Join(t.TempDir(), "bare")
	os.MkdirAll(bare, 0o755)
	if err := ensureLocalLinks(bare); err != nil {
		t.Errorf("a machine with no /usr/local/bin: %v", err)
	}
}

func TestARefusalIsSaidAndARenameFollowed(t *testing.T) {
	r := newHostRig(t)
	b, a := r.plug()
	r.n.byName["m"] = &member{name: "m", bridge: b, agent: a}
	r.n.Rename("m", "m2")
	c, err := net.Dial("unix", filepath.Join(r.run, MacSocket))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	bridge.WriteHeader(c, &bridge.Header{Verb: bridge.VerbRun, Argv: []string{"true"}})
	q := <-r.asked
	if q.Machine != "m2" {
		t.Errorf("after the rename the request names %q", q.Machine)
	}
	r.n.o.Requests.Refuse(q.Token, "the machine is isolated")
	c.SetDeadline(time.Now().Add(5 * time.Second))
	err = bridge.ReadReply(bufio.NewReader(c))
	if err == nil || !strings.Contains(err.Error(), "the Mac did not take the command: the machine is isolated") {
		t.Errorf("refusal: %v", err)
	}
}
