package forward

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"fev/host/docker/fakeguest"
	"fev/host/docker/vsock"
	api "fev/proto/daemon"
)

type world struct {
	t      *testing.T
	engine *fakeguest.Engine
	relay  *fakeguest.Relay
	table  *Table

	mu    sync.Mutex
	notes []string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bf.")
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, engine: fakeguest.NewEngine()}
	w.relay = &fakeguest.Relay{Engine: w.engine}
	path := filepath.Join(dir, "vsock_1026.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	go w.relay.Serve(l)
	w.table = New(nil, func(f api.Forward, removed bool) {
		note := fmt.Sprintf("%s %s", f.ID, f.State)
		if removed {
			note = f.ID + " removed"
		}
		w.mu.Lock()
		w.notes = append(w.notes, note)
		w.mu.Unlock()
	})
	w.table.SetOpener(vsock.Relay{Path: path})
	t.Cleanup(func() {
		w.table.Close()
		l.Close()
		w.engine.Close()
		os.RemoveAll(dir)
	})
	return w
}

func (w *world) notices() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.notes, ", ")
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	for range 50 {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := uint16(l.Addr().(*net.TCPAddr).Port)
		l.Close()
		free := true
		for _, addr := range []string{fmt.Sprintf("[::1]:%d", port), fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("0.0.0.0:%d", port), fmt.Sprintf("[::]:%d", port)} {
			if tl, err := listen(TCP, addr, false); err != nil {
				free = false
			} else {
				tl.(io.Closer).Close()
			}
			if ul, err := listen(UDP, addr, false); err != nil {
				free = false
			} else {
				ul.(io.Closer).Close()
			}
		}
		if free {
			return port
		}
	}
	t.Fatal("no free port")
	return 0
}

func (w *world) run(name, hostIP string, port uint16, proto string) string {
	w.t.Helper()
	id, err := w.engine.Run(fakeguest.Spec{Name: name, Ports: []fakeguest.Port{{HostIP: hostIP, HostPort: port, Proto: proto, ContainerPort: 80}}})
	if err != nil {
		w.t.Fatal(err)
	}
	return id
}

func guest(addr string, port uint16) netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr(addr), port)
}

func fetch(addr string) (string, error) {
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(body)), err
}

func (w *world) only() api.Forward {
	w.t.Helper()
	list := w.table.List()
	if len(list) != 1 {
		w.t.Fatalf("the table has %d rows: %+v", len(list), list)
	}
	return list[0]
}

func TestAPortPublishedOnLoopbackAnswersOnBothLoopbackAddressesOnly(t *testing.T) {
	w := newWorld(t)
	port := freePort(t)
	id := w.run("web", "", port, "tcp")
	w.table.Set(api.ForwardDocker, id, []Want{{TCP, port, guest("127.0.0.1", port), false}})

	f := w.only()
	want := api.Forward{
		ID: fmt.Sprintf("docker/%s/tcp/%d", id[:12], port), Source: "docker", Owner: id, Proto: "tcp",
		Mac: []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("[::1]:%d", port)}, Guest: fmt.Sprintf("127.0.0.1:%d", port), State: api.ForwardActive,
	}
	if fmt.Sprint(f) != fmt.Sprint(want) {
		t.Fatalf("got  %+v\nwant %+v", f, want)
	}
	for _, addr := range f.Mac {
		if body, err := fetch(addr); err != nil || body != "hello from web:80/tcp" {
			t.Fatalf("%s: %q, %v", addr, body, err)
		}
	}
	if n := w.relay.Opened(fmt.Sprintf("tcp 127.0.0.1:%d", port)); n != 2 {
		t.Fatalf("the relay was asked for the guest address %d times, want 2", n)
	}
	notFromTheLAN(t, w, port)

	conn, err := net.Dial("tcp4", f.Mac[0])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	w.table.Set(api.ForwardDocker, id, nil)
	if len(w.table.List()) != 0 {
		t.Fatalf("rows left: %+v", w.table.List())
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("a connection open across the removal still reads")
	}
	if _, err := fetch(f.Mac[0]); err == nil {
		t.Fatal("the port still answers after the forward was removed")
	}
	if got, want := w.notices(), f.ID+" active, "+f.ID+" removed"; got != want {
		t.Fatalf("notified %q, want %q", got, want)
	}
}

func TestAForwardedConnectionCarriesAHalfClose(t *testing.T) {
	w := newWorld(t)
	port := freePort(t)
	id := w.run("web", "", port, "tcp")
	w.table.Set(api.ForwardDocker, id, []Want{{TCP, port, guest("127.0.0.1", port), false}})
	conn, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte(i * 13)
	}
	go func() {
		fmt.Fprintf(conn, "POST /echo HTTP/1.1\r\nHost: x\r\nConnection: close\r\nContent-Length: %d\r\n\r\n", len(payload))
		conn.Write(payload)
		conn.(*net.TCPConn).CloseWrite()
	}()
	all, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < len(payload) || !bytes.Contains(all[:64], []byte("200 OK")) {
		t.Fatalf("read %d bytes for a body of %d: %q", len(all), len(payload), all[:min(len(all), 64)])
	}
}

func TestAPortNothingListensOnInTheGuestIsReset(t *testing.T) {
	w := newWorld(t)
	port := freePort(t)
	w.table.Set(api.ForwardDocker, "c1", []Want{{TCP, port, guest("127.0.0.1", port), false}})
	conn, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Read(make([]byte, 1))
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("the client read %v, want a reset", err)
	}
	w.table.SetOpener(nil)
	conn2, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	conn2.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = conn2.Read(make([]byte, 1)); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("with no guest the client read %v, want a reset", err)
	}
}

func reuseAddr(t *testing.T, l any) int {
	t.Helper()
	raw, err := l.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	value := -1
	raw.Control(func(fd uintptr) { value, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR) })
	return value
}

func TestForwardListenersBindWithoutAddressReuse(t *testing.T) {
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	plain, err := net.Listen("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	if reuseAddr(t, plain) == 0 {
		t.Fatal("Go no longer sets SO_REUSEADDR on a listener; the reasoning in this package needs another look")
	}
	plain.Close()
	ours, err := listen(TCP, addr, false)
	if err != nil {
		t.Fatal(err)
	}
	defer ours.(io.Closer).Close()
	if got := reuseAddr(t, ours); got != 0 {
		t.Fatalf("SO_REUSEADDR is %d on a forward listener", got)
	}
}

