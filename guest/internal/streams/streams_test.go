package streams

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"fev/proto/relay"
)

func pair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	path := filepath.Join(shortDir(t), "s")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	a, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a.(*net.UnixConn), b.(*net.UnixConn)
}

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bst")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func serve(t *testing.T, r *Relay) *net.UnixConn {
	t.Helper()
	host, guest := pair(t)
	if r.Local == nil {
		r.Local = func(a netip.Addr) bool { return a.IsLoopback() }
	}
	r.Logf = t.Logf
	go r.ServeConn(guest)
	return host
}

func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestRelayTCPCarriesBytesAndHalfClose(t *testing.T) {
	l := listenTCP(t)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		data, _ := io.ReadAll(c)
		c.Write(bytes.ToUpper(data))
	}()
	host := serve(t, &Relay{})
	if err := relay.Open(host, relay.Request{Kind: relay.TCP, Target: l.Addr().String()}); err != nil {
		t.Fatal(err)
	}
	host.Write([]byte("hello "))
	host.Write([]byte("relay"))
	host.CloseWrite()
	got, err := io.ReadAll(host)
	if err != nil || string(got) != "HELLO RELAY" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestTheSourcePortOfARelayStreamCanStillBePublished(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the bind rule under test is Linux's")
	}
	publish := func(port int) error {
		lc := net.ListenConfig{Control: reuseAddr}
		l, err := lc.Listen(t.Context(), "tcp4", "0.0.0.0:"+strconv.Itoa(port))
		if err == nil {
			l.Close()
		}
		return err
	}
	l := listenTCP(t)
	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	plain, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	defer (<-accepted).Close()
	if err := publish(plain.LocalAddr().(*net.TCPAddr).Port); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("binding the source port of a plain connection: %v; the rule this test rests on has changed", err)
	}

	host := serve(t, &Relay{})
	if err := relay.Open(host, relay.Request{Kind: relay.TCP, Target: l.Addr().String()}); err != nil {
		t.Fatal(err)
	}
	peer := <-accepted
	port := peer.RemoteAddr().(*net.TCPAddr).Port
	if err := publish(port); err != nil {
		t.Fatalf("while the stream is open, its source port %d cannot be bound: %v", port, err)
	}
	host.Close()
	io.Copy(io.Discard, peer)
	peer.Close()
	if err := publish(port); err != nil {
		t.Fatalf("after the stream closed, its source port %d cannot be bound: %v", port, err)
	}
}

type flaky struct {
	net.Listener
	fail []error
}

func (f *flaky) Accept() (net.Conn, error) {
	if len(f.fail) > 0 {
		err := f.fail[0]
		f.fail = f.fail[1:]
		return nil, err
	}
	return f.Listener.Accept()
}

func TestServingSurvivesAnAcceptThatFailsForWantOfDescriptors(t *testing.T) {
	target := listenTCP(t)
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("served"))
			c.Close()
		}
	}()
	emfile := &net.OpError{Op: "accept", Net: "vsock", Err: os.NewSyscallError("accept4", syscall.EMFILE)}
	dir := shortDir(t)
	for name, serve := range map[string]func(net.Listener) error{
		"relay": func(l net.Listener) error {
			r := &Relay{Local: func(a netip.Addr) bool { return a.IsLoopback() }}
			return r.Serve(l)
		},
		"docker port": func(l net.Listener) error {
			return ServeSplice(l, func() (net.Conn, error) { return net.Dial("tcp", target.Addr().String()) })
		},
	} {
		l, err := net.Listen("unix", filepath.Join(dir, strings.ReplaceAll(name, " ", "-")))
		if err != nil {
			t.Fatal(err)
		}
		ended := make(chan error, 1)
		go func() { ended <- serve(&flaky{Listener: l, fail: []error{emfile, emfile, emfile}}) }()
		conn, err := net.Dial("unix", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		if name == "relay" {
			if err := relay.Open(conn, relay.Request{Kind: relay.TCP, Target: target.Addr().String()}); err != nil {
				t.Fatalf("%s: after three failed accepts: %v", name, err)
			}
		}
		if got, err := io.ReadAll(conn); err != nil || string(got) != "served" {
			t.Fatalf("%s: after three failed accepts: %q, %v", name, got, err)
		}
		conn.Close()
		select {
		case err := <-ended:
			t.Fatalf("%s: serving ended: %v", name, err)
		default:
		}
		l.Close()
		if err := <-ended; err == nil {
			t.Fatalf("%s: serving a closed listener returned no error", name)
		}
	}
}

