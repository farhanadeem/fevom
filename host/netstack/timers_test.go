package netstack

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

func quietFor(ns *Stack, window, limit time.Duration) (TimerCounts, bool) {
	end := time.Now().Add(limit)
	for {
		before := ns.Timers()
		time.Sleep(window)
		after := ns.Timers()
		if after.Pending == 0 && after.Armed == before.Armed && after.Fired == before.Fired {
			return after, true
		}
		if time.Now().After(end) {
			return after, false
		}
	}
}

func TestNoTimerWithNoFlows(t *testing.T) {
	defer func(u, a time.Duration) { udpUnreplied, udpAssured = u, a }(udpUnreplied, udpAssured)
	udpUnreplied, udpAssured = 300*time.Millisecond, 600*time.Millisecond
	const window = time.Second

	w := newWorld(t)
	if c, err := w.dialTCP(t, "93.184.215.14:80"); err != nil {
		t.Fatal(err)
	} else {
		c.Close()
	}
	nud, terr := w.ns.stack.NUDConfigurations(nicID, ipv4.ProtocolNumber)
	if terr != nil {
		t.Fatal(terr)
	}
	nud.BaseReachableTime = time.Second
	if terr := w.ns.stack.SetNUDConfigurations(nicID, ipv4.ProtocolNumber, nud); terr != nil {
		t.Fatal(terr)
	}

	var open []io.Closer
	echo := func(halfClose bool) {
		c, err := w.dialTCP(t, "93.184.215.14:80")
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(10 * time.Second))
		msg := []byte(strings.Repeat("net-18 ", 20000))
		go c.Write(msg)
		if _, err := io.ReadFull(c, make([]byte, len(msg))); err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Time{})
		if halfClose {
			c.CloseWrite()
			io.Copy(io.Discard, c)
		}
		open = append(open, c)
	}
	echo(false)
	echo(true)
	echo(false)
	dst := netip.MustParseAddrPort("8.8.4.4:9999")
	u, err := gonet.DialUDP(w.guest, nil, &tcpip.FullAddress{NIC: 1, Addr: toAddr(dst.Addr()), Port: dst.Port()}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	u.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := u.Write([]byte("datagram")); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Read(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	open = append(open, u)
	w.host.add(t, "idle.example. 60 IN A 192.0.2.7")
	for _, network := range []string{"udp", "tcp"} {
		if got := answers(w.query(t, network, "idle.example.", 1)); !strings.Contains(got, "192.0.2.7") {
			t.Fatalf("DNS over %s answered %q", network, got)
		}
	}

	busy := w.ns.Timers()
	if busy.Armed == 0 || busy.Pending == 0 {
		t.Fatalf("with TCP connections and a UDP association open the count is %+v: it sees no timer, so it can show nothing", busy)
	}
	for _, c := range open {
		c.Close()
	}

	idle, ok := quietFor(w.ns, window, 20*time.Second)
	if !ok {
		t.Fatalf("no flows, and the stack still has timers 20 s later: %+v (%d armed since the flows closed); something arms one with nothing to do",
			idle, idle.Armed-busy.Armed)
	}
	t.Logf("with flows open: %+v; %v with no flows: %+v", busy, window, idle)
}

func TestAStackNothingHasCrossedArmsNoTimer(t *testing.T) {
	hostEnd, guestEnd := socketPair(t)
	ns, err := New(hostEnd, Options{Host: newFakeHost()})
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	g := newGuestStack(t, guestEnd, false)
	defer g.Close()
	time.Sleep(time.Second)
	if c := ns.Timers(); c.Armed != 0 || c.Pending != 0 {
		t.Fatalf("a stack with an interface up and no traffic has armed timers: %+v", c)
	}
}

func TestTheCountedTimerCountsEveryArmingAndNothingTwice(t *testing.T) {
	var c timerCounts
	var fired atomic.Int32
	tm := c.afterFunc(time.Hour, func() { fired.Add(1) })
	if got := c.read(); got != (TimerCounts{Armed: 1, Pending: 1}) {
		t.Fatalf("after one arming: %+v", got)
	}
	tm.Reset(time.Hour)
	if !tm.Stop() || tm.Stop() {
		t.Fatal("Stop is true once, for the timer it stopped")
	}
	if got := c.read(); got != (TimerCounts{Armed: 2, Pending: 0}) {
		t.Fatalf("after a reset and a stop: %+v", got)
	}
	tm.Reset(time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := c.read(); got != (TimerCounts{Armed: 3, Fired: 1, Pending: 0}) || fired.Load() != 1 || tm.Stop() {
		t.Fatalf("after firing: %+v, ran %d times", got, fired.Load())
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				tm.Reset(time.Microsecond)
			}
		}()
	}
	wg.Wait()
	tm.Stop()
	time.Sleep(20 * time.Millisecond)
	got := c.read()
	if got.Pending != 0 || got.Armed != 3+4000 || got.Fired > got.Armed || uint64(fired.Load()) != got.Fired {
		t.Fatalf("after 4000 racing resets: %+v, ran %d times", got, fired.Load())
	}
}

func TestOnlyTheCountedClockMakesTimers(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		timePkg := ""
		for _, imp := range f.Imports {
			if imp.Path.Value == `"time"` {
				timePkg = "time"
				if imp.Name != nil {
					timePkg = imp.Name.Name
				}
			}
		}
		if timePkg == "" || timePkg == "." || timePkg == "_" {
			if timePkg != "" {
				t.Errorf("%s imports time as %q, which hides what it calls", name, timePkg)
			}
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != timePkg || pkg.Obj != nil {
				return true
			}
			switch sel.Sel.Name {
			case "NewTicker", "Tick", "Ticker":
				t.Errorf("%s: time.%s: a ticker wakes the stack whether or not there are flows", fset.Position(sel.Pos()), sel.Sel.Name)
			case "AfterFunc", "NewTimer", "After", "Sleep", "Timer":
				if name != "timers.go" {
					t.Errorf("%s: time.%s: a timer the stack's count does not see; use the counted one (timers.go)", fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
	if checked < 10 {
		t.Fatalf("only %d source files were read: not the package's directory", checked)
	}
}