func TestTheBindConflictMatrix(t *testing.T) {
	type holder struct{ network, host string }
	holders := []holder{
		{"tcp4", "0.0.0.0"}, {"tcp4", "127.0.0.1"}, {"tcp6", "::"}, {"tcp6", "::1"}, {"tcp", "::"},
	}
	for _, h := range holders {
		port := freePort(t)
		other, err := net.Listen(h.network, net.JoinHostPort(h.host, fmt.Sprint(port)))
		if err != nil {
			t.Fatal(err)
		}
		for _, mine := range []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("[::1]:%d", port)} {
			v4 := strings.HasPrefix(mine, "127.")
			overlaps := (v4 && (h.host == "0.0.0.0" || h.host == "127.0.0.1" || (h.network == "tcp" && h.host == "::"))) ||
				(!v4 && (h.host == "::" || h.host == "::1"))
			l, err := listen(TCP, mine, false)
			if l != nil {
				l.(io.Closer).Close()
			}
			if overlaps && !errors.Is(err, syscall.EADDRINUSE) {
				t.Errorf("another program on %s %s: binding %s without reuse gave %v, want address in use", h.network, h.host, mine, err)
			}
			if !overlaps && err != nil {
				t.Errorf("another program on %s %s: binding %s without reuse gave %v, want success", h.network, h.host, mine, err)
			}
			l, err = listen(TCP, mine, true)
			if l != nil {
				l.(io.Closer).Close()
			}
			t.Logf("holder %-4s %-9s  bind %-16s  without reuse: %-8v with reuse: %v", h.network, h.host, mine, overlaps, map[bool]string{true: "ok", false: "in use"}[err == nil])
		}
		other.Close()
	}
}

func TestAPortAnotherProgramHasIsAConflict(t *testing.T) {
	for _, tc := range []struct{ name, network, host string }{
		{"on the wildcard address", "tcp4", "0.0.0.0"},
		{"on loopback", "tcp4", "127.0.0.1"},
		{"on the IPv6 loopback only", "tcp6", "::1"},
		{"on the IPv6 wildcard", "tcp6", "::"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			port := freePort(t)
			other, err := net.Listen(tc.network, net.JoinHostPort(tc.host, fmt.Sprint(port)))
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			go http.Serve(other, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "the other program") }))

			id := w.run("web", "", port, "tcp")
			w.table.Set(api.ForwardDocker, id, []Want{{TCP, port, guest("127.0.0.1", port), false}})
			f := w.only()
			if f.State != api.ForwardConflict || !strings.Contains(f.Error, fmt.Sprintf("port %d/tcp on the Mac is in use by another program", port)) {
				t.Fatalf("%+v", f)
			}
			for _, addr := range []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("[::1]:%d", port)} {
				body, err := fetch(addr)
				if err == nil && body != "the other program" {
					t.Fatalf("%s is answered by %q", addr, body)
				}
			}
			if n := w.relay.Opened(fmt.Sprintf("tcp 127.0.0.1:%d", port)); n != 0 {
				t.Fatal("connections reached the guest")
			}
			w.table.Set(api.ForwardDocker, id, []Want{{TCP, port, guest("127.0.0.1", port), false}})
			if got := w.notices(); got != f.ID+" conflict" {
				t.Fatalf("notified %q", got)
			}
		})
	}
}

func TestAConflictEndsWhenThePortIsFreeAndAskedForAgain(t *testing.T) {
	w := newWorld(t)
	port := freePort(t)
	other, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	id := w.run("web", "", port, "tcp")
	want := []Want{{TCP, port, guest("127.0.0.1", port), false}}
	w.table.Set(api.ForwardDocker, id, want)
	f := w.only()
	if f.State != api.ForwardConflict {
		t.Fatalf("%+v", f)
	}
	other.Close()
	if w.only().State != api.ForwardConflict {
		t.Fatal("the row changed by itself")
	}
	w.table.Set(api.ForwardDocker, id, want)
	if f := w.only(); f.State != api.ForwardActive || f.Error != "" {
		t.Fatalf("after the port was freed: %+v", f)
	}
	if body, err := fetch(fmt.Sprintf("127.0.0.1:%d", port)); err != nil || body != "hello from web:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
	if got := w.notices(); got != f.ID+" conflict, "+f.ID+" active" {
		t.Fatalf("notified %q", got)
	}
}

func TestRetryGivesAForwardInConflictItsPortOnceItIsFree(t *testing.T) {
	w := newWorld(t)
	taken, free := freePort(t), freePort(t)
	other, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", taken))
	if err != nil {
		t.Fatal(err)
	}
	blocked := w.run("web", "", taken, "tcp")
	fine := w.run("api", "", free, "tcp")
	w.table.Set(api.ForwardDocker, blocked, []Want{{TCP, taken, guest("127.0.0.1", taken), false}})
	w.table.Set(api.ForwardDocker, fine, []Want{{TCP, free, guest("127.0.0.1", free), false}})
	state := func(owner string) api.Forward {
		for _, f := range w.table.List() {
			if f.Owner == owner {
				return f
			}
		}
		t.Fatalf("no forward of %s", owner)
		return api.Forward{}
	}
	conflict, active := state(blocked), state(fine)
	if conflict.State != api.ForwardConflict || active.State != api.ForwardActive {
		t.Fatalf("%+v, %+v", conflict, active)
	}
	got, err := w.table.Retry("")
	if err != nil || len(got) != 1 || got[0].ID != conflict.ID || got[0].State != api.ForwardConflict || got[0].Error == "" {
		t.Fatalf("retry with the port still taken: %+v, %v", got, err)
	}
	got, err = w.table.Retry(active.ID)
	if err != nil || len(got) != 1 || got[0].ID != active.ID || got[0].State != api.ForwardActive {
		t.Fatalf("retry of a forward that is not in conflict: %+v, %v", got, err)
	}
	if body, err := fetch(fmt.Sprintf("127.0.0.1:%d", free)); err != nil || body != "hello from api:80/tcp" {
		t.Fatalf("the forward that was fine: %q, %v", body, err)
	}
	if _, err := w.table.Retry("docker/nobody/tcp/1"); !errors.Is(err, ErrNoForward) {
		t.Fatalf("retry of an id nobody has: %v", err)
	}
	other.Close()
	if state(blocked).State != api.ForwardConflict {
		t.Fatal("the row changed by itself")
	}
	got, err = w.table.Retry(conflict.ID)
	if err != nil || len(got) != 1 || got[0].State != api.ForwardActive || got[0].Error != "" {
		t.Fatalf("retry after the port was freed: %+v, %v", got, err)
	}
	if body, err := fetch(fmt.Sprintf("127.0.0.1:%d", taken)); err != nil || body != "hello from web:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
	if got, err := w.table.Retry(""); err != nil || len(got) != 0 {
		t.Fatalf("retry with nothing in conflict: %+v, %v", got, err)
	}
	if notices := w.notices(); strings.Count(notices, conflict.ID+" conflict") != 1 || strings.Count(notices, conflict.ID+" active") != 1 {
		t.Fatalf("notified %q", notices)
	}
}