func TestTheRelayRefusesStreamsPastItsLimit(t *testing.T) {
	if got := []int{StreamsFor(4096), StreamsFor(1024), StreamsFor(10), StreamsFor(1 << 40)}; got[0] != 2016 || got[1] != 480 || got[2] != 1 || got[3] != 1<<20 {
		t.Fatalf("streams for 4096, 1024, 10 descriptors and no limit: %v", got)
	}
	l := listenTCP(t)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(c, c)
				c.Close()
			}()
		}
	}()
	r := &Relay{Max: 2}
	open := func() (*net.UnixConn, error) {
		host := serve(t, r)
		host.SetDeadline(time.Now().Add(10 * time.Second))
		return host, relay.Open(host, relay.Request{Kind: relay.TCP, Target: l.Addr().String()})
	}
	echo := func(c *net.UnixConn, text string) {
		t.Helper()
		c.Write([]byte(text))
		buf := make([]byte, len(text))
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != text {
			t.Fatalf("%q came back as %q, %v", text, buf, err)
		}
	}
	one, err := open()
	if err != nil {
		t.Fatal(err)
	}
	two, err := open()
	if err != nil {
		t.Fatal(err)
	}
	host, guest := pair(t)
	go r.ServeConn(guest)
	host.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := io.ReadAll(host)
	if err != nil || !strings.Contains(string(line), "all 2 of its streams") {
		t.Fatalf("a third stream, before any request: %q, %v", line, err)
	}
	var refused *relay.Error
	if _, err := open(); !errors.As(err, &refused) || refused.Code != relay.CodeFailed {
		t.Fatalf("a third stream: %v", err)
	}
	echo(one, "still one")
	echo(two, "still two")
	one.Close()
	deadline := time.Now().Add(10 * time.Second)
	for r.active.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("%d streams are counted after one of two ended", r.active.Load())
		}
		time.Sleep(time.Millisecond)
	}
	three, err := open()
	if err != nil {
		t.Fatalf("a stream after one ended: %v", err)
	}
	echo(three, "three")
	echo(two, "two again")
}

func TestRelayTCPCarriesALargeStreamThatPauses(t *testing.T) {
	l := listenTCP(t)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
		c.(*net.TCPConn).CloseWrite()
	}()
	host := serve(t, &Relay{})
	if err := relay.Open(host, relay.Request{Kind: relay.TCP, Target: l.Addr().String()}); err != nil {
		t.Fatal(err)
	}
	burst := make([]byte, 5<<20+12345)
	for i := range burst {
		burst[i] = byte(i * 7)
	}
	go func() {
		for range 3 {
			host.Write(burst)
			time.Sleep(3 * largeBufferIdle)
		}
		host.CloseWrite()
	}()
	host.SetReadDeadline(time.Now().Add(30 * time.Second))
	got, err := io.ReadAll(host)
	if err != nil || len(got) != 3*len(burst) {
		t.Fatalf("%d bytes of %d came back, %v", len(got), 3*len(burst), err)
	}
	for i := range 3 {
		if !bytes.Equal(got[i*len(burst):(i+1)*len(burst)], burst) {
			t.Fatalf("burst %d came back changed", i)
		}
	}
}

