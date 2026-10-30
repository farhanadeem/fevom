package webproxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"fev/guest/internal/webproxy/webctl"
	api "fev/proto/agent"
)

func TestInitHoldsTheWebPortsUntilTheFirstConnection(t *testing.T) {
	h, err := webctl.Hold(netip.MustParseAddr("127.0.0.1"), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a80, a443, err := h.Addrs()
	if err != nil {
		t.Fatal(err)
	}
	if a80.Port() == 0 || a443.Port() == 0 || a80 == a443 {
		t.Fatalf("bound at %s and %s", a80, a443)
	}
	woke := make(chan error, 1)
	go func() { woke <- h.Wait() }()
	select {
	case err := <-woke:
		t.Fatalf("Wait returned with no connection: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	first, err := net.DialTimeout("tcp", a443.String(), 2*time.Second)
	if err != nil {
		t.Fatalf("a connection while no child runs: %v", err)
	}
	defer first.Close()
	select {
	case err := <-woke:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return for a waiting connection")
	}
	var dups [2]int
	for i, f := range h.Files() {
		if dups[i], err = unix.Dup(int(f.Fd())); err != nil {
			t.Fatal(err)
		}
	}
	l80, l443, err := Listeners(fmt.Sprintf("%d,%d", dups[0], dups[1]))
	if err != nil {
		t.Fatal(err)
	}
	if l80.Addr().String() != a80.String() || l443.Addr().String() != a443.String() {
		t.Fatalf("the child's listeners %s %s, init's %s %s", l80.Addr(), l443.Addr(), a80, a443)
	}
	c, err := l443.Accept()
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("x"))
	b := make([]byte, 1)
	first.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(first, b); err != nil || b[0] != 'x' {
		t.Fatalf("the waiting connection was not the one accepted: %q %v", b, err)
	}
	c.Close()
	l80.Close()
	l443.Close()
	gap, err := net.DialTimeout("tcp", a80.String(), 2*time.Second)
	if err != nil {
		t.Fatalf("a connection in a restart's gap: %v", err)
	}
	defer gap.Close()
	h.Close()
	gap.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = gap.Read(b)
	var ne net.Error
	if err == nil || errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("a connection waiting at the close: %v", err)
	}
	if _, err := net.DialTimeout("tcp", a80.String(), 2*time.Second); err == nil {
		t.Fatal("connected after the close")
	}
}

func TestTheChildKnowsTheSetBeforeItAccepts(t *testing.T) {
	r := newRig(t)
	r.backend("app", "198.19.64.2:80", page("app"), false, false)
	var ls [2]net.Listener
	for i := range ls {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		ls[i] = l
	}
	r.mu.Lock()
	r.origin[ls[0].Addr().String()] = netip.MustParseAddrPort("198.19.4.2:80")
	r.origin[ls[1].Addr().String()] = netip.MustParseAddrPort("198.19.4.2:443")
	r.mu.Unlock()

	c, err := net.Dial("tcp", ls[0].Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: app.fev.local\r\nConnection: close\r\n\r\n")

	ctrlR, ctrlW := io.Pipe()
	repR, repW := io.Pipe()
	ns := &Namespaces{}
	served := make(chan error, 1)
	go func() { served <- Serve(ctrlR, &Reporter{W: repW}, ls[0], ls[1], r.p, ns) }()
	reports := bufio.NewScanner(repR)
	go func() {
		time.Sleep(200 * time.Millisecond)
		set := api.DomainsParams{Seq: 1, Index: "198.19.4.1", Targets: []api.DomainTarget{container("app", "app", "198.19.4.2", "198.19.64.2")}}
		b, _ := json.Marshal(Control{NetNSRoot: "/proc/1/root", Machines: map[string]string{}, Domains: &set})
		ctrlW.Write(append(b, '\n'))
	}()
	if !reports.Scan() || !strings.Contains(reports.Text(), `"ready":true`) {
		t.Fatalf("the first report: %q", reports.Text())
	}
	go io.Copy(io.Discard, repR)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("the waiting connection: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(string(body), "app host=app.fev.local") {
		t.Fatalf("the waiting connection: %d %q", resp.StatusCode, body)
	}
	ctrlW.Close()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve at the control socket's end: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return at the control socket's end")
	}
}

func TestAChildWithNoFirstLineDoesNotServe(t *testing.T) {
	r := newRig(t)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	if err := Serve(strings.NewReader(""), &Reporter{W: io.Discard}, l, l, r.p, &Namespaces{}); err == nil {
		t.Fatal("served with no first line")
	}
}