func TestTwoForwardsClaimingOneMacPort(t *testing.T) {
	w := newWorld(t)
	port := freePort(t)
	a := w.run("a", "127.0.0.1", port, "tcp")
	b := w.run("b", "::1", port, "tcp")
	w.table.Set(api.ForwardDocker, a, []Want{{TCP, port, guest("127.0.0.1", port), false}})
	w.table.Set(api.ForwardDocker, b, []Want{{TCP, port, guest("::1", port), false}})

	list := w.table.List()
	byOwner := map[string]api.Forward{}
	for _, f := range list {
		byOwner[f.Owner] = f
	}
	if len(list) != 2 || byOwner[a].State != api.ForwardActive || byOwner[b].State != api.ForwardConflict {
		t.Fatalf("%+v", list)
	}
	if !strings.Contains(byOwner[b].Error, "already forwarded for docker "+a[:12]) {
		t.Fatalf("the second forward's error: %q", byOwner[b].Error)
	}
	if body, _ := fetch(fmt.Sprintf("127.0.0.1:%d", port)); body != "hello from a:80/tcp" {
		t.Fatalf("the port leads to %q", body)
	}
	w.table.Set(api.ForwardDocker, a, nil)
	if f := w.only(); f.Owner != b || f.State != api.ForwardActive || f.Guest != fmt.Sprintf("[::1]:%d", port) {
		t.Fatalf("%+v", f)
	}
	if body, err := fetch(fmt.Sprintf("127.0.0.1:%d", port)); err != nil || body != "hello from b:80/tcp" {
		t.Fatalf("after the takeover the port leads to %q, %v", body, err)
	}
	want := fmt.Sprintf("%[1]s active, %[2]s conflict, %[1]s removed, %[2]s active", byOwner[a].ID, byOwner[b].ID)
	if got := w.notices(); got != want {
		t.Fatalf("notified\n  %s\nwant\n  %s", got, want)
	}
}

func TestAPortReleasedAMomentAgoCanBeBoundAgain(t *testing.T) {
	w := newWorld(t)
	port := freePort(t)
	id := w.run("web", "", port, "tcp")
	want := []Want{{TCP, port, guest("127.0.0.1", port), false}}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	w.table.Set(api.ForwardDocker, id, want)
	if body, err := fetch(addr); err != nil || body != "hello from web:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
	w.table.Set(api.ForwardDocker, id, nil)
	strict, err := listen(TCP, addr, false)
	if err == nil {
		strict.(io.Closer).Close()
		t.Fatal("a bind without reuse succeeded right after the release; the reuse window may no longer be needed")
	} else if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatal(err)
	}
	w.table.Set(api.ForwardDocker, id, want)
	if f := w.only(); f.State != api.ForwardActive {
		t.Fatalf("bound again: %+v", f)
	}
	if body, err := fetch(addr); err != nil || body != "hello from web:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}

	w.table.Set(api.ForwardDocker, id, nil)
	w.table.mu.Lock()
	if _, ok := w.table.released[key{TCP, port}]; !ok {
		t.Error("the release was not noted")
	}
	now := time.Now().Add(reuseWindow + time.Second)
	w.table.now = func() time.Time { return now }
	w.table.mu.Unlock()
	other, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		t.Skipf("cannot play the other program: %v", err)
	}
	defer other.Close()
	w.table.Set(api.ForwardDocker, id, want)
	if f := w.only(); f.State != api.ForwardConflict {
		t.Fatalf("after the window, with another program on the port: %+v", f)
	}
	w.table.mu.Lock()
	if len(w.table.released) != 0 {
		t.Errorf("old releases are kept: %v", w.table.released)
	}
	w.table.mu.Unlock()
}

func lowPort(t *testing.T) uint16 {
	t.Helper()
	for port := uint16(1011); port > 900; port-- {
		ok := true
		addrs, _ := addrsFor(key{TCP, port}, true)
		for _, addr := range addrs {
			for _, proto := range []string{TCP, UDP} {
				l, err := listen(proto, addr, false)
				if err != nil {
					ok = false
					continue
				}
				l.(io.Closer).Close()
			}
		}
		if ok {
			return port
		}
	}
	t.Skip("no free port below 1024")
	return 0
}

func lanAddr() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	return ""
}

func TestLowPortsBindOnTheWildcardAddressOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can bind anything")
	}
	port := lowPort(t)
	for _, addr := range []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("[::1]:%d", port)} {
		for _, proto := range []string{TCP, UDP} {
			l, err := listen(proto, addr, false)
			if err == nil {
				l.(io.Closer).Close()
				t.Errorf("%s %s could be bound without privilege", proto, addr)
			} else if !errors.Is(err, syscall.EACCES) {
				t.Errorf("%s %s: %v, want permission denied", proto, addr, err)
			}
		}
	}
}

func TestALowPortIsServedToThisMacOnly(t *testing.T) {
	w := newWorld(t)
	port := lowPort(t)
	id := w.run("web", "", port, "tcp")
	w.table.Set(api.ForwardDocker, id, []Want{{TCP, port, guest("127.0.0.1", port), false}})
	f := w.only()
	if f.State != api.ForwardActive || fmt.Sprint(f.Mac) != fmt.Sprintf("[0.0.0.0:%d [::]:%d]", port, port) {
		t.Fatalf("%+v", f)
	}
	for _, addr := range []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("[::1]:%d", port)} {
		if body, err := fetch(addr); err != nil || body != "hello from web:80/tcp" {
			t.Fatalf("%s: %q, %v", addr, body, err)
		}
	}
	lan := lanAddr()
	if lan == "" {
		t.Skip("this Mac has no address but loopback; the refusal of other peers is not exercised")
	}
	before := w.relay.Opened(fmt.Sprintf("tcp 127.0.0.1:%d", port))
	if body, err := fetch(fmt.Sprintf("%s:%d", lan, port)); err == nil {
		t.Fatalf("a peer at %s was served %q", lan, body)
	}
	if after := w.relay.Opened(fmt.Sprintf("tcp 127.0.0.1:%d", port)); after != before {
		t.Fatal("a connection from off the Mac reached the guest")
	}
}

func udpAsk(t *testing.T, conn net.Conn, msg string) string {
	t.Helper()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no answer to %q: %v", msg, err)
	}
	return string(buf[:n])
}