func TestRelayRefusalsCarryACode(t *testing.T) {
	l := listenTCP(t)
	closed := l.Addr().String()
	l.Close()
	for name, c := range map[string]struct {
		line string
		code string
	}{
		"nothing listening":     {"tcp " + closed, relay.CodeRefused},
		"not the guest's":       {"tcp 93.184.216.34:80", relay.CodeDenied},
		"a container's address": {"udp 172.17.0.2:53", relay.CodeDenied},
		"unspecified":           {"tcp 0.0.0.0:80", relay.CodeDenied},
		"a name":                {"tcp localhost:80", relay.CodeBadRequest},
		"unknown kind":          {"unix /run/docker.sock", relay.CodeBadRequest},
		"no session service":    {"exec " + strings.Repeat("a", relay.TokenLen), relay.CodeDenied},
		"overlong":              {"tcp " + strings.Repeat("1", relay.MaxLine), relay.CodeBadRequest},
	} {
		host := serve(t, &Relay{})
		io.WriteString(host, c.line+"\n")
		err := relay.ReadReply(host)
		var refused *relay.Error
		if !errors.As(err, &refused) || refused.Code != c.code {
			t.Errorf("%s: got %v, want code %s", name, err, c.code)
		}
		host.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := host.Read(make([]byte, 1)); err != io.EOF && !errors.Is(err, syscall.ECONNRESET) {
			t.Errorf("%s: connection not closed after the refusal: %v", name, err)
		}
	}
}

func TestRelayUDPKeepsDatagramBoundaries(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	sources := make(chan string, 8)
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			sources <- from.String()
			pc.WriteTo(append([]byte("re:"), buf[:n]...), from)
		}
	}()
	open := func() *net.UnixConn {
		host := serve(t, &Relay{})
		if err := relay.Open(host, relay.Request{Kind: relay.UDP, Target: pc.LocalAddr().String()}); err != nil {
			t.Fatal(err)
		}
		return host
	}
	one, two := open(), open()
	buf := make([]byte, relay.MaxDatagram)
	for _, msg := range []string{"a", "", strings.Repeat("x", 9000)} {
		if err := relay.WriteDatagram(one, []byte(msg)); err != nil {
			t.Fatal(err)
		}
		one.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := relay.ReadDatagram(one, buf)
		if err != nil || string(buf[:n]) != "re:"+msg {
			t.Fatalf("datagram of %d bytes came back as %d bytes, %v", len(msg), n, err)
		}
	}
	relay.WriteDatagram(two, []byte("b"))
	two.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := relay.ReadDatagram(two, buf); err != nil || string(buf[:n]) != "re:b" {
		t.Fatalf("second stream: %q, %v", buf[:n], err)
	}
	first := <-sources
	<-sources
	<-sources
	if second := <-sources; second == first {
		t.Fatalf("two streams shared the source %s", first)
	}
}

func TestRelayHandsASessionTokenToExec(t *testing.T) {
	token := strings.Repeat("0123456789abcdef", 2)
	host := serve(t, &Relay{Exec: func(got string, conn net.Conn) {
		if got != token {
			relay.WriteError(conn, relay.CodeDenied, "no such session")
			return
		}
		relay.WriteOK(conn)
		io.WriteString(conn, "session output")
	}})
	if err := relay.Open(host, relay.Request{Kind: relay.Exec, Target: token}); err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(host)
	if string(got) != "session output" {
		t.Fatalf("got %q", got)
	}
}

func TestSpliceServesOneTargetAndClosesWhenThereIsNone(t *testing.T) {
	dir := shortDir(t)
	engine, err := net.Listen("unix", filepath.Join(dir, "engine"))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	go func() {
		for {
			c, err := engine.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				data, _ := io.ReadAll(c)
				c.Write(append([]byte("engine got "), data...))
			}()
		}
	}()
	var down atomic.Bool
	front, err := net.Listen("unix", filepath.Join(dir, "front"))
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()
	go ServeSplice(front, func() (net.Conn, error) {
		if down.Load() {
			return nil, errors.New("the engine is not ready")
		}
		return net.Dial("unix", filepath.Join(dir, "engine"))
	})

	c, err := net.Dial("unix", filepath.Join(dir, "front"))
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, "a request body")
	c.(*net.UnixConn).CloseWrite()
	got, _ := io.ReadAll(c)
	c.Close()
	if string(got) != "engine got a request body" {
		t.Fatalf("got %q", got)
	}

	down.Store(true)
	c, err = net.Dial("unix", filepath.Join(dir, "front"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("with no engine: read %d, %v", n, err)
	}
}

func TestLocalAddress(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1": true, "127.8.9.10": true, "::1": true,
		"0.0.0.0": false, "::": false, "224.0.0.1": false, "203.0.113.9": false,
	} {
		if got := LocalAddress(netip.MustParseAddr(addr)); got != want {
			t.Errorf("LocalAddress(%s) = %v", addr, got)
		}
	}
}
