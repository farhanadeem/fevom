//go:build darwin

package privhelper

import (
	"bufio"
	"context"
	"errors"
	api "fev/proto/privhelper"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func scutilDomains(t *testing.T) []string {
	out, err := exec.Command("/usr/sbin/scutil", "--dns").Output()
	if err != nil {
		t.Skip("scutil:", err)
	}
	var ds []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "domain" || strings.HasPrefix(k, "search domain[") {
			d := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(v), "."))
			if !slices.Contains(ds, d) {
				ds = append(ds, d)
			}
		}
	}
	return ds
}

func TestTheStoreHoldsTheDomainsScutilShows(t *testing.T) {
	store, err := Darwin{}.DNSResolvers()
	if err != nil {
		t.Fatal(err)
	}
	files, err := foreignResolvers(ResolverDir)
	if err != nil {
		t.Fatal(err)
	}
	var have []string
	for _, r := range append(store, files...) {
		for _, d := range append(r.Domains, r.Search...) {
			have = append(have, strings.ToLower(strings.TrimSuffix(d, ".")))
		}
	}
	for _, d := range scutilDomains(t) {
		if d == "local" || strings.HasSuffix(d, ".arpa") {
			continue
		}
		if !slices.Contains(have, d) {
			t.Errorf("scutil --dns shows %s; the store and %s do not", d, ResolverDir)
		}
	}
	t.Logf("%d resolvers in the store: %+v", len(store), store)
}

func TestTheStoresEntriesAreReadAsAUser(t *testing.T) {
	es, err := DarwinStore{}.Entries()
	if err != nil {
		t.Fatal(err)
	}
	for n, e := range es {
		t.Logf("fev's entry for %s on this Mac: %+v", n, e)
	}
	got := parseEntryLines("K State:/Network/Service/VPN/DNS\nP 53\nK " + api.StoreKey("shop.local") + "\nP 15353\nT 1792231200.500000\nK State:/Network/Service/dev.fevom.resolver.a/b/DNS\nP 1\n")
	if len(got) != 1 || got["shop.local"].Port != 15353 || got["shop.local"].Written.Unix() != 1792231200 {
		t.Errorf("parsed: %+v", got)
	}
}

func TestTheLocalHostNameIsTheMacs(t *testing.T) {
	got, err := Darwin{}.LocalHostName()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/usr/sbin/scutil", "--get", "LocalHostName").Output()
	if err != nil {
		t.Skip("scutil:", err)
	}
	if want := strings.TrimSpace(string(out)); got != want {
		t.Errorf("%q, scutil says %q", got, want)
	}
}

func TestThePeerIsReadFromTheConnection(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "s"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := net.Dial("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sc, err := l.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	p, err := PeerOf(sc)
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	if p.UID != os.Getuid() || p.PID != os.Getpid() || filepath.Base(p.Path) != filepath.Base(self) {
		t.Errorf("peer %+v, want uid %d pid %d %s", p, os.Getuid(), os.Getpid(), self)
	}
}

func TestBindPortTakesNoPortAnotherHolds(t *testing.T) {
	for _, c := range []struct{ network, wildcard, loopback string }{{"tcp4", "0.0.0.0:0", "127.0.0.1"}, {"tcp6", "[::]:0", "::1"}} {
		l, err := net.Listen(c.network, c.wildcard)
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		fd, err := Darwin{}.BindPort("tcp", netip.AddrPortFrom(netip.MustParseAddr(c.loopback), uint16(port)))
		if err == nil {
			syscall.Close(fd)
			t.Errorf("%s port %d was taken on %s beside another program's wildcard listener", c.network, port, c.loopback)
		}
		l.Close()
	}
	u, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	port := u.LocalAddr().(*net.UDPAddr).Port
	if fd, err := (Darwin{}).BindPort("udp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))); err == nil {
		syscall.Close(fd)
		t.Errorf("udp port %d was taken beside another program's", port)
	}
}

func TestBindPortBindsLoopback(t *testing.T) {
	for _, c := range []struct {
		proto string
		addr  string
	}{{"tcp", "127.0.0.1"}, {"udp", "127.0.0.1"}, {"tcp", "::1"}, {"udp", "::1"}} {
		fd, err := Darwin{}.BindPort(c.proto, netip.AddrPortFrom(netip.MustParseAddr(c.addr), 0))
		if err != nil {
			t.Errorf("%s %s: %v", c.proto, c.addr, err)
			continue
		}
		f := os.NewFile(uintptr(fd), "bound")
		if c.proto == "tcp" {
			l, err := net.FileListener(f)
			if err != nil {
				t.Errorf("%s %s: %v", c.proto, c.addr, err)
			} else {
				conn, err := net.Dial("tcp", l.Addr().String())
				if err != nil {
					t.Errorf("dialling %s: %v", l.Addr(), err)
				} else {
					conn.Close()
				}
				l.Close()
			}
		}
		f.Close()
	}
}

func TestWithoutRootTheTunnelAndRoutesAreRefusedByTheKernel(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root: the root tier's")
	}
	before, _ := net.Interfaces()
	_, _, err := Darwin{}.OpenTunnel(1500, netip.MustParseAddr("198.19.3.1"), netip.MustParseAddr("198.19.3.2"), netip.Addr{})
	if err == nil {
		t.Fatal("a utun was made without root")
	}
	after, _ := net.Interfaces()
	if len(after) != len(before) {
		t.Errorf("interfaces %d, then %d", len(before), len(after))
	}
	t.Logf("utun: %v", err)
	err = Darwin{}.AddRoute("lo0", netip.MustParsePrefix("192.0.2.0/24"))
	if !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EACCES) {
		t.Errorf("a route without root: %v", err)
	}
	t.Logf("route: %v", err)
}

func TestTheProbeFindsTheMacsOwnNameAndNotARandomOne(t *testing.T) {
	if os.Getenv("FEV_PROBE_LIVE") != "1" {
		t.Skip("FEV_PROBE_LIVE=1 asks the LAN")
	}
	own, err := Darwin{}.LocalHostName()
	if err != nil {
		t.Fatal(err)
	}
	ownName := strings.ToLower(own) + ".local"
	random := "fev-probe-" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "") + ".local"
	start := time.Now()
	got, err := Darwin{}.Probe(context.Background(), []string{ownName, random}, []ProbeKind{ProbeSystem, ProbeZone, ProbeMulticast}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	t.Logf("%s: %+v; %s: %+v; %s", ownName, got[ownName], random, got[random], took)
	if !slices.ContainsFunc(got[ownName], func(a Answer) bool { return a.Kind == ProbeMulticast }) {
		t.Errorf("the Mac's own name was not answered by mDNS")
	}
	if len(got[random]) != 0 {
		t.Errorf("a random name was answered: %+v", got[random])
	}
	if took > 1500*time.Millisecond {
		t.Errorf("the probe took %s", took)
	}
}