func TestUDPIsForwardedPerClientAndAnswersComeBack(t *testing.T) {
	w := newWorld(t)
	port := freePort(t)
	id := w.run("dns", "", port, "udp")
	w.table.Set(api.ForwardDocker, id, []Want{{UDP, port, guest("127.0.0.1", port), false}})
	if f := w.only(); f.State != api.ForwardActive || f.Proto != UDP {
		t.Fatalf("%+v", f)
	}
	line := fmt.Sprintf("udp 127.0.0.1:%d", port)
	one, _ := net.Dial("udp4", fmt.Sprintf("127.0.0.1:%d", port))
	defer one.Close()
	two, _ := net.Dial("udp6", fmt.Sprintf("[::1]:%d", port))
	defer two.Close()
	for i := range 3 {
		if got := udpAsk(t, one, fmt.Sprint("one ", i)); got != fmt.Sprint("dns:80/udp one ", i) {
			t.Fatalf("got %q", got)
		}
	}
	if got := udpAsk(t, two, "two"); got != "dns:80/udp two" {
		t.Fatalf("got %q", got)
	}
	big := strings.Repeat("q", 1400)
	if got := udpAsk(t, one, big); got != "dns:80/udp "+big {
		t.Fatalf("a large datagram came back as %d bytes", len(got))
	}
	if n := w.relay.Opened(line); n != 2 {
		t.Fatalf("%d sessions were opened for two clients", n)
	}
	w.table.Set(api.ForwardDocker, id, nil)
	deadline := time.Now().Add(5 * time.Second)
	for w.relay.Streams.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d streams are still open after the forward was removed", w.relay.Streams.Load())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAUDPSessionLivesByWhetherItWasAnswered(t *testing.T) {
	w := newWorld(t)
	w.table.unreplied, w.table.assured = 80*time.Millisecond, 500*time.Millisecond
	port := freePort(t)
	id := w.run("dns", "", port, "udp")
	w.table.Set(api.ForwardDocker, id, []Want{{UDP, port, guest("127.0.0.1", port), false}})
	line := fmt.Sprintf("udp 127.0.0.1:%d", port)
	streams := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for w.relay.Streams.Load() != want {
			if time.Now().After(deadline) {
				t.Fatalf("%d streams are open, want %d", w.relay.Streams.Load(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}

	silent, _ := net.Dial("udp4", fmt.Sprintf("127.0.0.1:%d", port))
	defer silent.Close()
	answered, _ := net.Dial("udp4", fmt.Sprintf("127.0.0.1:%d", port))
	defer answered.Close()
	started := time.Now()
	silent.Write([]byte("no reply"))
	udpAsk(t, answered, "hello")
	streams(2)
	streams(1)
	if took := time.Since(started); took < 80*time.Millisecond || took > 400*time.Millisecond {
		t.Fatalf("the unanswered session ended after %v, want about 80 ms", took)
	}
	udpAsk(t, answered, "again")
	if n := w.relay.Opened(line); n != 2 {
		t.Fatalf("%d sessions were opened, want 2", n)
	}
	streams(0)
	if took := time.Since(started); took < 500*time.Millisecond {
		t.Fatalf("the answered session ended after %v, before its 500 ms", took)
	}
	if got := udpAsk(t, answered, "back"); got != "dns:80/udp back" {
		t.Fatalf("got %q", got)
	}
	if n := w.relay.Opened(line); n != 3 {
		t.Fatalf("%d sessions were opened, want 3", n)
	}
}

func TestAUDPPortAnotherProgramHasIsAConflict(t *testing.T) {
	w := newWorld(t)
	port := freePort(t)
	other, err := net.ListenPacket("udp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	w.table.Set(api.ForwardDocker, "c1", []Want{{UDP, port, guest("127.0.0.1", port), false}})
	if f := w.only(); f.State != api.ForwardConflict || !strings.Contains(f.Error, "in use by another program") {
		t.Fatalf("%+v", f)
	}
}

func TestAChangedGuestAddressNeedsNoNewListener(t *testing.T) {
	w := newWorld(t)
	port := freePort(t)
	id := w.run("web", "::1", port, "tcp")
	w.table.Set(api.ForwardDocker, id, []Want{{TCP, port, guest("127.0.0.1", port), false}})
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if _, err := fetch(addr); err == nil {
		t.Fatal("the guest's IPv4 loopback answered for a port published on its IPv6 one")
	}
	w.table.Set(api.ForwardDocker, id, []Want{{TCP, port, guest("::1", port), false}})
	if body, err := fetch(addr); err != nil || body != "hello from web:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
	if f := w.only(); f.Guest != fmt.Sprintf("[::1]:%d", port) || f.State != api.ForwardActive {
		t.Fatalf("%+v", f)
	}
}

func TestDropRemovesASourcesRowsAndWantsThatAreNotForwardsAreIgnored(t *testing.T) {
	w := newWorld(t)
	p1, p2 := freePort(t), freePort(t)
	w.table.Set(api.ForwardDocker, "c1", []Want{
		{TCP, p1, guest("127.0.0.1", p1), false},
		{TCP, p1, guest("127.0.0.1", p1), false},
		{"sctp", p2, guest("127.0.0.1", p2), false},
		{TCP, 0, guest("127.0.0.1", p2), false},
		{TCP, p2, netip.AddrPort{}, false},
	})
	w.table.Set("listen", "m1", []Want{{UDP, p2, guest("127.0.0.1", p2), false}})
	if list := w.table.List(); len(list) != 2 {
		t.Fatalf("%+v", list)
	}
	if owners := w.table.Owners(api.ForwardDocker); fmt.Sprint(owners) != "[c1]" {
		t.Fatalf("owners %v", owners)
	}
	w.table.Drop(api.ForwardDocker)
	if f := w.only(); f.Source != "listen" {
		t.Fatalf("%+v", f)
	}
}

func TestTheTableIsBounded(t *testing.T) {
	w := newWorld(t)
	port := freePort(t)
	want := []Want{{TCP, port, guest("127.0.0.1", port), false}}
	w.table.Set(api.ForwardDocker, "holder", want)
	w.table.mu.Lock()
	w.table.count = maxRows
	w.table.mu.Unlock()
	w.table.Set(api.ForwardDocker, "one-too-many", want)
	if f := w.only(); f.Owner != "holder" {
		t.Fatalf("%+v", f)
	}
	w.table.mu.Lock()
	w.table.count = 1
	w.table.mu.Unlock()
	w.table.Set(api.ForwardDocker, "next", want)
	if list := w.table.List(); len(list) != 2 {
		t.Fatalf("%+v", list)
	}
	w.table.Drop(api.ForwardDocker)
	w.table.mu.Lock()
	defer w.table.mu.Unlock()
	if w.table.count != 0 || len(w.table.rows) != 0 {
		t.Fatalf("after dropping everything: count %d, %d keys", w.table.count, len(w.table.rows))
	}
}

func notFromTheLAN(t *testing.T, w *world, port uint16) {
	t.Helper()
	lan := lanAddr()
	if lan == "" {
		t.Log("SKIPPED: this Mac has no address but loopback; that the port is closed to the LAN is not exercised")
		return
	}
	line := fmt.Sprintf("tcp 127.0.0.1:%d", port)
	before := w.relay.Opened(line)
	if body, err := fetch(fmt.Sprintf("%s:%d", lan, port)); err == nil {
		t.Fatalf("a peer at %s was served %q", lan, body)
	}
	if after := w.relay.Opened(line); after != before {
		t.Fatal("a connection to the LAN address reached the guest")
	}
}

func fromTheLAN(t *testing.T, port uint16, want string) {
	t.Helper()
	t.Run("from the LAN address", func(t *testing.T) {
		lan := lanAddr()
		if lan == "" {
			t.Skip("this Mac has no address but loopback; that the port answers the LAN is not exercised")
		}
		if body, err := fetch(fmt.Sprintf("%s:%d", lan, port)); err != nil || body != want {
			t.Fatalf("%s:%d: %q, %v", lan, port, body, err)
		}
	})
}

func wildcards(port uint16) string { return fmt.Sprintf("[0.0.0.0:%d [::]:%d]", port, port) }
func loopbacks(port uint16) string { return fmt.Sprintf("[127.0.0.1:%d [::1]:%d]", port, port) }

func TestAPortPublishedForEveryAddressAnswersOnLoopbackAndTheLAN(t *testing.T) {
	w := newWorld(t)
	port := freePort(t)
	id := w.run("web", "", port, "tcp")
	w.table.Set(api.ForwardDocker, id, []Want{{TCP, port, guest("127.0.0.1", port), true}})
	f := w.only()
	if f.State != api.ForwardActive || fmt.Sprint(f.Mac) != wildcards(port) {
		t.Fatalf("%+v", f)
	}
	for _, addr := range []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("[::1]:%d", port)} {
		if body, err := fetch(addr); err != nil || body != "hello from web:80/tcp" {
			t.Fatalf("%s: %q, %v", addr, body, err)
		}
	}
	fromTheLAN(t, port, "hello from web:80/tcp")
}

func TestWithLANExposureOffAPortPublishedForEveryAddressIsLoopbackOnly(t *testing.T) {
	w := newWorld(t)
	w.table.SetLAN(false)
	port := freePort(t)
	id := w.run("web", "", port, "tcp")
	w.table.Set(api.ForwardDocker, id, []Want{{TCP, port, guest("127.0.0.1", port), true}})
	f := w.only()
	if f.State != api.ForwardActive || fmt.Sprint(f.Mac) != loopbacks(port) {
		t.Fatalf("%+v", f)
	}
	if body, err := fetch(f.Mac[0]); err != nil || body != "hello from web:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
	notFromTheLAN(t, w, port)
}

func TestChangingLANExposureRebindsTheForwardsItChanges(t *testing.T) {
	w := newWorld(t)
	wide, narrow := freePort(t), freePort(t)
	a := w.run("wide", "", wide, "tcp")
	b := w.run("narrow", "127.0.0.1", narrow, "tcp")
	w.table.Set(api.ForwardDocker, a, []Want{{TCP, wide, guest("127.0.0.1", wide), true}})
	w.table.Set(api.ForwardDocker, b, []Want{{TCP, narrow, guest("127.0.0.1", narrow), false}})
	ida, idb := rowID(api.ForwardDocker, a, key{TCP, wide}), rowID(api.ForwardDocker, b, key{TCP, narrow})
	mac := func(id string) string {
		for _, f := range w.table.List() {
			if f.ID == id {
				if f.State != api.ForwardActive {
					t.Fatalf("%+v", f)
				}
				return fmt.Sprint(f.Mac)
			}
		}
		t.Fatalf("no row %s", id)
		return ""
	}
	for _, port := range []uint16{wide, narrow} {
		if _, err := fetch(fmt.Sprintf("127.0.0.1:%d", port)); err != nil {
			t.Fatal(err)
		}
	}
	open := func(port uint16) net.Conn {
		conn, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	onWide, onNarrow := open(wide), open(narrow)

	w.table.SetLAN(false)
	if mac(ida) != loopbacks(wide) || mac(idb) != loopbacks(narrow) {
		t.Fatalf("with the setting off: %+v", w.table.List())
	}
	if body, err := fetch(fmt.Sprintf("127.0.0.1:%d", wide)); err != nil || body != "hello from wide:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
	notFromTheLAN(t, w, wide)
	onWide.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, onWide); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("a connection through the forward that was rebound is still open")
	}
	w.table.SetLAN(false)

	w.table.SetLAN(true)
	if mac(ida) != wildcards(wide) || mac(idb) != loopbacks(narrow) {
		t.Fatalf("with the setting on again: %+v", w.table.List())
	}
	if body, err := fetch(fmt.Sprintf("[::1]:%d", wide)); err != nil || body != "hello from wide:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
	fromTheLAN(t, wide, "hello from wide:80/tcp")
	fmt.Fprintf(onNarrow, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	onNarrow.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := onNarrow.Read(make([]byte, 1)); err != nil {
		t.Fatalf("the connection through the loopback forward: %v", err)
	}
	if got, want := w.notices(), fmt.Sprintf("%[1]s active, %[2]s active, %[1]s active, %[1]s active", ida, idb); got != want {
		t.Fatalf("notified %q, want %q", got, want)
	}
}

func TestTheWidestPublishOfAPortWinsAndAChangeOfItRebinds(t *testing.T) {
	w := newWorld(t)
	port := freePort(t)
	narrow := Want{TCP, port, guest("127.0.0.1", port), false}
	wide := Want{TCP, port, guest("10.1.2.3", port), true}
	w.table.Set(api.ForwardDocker, "c1", []Want{narrow, wide})
	if f := w.only(); f.State != api.ForwardActive || fmt.Sprint(f.Mac) != wildcards(port) || f.Guest != fmt.Sprintf("127.0.0.1:%d", port) {
		t.Fatalf("%+v", f)
	}
	w.table.Set(api.ForwardDocker, "c1", []Want{narrow})
	if f := w.only(); f.State != api.ForwardActive || fmt.Sprint(f.Mac) != loopbacks(port) {
		t.Fatalf("asked for again on loopback: %+v", f)
	}
	w.table.Set(api.ForwardDocker, "c2", []Want{wide})
	waiting := func() api.Forward {
		for _, f := range w.table.List() {
			if f.Owner == "c2" {
				return f
			}
		}
		t.Fatal("no row for c2")
		return api.Forward{}
	}
	if f := waiting(); f.State != api.ForwardConflict || fmt.Sprint(f.Mac) != wildcards(port) {
		t.Fatalf("%+v", f)
	}
	w.table.SetLAN(false)
	if f := waiting(); f.State != api.ForwardConflict || fmt.Sprint(f.Mac) != loopbacks(port) {
		t.Fatalf("with the setting off: %+v", f)
	}
}

func v6only(t *testing.T, l any) int {
	t.Helper()
	raw, err := l.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	value := -1
	raw.Control(func(fd uintptr) { value, _ = unix.GetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY) })
	return value
}

func TestTheIPv6WildcardListenerIsIPv6Only(t *testing.T) {
	port := freePort(t)
	for _, proto := range []string{TCP, UDP} {
		six, err := listen(proto, fmt.Sprintf("[::]:%d", port), false)
		if err != nil {
			t.Fatal(err)
		}
		if got := v6only(t, six); got != 1 {
			t.Errorf("%s: IPV6_V6ONLY is %d on the IPv6 wildcard listener", proto, got)
		}
		four, err := listen(proto, fmt.Sprintf("0.0.0.0:%d", port), false)
		if err != nil {
			t.Errorf("%s: the IPv4 wildcard beside our IPv6 one: %v", proto, err)
		} else {
			four.(io.Closer).Close()
		}
		six.(io.Closer).Close()
	}
}

func TestTheWildcardBindConflictMatrix(t *testing.T) {
	type holder struct {
		host            string
		four, six, dual bool
	}
	holders := []holder{
		{"127.0.0.1", true, false, false}, {"0.0.0.0", true, false, false},
		{"::1", false, true, false}, {"::", false, true, false}, {"::", true, true, true},
	}
	if lan := lanAddr(); lan != "" {
		holders = append(holders, holder{lan, true, false, false})
	} else {
		t.Log("SKIPPED: this Mac has no address but loopback; a holder of one LAN address is not measured")
	}
	hold := func(proto string, h holder, port uint16, reuse bool) (io.Closer, error) {
		lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				value := 0
				if reuse {
					value = 1
				}
				unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, value)
			})
		}}
		family := "4"
		switch {
		case h.dual:
			family = ""
		case strings.Contains(h.host, ":"):
			family = "6"
		}
		addr := net.JoinHostPort(h.host, fmt.Sprint(port))
		if proto == TCP {
			return lc.Listen(t.Context(), TCP+family, addr)
		}
		return lc.ListenPacket(t.Context(), UDP+family, addr)
	}
	for _, proto := range []string{TCP, UDP} {
		for _, reuse := range []bool{false, true} {
			for _, h := range holders {
				port := freePort(t)
				other, err := hold(proto, h, port, reuse)
				if err != nil {
					t.Fatal(err)
				}
				for _, mine := range []struct {
					addr    string
					refused bool
				}{{fmt.Sprintf("0.0.0.0:%d", port), h.four}, {fmt.Sprintf("[::]:%d", port), h.six}} {
					l, err := listen(proto, mine.addr, false)
					if l != nil {
						l.(io.Closer).Close()
					}
					if mine.refused && !errors.Is(err, syscall.EADDRINUSE) {
						t.Errorf("%s: another program on %s (dual %v, reuse %v): binding %s without reuse gave %v, want address in use", proto, h.host, h.dual, reuse, mine.addr, err)
					}
					if !mine.refused && err != nil {
						t.Errorf("%s: another program on %s (reuse %v): binding %s without reuse gave %v, want success", proto, h.host, reuse, mine.addr, err)
					}
					l, err = listen(proto, mine.addr, true)
					if l != nil {
						l.(io.Closer).Close()
					}
					t.Logf("%s holder %-12s dual %-5v reuse %-5v  bind %-14s  without reuse refused: %-5v  with reuse: %s", proto, h.host, h.dual, reuse, mine.addr, mine.refused, map[bool]string{true: "ok", false: "in use"}[err == nil])
					if proto == TCP && ((h.host == "127.0.0.1" && strings.HasPrefix(mine.addr, "0.")) || (h.host == "::1" && strings.HasPrefix(mine.addr, "["))) {
						if err != nil {
							t.Errorf("%s: with reuse, the wildcard bind beside a loopback holder gave %v; listenAgain's probe may no longer be needed", proto, err)
						}
						probe, err := listen(proto, net.JoinHostPort(h.host, fmt.Sprint(port)), true)
						if probe != nil {
							probe.(io.Closer).Close()
						}
						if !errors.Is(err, syscall.EADDRINUSE) {
							t.Errorf("%s: with reuse, binding %s that another program holds gave %v, want address in use", proto, h.host, err)
						}
					}
				}
				other.Close()
			}
		}
	}
}

func TestAPortAnotherProgramHasIsAConflictForAWildcardForward(t *testing.T) {
	cases := []struct{ name, network, host string }{
		{"on loopback", "tcp4", "127.0.0.1"},
		{"on the IPv6 loopback", "tcp6", "::1"},
		{"on the wildcard address", "tcp4", "0.0.0.0"},
		{"on the IPv6 wildcard", "tcp6", "::"},
	}
	if lan := lanAddr(); lan != "" {
		cases = append(cases, struct{ name, network, host string }{"on the LAN address", "tcp4", lan})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			port := freePort(t)
			other, err := net.Listen(tc.network, net.JoinHostPort(tc.host, fmt.Sprint(port)))
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			go http.Serve(other, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "the other program") }))

			id := w.run("web", "", port, "tcp")
			w.table.Set(api.ForwardDocker, id, []Want{{TCP, port, guest("127.0.0.1", port), true}})
			f := w.only()
			if f.State != api.ForwardConflict || !strings.Contains(f.Error, fmt.Sprintf("port %d/tcp on the Mac is in use by another program", port)) || fmt.Sprint(f.Mac) != wildcards(port) {
				t.Fatalf("%+v", f)
			}
			addrs := []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("[::1]:%d", port)}
			if lan := lanAddr(); lan != "" {
				addrs = append(addrs, fmt.Sprintf("%s:%d", lan, port))
			}
			for _, addr := range addrs {
				if body, err := fetch(addr); err == nil && body != "the other program" {
					t.Fatalf("%s is answered by %q", addr, body)
				}
			}
			for _, addr := range f.Mac {
				l, err := listen(TCP, addr, false)
				if err == nil {
					l.(io.Closer).Close()
				} else if ap := netip.MustParseAddrPort(addr); ap.Addr().Is4() == (tc.network == "tcp6") {
					t.Fatalf("%s is still bound in the family the other program is not in: %v", addr, err)
				}
			}
			reach := map[string]string{"0.0.0.0": "127.0.0.1", "::": "::1"}[tc.host]
			if reach == "" {
				reach = tc.host
			}
			if body, err := fetch(net.JoinHostPort(reach, fmt.Sprint(port))); err != nil || body != "the other program" {
				t.Fatalf("the other program no longer answers at %s: %q, %v", reach, body, err)
			}
			if n := w.relay.Opened(fmt.Sprintf("tcp 127.0.0.1:%d", port)); n != 0 {
				t.Fatal("connections reached the guest")
			}
		})
	}
}

func TestAWildcardForwardBoundAgainStillSeesALoopbackListener(t *testing.T) {
	w := newWorld(t)
	port := freePort(t)
	id := w.run("web", "", port, "tcp")
	want := []Want{{TCP, port, guest("127.0.0.1", port), true}}
	w.table.Set(api.ForwardDocker, id, want)
	if body, err := fetch(fmt.Sprintf("127.0.0.1:%d", port)); err != nil || body != "hello from web:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
	held, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	w.table.Set(api.ForwardDocker, id, nil)
	if l, err := listen(TCP, fmt.Sprintf("0.0.0.0:%d", port), false); err == nil {
		l.(io.Closer).Close()
		t.Fatal("a wildcard bind without reuse succeeded right after the release; the reuse window may no longer be needed")
	}
	w.table.Set(api.ForwardDocker, id, want)
	if f := w.only(); f.State != api.ForwardActive || fmt.Sprint(f.Mac) != wildcards(port) {
		t.Fatalf("bound again: %+v", f)
	}
	if body, err := fetch(fmt.Sprintf("[::1]:%d", port)); err != nil || body != "hello from web:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
	fromTheLAN(t, port, "hello from web:80/tcp")
	w.table.Set(api.ForwardDocker, id, nil)

	other, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("cannot play the other program: %v", err)
	}
	defer other.Close()
	go http.Serve(other, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "the other program") }))
	w.table.Set(api.ForwardDocker, id, want)
	f := w.only()
	if f.State != api.ForwardConflict || !strings.Contains(f.Error, "in use by another program") {
		t.Fatalf("with another program on loopback in the window: %+v", f)
	}
	if body, err := fetch(fmt.Sprintf("127.0.0.1:%d", port)); err != nil || body != "the other program" {
		t.Fatalf("the other program: %q, %v", body, err)
	}
	if _, err := fetch(fmt.Sprintf("[::1]:%d", port)); err == nil {
		t.Fatal("the IPv6 half of a forward in conflict is bound")
	}
	if lan := lanAddr(); lan != "" {
		if body, err := fetch(fmt.Sprintf("%s:%d", lan, port)); err == nil {
			t.Fatalf("the LAN half of a forward in conflict serves %q", body)
		}
	}
}

func TestALowPortPublishedForEveryAddressServesTheLAN(t *testing.T) {
	w := newWorld(t)
	port := lowPort(t)
	id := w.run("web", "", port, "tcp")
	w.table.Set(api.ForwardDocker, id, []Want{{TCP, port, guest("127.0.0.1", port), true}})
	if f := w.only(); f.State != api.ForwardActive || fmt.Sprint(f.Mac) != wildcards(port) {
		t.Fatalf("%+v", f)
	}
	if body, err := fetch(fmt.Sprintf("127.0.0.1:%d", port)); err != nil || body != "hello from web:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
	fromTheLAN(t, port, "hello from web:80/tcp")

	w.table.SetLAN(false)
	if f := w.only(); f.State != api.ForwardActive || fmt.Sprint(f.Mac) != wildcards(port) {
		t.Fatalf("with the setting off: %+v", f)
	}
	if body, err := fetch(fmt.Sprintf("[::1]:%d", port)); err != nil || body != "hello from web:80/tcp" {
		t.Fatalf("with the setting off: %q, %v", body, err)
	}
	notFromTheLAN(t, w, port)
}

func TestUDPFollowsTheSameAddressRule(t *testing.T) {
	w := newWorld(t)
	wide, narrow := freePort(t), freePort(t)
	w.run("dns", "", wide, "udp")
	w.run("dns2", "127.0.0.1", narrow, "udp")
	w.table.Set(api.ForwardDocker, "c1", []Want{{UDP, wide, guest("127.0.0.1", wide), true}})
	w.table.Set(api.ForwardDocker, "c2", []Want{{UDP, narrow, guest("127.0.0.1", narrow), false}})
	list := w.table.List()
	if len(list) != 2 || fmt.Sprint(list[0].Mac) != wildcards(wide) || fmt.Sprint(list[1].Mac) != loopbacks(narrow) || list[0].State != api.ForwardActive || list[1].State != api.ForwardActive {
		t.Fatalf("%+v", list)
	}
	for _, target := range []string{fmt.Sprintf("127.0.0.1:%d", wide), fmt.Sprintf("[::1]:%d", wide), fmt.Sprintf("127.0.0.1:%d", narrow)} {
		conn, err := net.Dial("udp", target)
		if err != nil {
			t.Fatal(err)
		}
		if got := udpAsk(t, conn, "q"); !strings.HasSuffix(got, ":80/udp q") {
			t.Fatalf("%s: %q", target, got)
		}
		conn.Close()
	}
	silent := func(target string) {
		t.Helper()
		conn, err := net.Dial("udp4", target)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.Write([]byte("q"))
		conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
		if n, err := conn.Read(make([]byte, 64)); err == nil {
			t.Fatalf("%s answered %d bytes", target, n)
		}
	}
	t.Run("from the LAN address", func(t *testing.T) {
		lan := lanAddr()
		if lan == "" {
			t.Skip("this Mac has no address but loopback; UDP at the LAN address is not exercised")
		}
		conn, err := net.Dial("udp4", net.JoinHostPort(lan, fmt.Sprint(wide)))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if got := udpAsk(t, conn, "lan"); got != "dns:80/udp lan" {
			t.Fatalf("%q", got)
		}
		silent(fmt.Sprintf("%s:%d", lan, narrow))
		w.table.SetLAN(false)
		defer w.table.SetLAN(true)
		if f := w.table.List()[0]; f.State != api.ForwardActive || fmt.Sprint(f.Mac) != loopbacks(wide) {
			t.Fatalf("with the setting off: %+v", f)
		}
		silent(fmt.Sprintf("%s:%d", lan, wide))
	})
	port := freePort(t)
	other, err := net.ListenPacket("udp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	w.table.Set(api.ForwardDocker, "c3", []Want{{UDP, port, guest("127.0.0.1", port), true}})
	for _, f := range w.table.List() {
		if f.Owner == "c3" && (f.State != api.ForwardConflict || !strings.Contains(f.Error, "in use by another program")) {
			t.Fatalf("%+v", f)
		}
	}
	if six, err := listen(UDP, fmt.Sprintf("[::]:%d", port), false); err != nil {
		t.Fatalf("the IPv6 half of a UDP forward in conflict is bound: %v", err)
	} else {
		six.(io.Closer).Close()
	}
}

func TestNoOtherProgramCanTakeTheLoopbackHalfOfAWildcardForward(t *testing.T) {
	withReuse := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) { unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1) })
	}}
	port := freePort(t)
	lone, err := listen(TCP, fmt.Sprintf("0.0.0.0:%d", port), false)
	if err != nil {
		t.Fatal(err)
	}
	thief, err := withReuse.Listen(t.Context(), "tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("macOS now refuses a loopback bind beside another socket's wildcard listener (%v); the loopback listeners of a wildcard forward may no longer be needed", err)
	}
	thief.Close()
	lone.(io.Closer).Close()

	for _, proto := range []string{TCP, UDP} {
		t.Run(proto, func(t *testing.T) {
			w := newWorld(t)
			port := freePort(t)
			id := w.run("web", "", port, proto)
			w.table.Set(api.ForwardDocker, id, []Want{{proto, port, guest("127.0.0.1", port), true}})
			if f := w.only(); f.State != api.ForwardActive || fmt.Sprint(f.Mac) != wildcards(port) {
				t.Fatalf("%+v", f)
			}
			for _, addr := range []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("[::1]:%d", port)} {
				family := map[bool]string{true: "4", false: "6"}[netip.MustParseAddrPort(addr).Addr().Is4()]
				var other io.Closer
				var err error
				if proto == TCP {
					other, err = withReuse.Listen(t.Context(), TCP+family, addr)
				} else {
					other, err = withReuse.ListenPacket(t.Context(), UDP+family, addr)
				}
				if err == nil {
					other.Close()
					t.Errorf("another program bound %s/%s beside the forward", addr, proto)
				} else if !errors.Is(err, syscall.EADDRINUSE) {
					t.Errorf("%s/%s: %v", addr, proto, err)
				}
			}
			if proto == TCP {
				for _, addr := range []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("[::1]:%d", port)} {
					if body, err := fetch(addr); err != nil || body != "hello from web:80/tcp" {
						t.Fatalf("%s: %q, %v", addr, body, err)
					}
				}
				fromTheLAN(t, port, "hello from web:80/tcp")
			} else {
				for _, addr := range []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("[::1]:%d", port)} {
					conn, err := net.Dial("udp", addr)
					if err != nil {
						t.Fatal(err)
					}
					if got := udpAsk(t, conn, "q"); got != "web:80/udp q" {
						t.Fatalf("%s: %q", addr, got)
					}
					conn.Close()
				}
			}
			w.table.Set(api.ForwardDocker, id, nil)
			if proto == UDP {
				for _, addr := range []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("[::1]:%d", port), fmt.Sprintf("0.0.0.0:%d", port), fmt.Sprintf("[::]:%d", port)} {
					l, err := listen(UDP, addr, false)
					if err != nil {
						t.Fatalf("%s is still held after the forward was removed: %v", addr, err)
					}
					l.(io.Closer).Close()
				}
			}
		})
	}
}

func macAddrs(six bool) []netip.Addr {
	var out []netip.Addr
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		ip = ip.Unmap()
		if !ok || ip.Is6() != six || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
			continue
		}
		if ip.IsLoopback() {
			out = append([]netip.Addr{ip}, out...)
		} else {
			out = append(out, ip)
		}
	}
	return out
}

func askFrom(network string, from netip.Addr, to netip.AddrPort, wait time.Duration) (answered netip.Addr, text string, ok bool) {
	client, err := net.ListenUDP(network, net.UDPAddrFromAddrPort(netip.AddrPortFrom(from, 0)))
	if err != nil {
		return netip.Addr{}, "", false
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(wait))
	if _, err := client.WriteToUDPAddrPort([]byte("q"), to); err != nil {
		return netip.Addr{}, "", false
	}
	buf := make([]byte, 64)
	n, peer, err := client.ReadFromUDPAddrPort(buf)
	if err != nil {
		return netip.Addr{}, "", false
	}
	return peer.Addr().Unmap(), string(buf[:n]), true
}

func TestAWildcardUDPForwardAnswersFromTheAddressSentTo(t *testing.T) {
	w := newWorld(t)
	port := freePort(t)
	w.run("dns", "", port, "udp")
	w.table.Set(api.ForwardDocker, "c1", []Want{{UDP, port, guest("127.0.0.1", port), true}})
	if f := w.only(); f.State != api.ForwardActive || fmt.Sprint(f.Mac) != wildcards(port) {
		t.Fatalf("%+v", f)
	}
	pairs, wrongByItself, between := 0, 0, 0
	for _, six := range []bool{false, true} {
		addrs := macAddrs(six)
		network, wild := "udp4", "0.0.0.0:0"
		if six {
			network, wild = "udp6", "[::]:0"
		}
		plain, err := net.ListenPacket(network, wild)
		if err != nil {
			t.Fatal(err)
		}
		defer plain.Close()
		go func() {
			buf := make([]byte, 64)
			for {
				n, from, err := plain.ReadFrom(buf)
				if err != nil {
					return
				}
				plain.WriteTo(buf[:n], from)
			}
		}()
		plainPort := uint16(plain.LocalAddr().(*net.UDPAddr).Port)
		for _, from := range addrs {
			for _, to := range addrs {
				if from == to || to.IsLoopback() {
					continue
				}
				byItself, _, carried := askFrom(network, from, netip.AddrPortFrom(to, plainPort), 200*time.Millisecond)
				if !carried {
					continue
				}
				answered, text, ok := askFrom(network, from, netip.AddrPortFrom(to, port), 5*time.Second)
				if !ok {
					t.Errorf("from %s to %s: no answer", from, to)
					continue
				}
				pairs++
				if byItself != to {
					wrongByItself++
				}
				if !from.IsLoopback() {
					between++
				}
				if answered != to || text != "dns:80/udp q" {
					t.Errorf("a datagram from %s to %s was answered from %s (%q); a plain wildcard socket answers from %s", from, to, answered, text, byItself)
				}
			}
		}
	}
	if pairs == 0 {
		t.Skip("this Mac has no address but loopback: there are no two addresses to tell apart")
	}
	t.Logf("%d pairs of addresses answered from the address sent to; a plain wildcard socket gets %d of them wrong; %d are between two addresses that are not loopback", pairs, wrongByItself, between)
}

func TestTheReplyAddressControlMessage(t *testing.T) {
	if leaveFrom(netip.Addr{}) != nil || leaveFrom(netip.IPv4Unspecified()) != nil || leaveFrom(netip.MustParseAddr("224.0.0.251")) != nil {
		t.Fatal("an address nothing can leave from was given a control message")
	}
	if got := arrivedAt(nil); got.IsValid() {
		t.Fatalf("no control message read as %s", got)
	}
	if got := arrivedAt([]byte{1, 2, 3}); got.IsValid() {
		t.Fatalf("a broken control message read as %s", got)
	}
	four := leaveFrom(netip.MustParseAddr("192.0.2.7"))
	copy(four[len(four)-4:], four[len(four)-8:len(four)-4])
	if got := arrivedAt(four); got != netip.MustParseAddr("192.0.2.7") {
		t.Fatalf("IPv4: %s", got)
	}
	if got := arrivedAt(leaveFrom(netip.MustParseAddr("2001:db8::7"))); got != netip.MustParseAddr("2001:db8::7") {
		t.Fatalf("IPv6: %s", got)
	}
}
