package privhelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"fev/proto/contract"
	"fev/proto/jsonrpc"
	"fev/proto/netplan"
)

var verbs = []string{
	MethodHello, MethodTunnelOpen, MethodRoutesSet, MethodPortBind, MethodDockerSockLink,
	MethodDockerSockUnlink, MethodLocalNamesSet, MethodResolverSet, MethodResolverClear,
}

func TestTheFixturesMatchTheSchema(t *testing.T) {
	built, err := Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range contract.Verify(built, Schema) {
		t.Error(err)
	}
	pending, err := PendingFixtures()
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range contract.VerifyPending(pending, Pending, Schema) {
		t.Error(err)
	}
	for _, v := range verbs {
		_, s := Schema[v]
		_, p := Pending[v]
		if s == p {
			t.Errorf("%s: in Schema %v, in Pending %v", v, s, p)
		}
	}
	if len(Schema)+len(Pending) != len(verbs) {
		t.Errorf("%d verbs in the tables, %d defined", len(Schema)+len(Pending), len(verbs))
	}
}

func bind[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	v, err := jsonrpc.Bind[T](raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTheExamplesObeyTheRules(t *testing.T) {
	plan := netplan.Default()
	p6, _ := netplan.Parse6("fd5b:6e2a:91c0::/48")
	bases := Bases{plan.Base(), p6.Prefix()}
	routed := map[string]bool{}
	for _, r := range plan.Routed() {
		routed[r.String()] = true
	}
	fixtures, _ := PendingFixtures()
	built, _ := Fixtures()
	seen := map[string]bool{}
	for _, fx := range append(fixtures, built...) {
		seen[fx.Method] = true
		switch fx.Method {
		case MethodHello:
			r := bind[HelloResult](t, fx.Result)
			if r.ProtocolVersion != ProtocolVersion || r.Base != plan.Base().String() || r.UID == 0 {
				t.Errorf("hello: %+v", r)
			}
		case MethodTunnelOpen:
			p, r := bind[TunnelParams](t, fx.Params), bind[Tunnel](t, fx.Result)
			if CheckMTU(p.MTU) != nil || r.MTU != p.MTU || r.Mac != plan.TunnelMac().String() || r.Peer != plan.TunnelPeer().String() ||
				(r.Mac6 != "") != p.IPv6 || (p.IPv6 && r.Mac6 != p6.TunnelMac6().String()) || !strings.HasPrefix(r.Interface, "utun") {
				t.Errorf("tunnel.open %+v: %+v", p, r)
			}
		case MethodRoutesSet:
			p, r := bind[RoutesParams](t, fx.Params), bind[Routes](t, fx.Result)
			if _, err := CheckRoutes(bases, p.Prefixes); err != nil {
				t.Errorf("routes.set: %v", err)
			}
			for _, s := range p.Prefixes {
				if !routed[s] {
					t.Errorf("routes.set routes %s, which the plan does not", s)
				}
			}
			if strings.Join(r.Prefixes, " ") != strings.Join(p.Prefixes, " ") || r.Interface != p.Interface {
				t.Errorf("routes.set of %v left %v", p.Prefixes, r.Prefixes)
			}
		case MethodPortBind:
			p, r := bind[PortParams](t, fx.Params), bind[Bound](t, fx.Result)
			ap, err := netip.ParseAddrPort(r.Address)
			if CheckPort(p) != nil || err != nil || !ap.Addr().IsLoopback() || int(ap.Port()) != p.Port {
				t.Errorf("port.bind %+v: %+v", p, r)
			}
		case MethodLocalNamesSet:
			p, r := bind[LocalNamesParams](t, fx.Params), bind[LocalNames](t, fx.Result)
			if len(p.Names) > MaxLocalNames {
				t.Errorf("localnames.set of %d names", len(p.Names))
			}
			names := []string{}
			for _, n := range r.Names {
				names = append(names, n.Name)
				if c, err := CanonicalLocalName(n.Name, ""); err != nil || c != n.Name {
					t.Errorf("localnames.set: %q is not canonical: %v", n.Name, err)
				}
				switch n.State {
				case NamePresent:
					if n.Reason != "" || n.AnsweredBy != "" {
						t.Errorf("localnames.set: present with a reason: %+v", n)
					}
				case NameRefused, NameRemoved:
					if n.State == NameRefused && n.Reason == "" {
						t.Errorf("localnames.set: refused with no reason: %+v", n)
					}
				default:
					t.Errorf("localnames.set: state %q", n.State)
				}
			}
			if !sort.StringsAreSorted(names) {
				t.Errorf("localnames.set: %v is not in order", names)
			}
		}
	}
	for _, v := range verbs {
		if !seen[v] {
			t.Errorf("%s has no example", v)
		}
	}
}

func TestRoutesOutsideTheRecordedBaseAreRefused(t *testing.T) {
	bases, err := BasesOf(InstallRecord{Base: "198.19.0.0/16", Base6: "fd5b:6e2a:91c0::/48"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ok := range [][]string{
		{"198.19.0.0/24", "198.19.64.0/18"},
		{"198.19.0.0/16"},
		{"fd5b:6e2a:91c0::/48", "fd5b:6e2a:91c0:3::/64"},
		{},
	} {
		if _, err := CheckRoutes(bases, ok); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for _, bad := range [][]string{
		{"8.8.8.0/24"},
		{"10.123.0.0/24"},
		{"10.0.0.0/8"},
		{"172.16.30.0/24"},
		{"0.0.0.0/0"},
		{"::/0"},
		{"198.18.0.0/15"},
		{"198.19.0.1/24"},
		{"198.19.0.0/24", "198.19.0.0/24"},
		{"127.0.0.0/8"},
		{"fe80::/64"},
		{"fd00:1::/48"},
		{"fd5b:6e2a:91c0::/40"},
		{"224.0.0.0/4"},
		{"198.19.0.0/24 "},
	} {
		_, err := CheckRoutes(bases, bad)
		var r Refusal
		if !errors.As(err, &r) || r.Arg == "" || r.Reason == "" {
			t.Errorf("%v: %v", bad, err)
		}
	}
	var many []string
	for i := range MaxRoutes + 1 {
		many = append(many, fmt.Sprintf("198.19.%d.0/24", i))
	}
	if _, err := CheckRoutes(bases, many); err == nil {
		t.Error("65 routes were taken")
	}
	extra, _ := BasesOf(InstallRecord{Base: "198.19.0.0/16", Extra: []string{"10.123.0.0/24"}})
	if _, err := CheckRoutes(extra, []string{"10.123.0.0/24"}); err != nil {
		t.Errorf("a prefix the install recorded: %v", err)
	}
	if _, err := BasesOf(InstallRecord{Base: "198.19.0.1/16"}); err == nil {
		t.Error("a record with host bits in its base was read")
	}
	if _, err := BasesOf(InstallRecord{}); err == nil {
		t.Error("a record with no base was read")
	}
}

func TestCustomNamesAreCanonicalAndOnlyHostNamesUnderLocal(t *testing.T) {
	for in, want := range map[string]string{
		"app.local":       "app.local",
		"App.Local.":      "app.local",
		"x.bar-m7r.local": "x.bar-m7r.local",
		"a1-b2.local":     "a1-b2.local",
	} {
		if got, err := CanonicalLocalName(in, "adas-mac.local"); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"local", "local.", ".local", "fev.local", "Fev.Local.", "web.fev.local", "x.y.fev.local",
		"adas-mac.local", "Adas-Mac.local.", "_tcp.local", "_http._tcp.local", "a_b.local",
		"-a.local", "a-.local", "a..local", "app.test", "example.com", "app.localhost",
		strings.Repeat("a", 64) + ".local", strings.Repeat("a.", 124) + "x.local", "*.app.local", "app .local", "",
	} {
		_, err := CanonicalLocalName(bad, "adas-mac.local")
		var r Refusal
		if !errors.As(err, &r) || r.Reason == "" {
			t.Errorf("%q was taken: %v", bad, err)
		}
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"corp.local", "dc1.corp.local", true},
		{"dc1.corp.local", "corp.local", true},
		{"corp.local", "corp.local", true},
		{"corp.local", "xcorp.local", false},
		{"a.local", "b.local", false},
	} {
		if Overlaps(c.a, c.b) != c.want {
			t.Errorf("Overlaps(%s, %s) != %v", c.a, c.b, c.want)
		}
	}
}

func TestPortsAndMTUAreBounded(t *testing.T) {
	for _, p := range []PortParams{{"tcp", 4, 80}, {"udp", 6, 53}, {"tcp", 6, 1023}, {"tcp", 4, 1}} {
		if err := CheckPort(p); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	for _, p := range []PortParams{{"tcp", 4, 0}, {"tcp", 4, 1024}, {"tcp", 4, 8080}, {"sctp", 4, 80}, {"tcp", 5, 80}, {"", 4, 80}} {
		if err := CheckPort(p); err == nil {
			t.Errorf("%+v was taken", p)
		}
	}
	for mtu, ok := range map[int]bool{1279: false, 1280: true, 1500: true, 16384: true, 65535: true, 65536: false, 0: false} {
		if (CheckMTU(mtu) == nil) != ok {
			t.Errorf("MTU %d: %v", mtu, CheckMTU(mtu))
		}
	}
}

func fakeHelper(t *testing.T, serve func(req Request, uc *net.UnixConn)) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "priv.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			uc := c.(*net.UnixConn)
			req, err := ReadRequest(uc)
			if err == nil {
				serve(req, uc)
			}
			uc.Close()
		}
	}()
	return path
}

func TestCallCarriesTheDescriptorAndTheRefusal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	wfd := int(w.Fd())
	path := fakeHelper(t, func(req Request, uc *net.UnixConn) {
		switch req.Method {
		case MethodTunnelOpen:
			var p TunnelParams
			json.Unmarshal(req.Params, &p)
			if err := CheckMTU(p.MTU); err != nil {
				WriteReply(uc, req, nil, err, -1)
				return
			}
			WriteReply(uc, req, Tunnel{Interface: "utun9", Mac: "198.19.3.1", Peer: "198.19.3.2", MTU: p.MTU}, nil, wfd)
		case MethodPortBind:
			WriteReply(uc, req, Bound{Address: "127.0.0.1:80"}, nil, -1)
		case MethodDockerSockLink:
			WriteReply(uc, req, DockerSock{State: StateFev}, nil, wfd)
		case MethodHello:
		default:
			WriteReply(uc, req, nil, &Error{Code: CodeNotImplemented, Message: req.Method + " is not implemented in this build"}, -1)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var tun Tunnel
	f, err := Call(ctx, path, MethodTunnelOpen, TunnelParams{MTU: 1500}, &tun)
	if err != nil || f == nil || tun.Interface != "utun9" {
		t.Fatalf("tunnel.open: %v %v %+v", f, err, tun)
	}
	if _, err := f.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	b := make([]byte, 1)
	if _, err := r.Read(b); err != nil || b[0] != 'x' {
		t.Errorf("read through the passed descriptor: %q %v", b, err)
	}

	_, err = Call(ctx, path, MethodTunnelOpen, TunnelParams{MTU: 100}, &tun)
	ref, ok := AsRefusal(err)
	if !ok || ref.Arg != "mtu" {
		t.Errorf("a refused MTU: %v", err)
	}
	if _, err := Call(ctx, path, MethodPortBind, PortParams{"tcp", 4, 80}, nil); err == nil || !strings.Contains(err.Error(), "without its descriptor") {
		t.Errorf("port.bind with no descriptor: %v", err)
	}
	if _, err := Call(ctx, path, MethodDockerSockLink, Empty{}, nil); err == nil || !strings.Contains(err.Error(), "passes none") {
		t.Errorf("dockersock.link with a descriptor: %v", err)
	}
	var e *Error
	if _, err := Call(ctx, path, MethodLocalNamesSet, LocalNamesParams{Names: []string{}}, nil); !errors.As(err, &e) || e.Code != CodeNotImplemented {
		t.Errorf("a pending verb: %v", err)
	}
	if _, err := Call(ctx, path, MethodHello, Empty{}, nil); err == nil || !strings.Contains(err.Error(), "without a reply") {
		t.Errorf("a closed connection: %v", err)
	}
}

func TestARequestIsOneBoundedLine(t *testing.T) {
	good := `{"jsonrpc":"2.0","id":1,"method":"hello","params":{}}` + "\n"
	if req, err := ReadRequest(strings.NewReader(good)); err != nil || req.Method != MethodHello {
		t.Errorf("%q: %+v %v", good, req, err)
	}
	for _, bad := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"hello"}`,
		`{"jsonrpc":"1.0","id":1,"method":"hello"}` + "\n",
		`{"jsonrpc":"2.0","id":1,"method":"hello","path":"/etc"}` + "\n",
		`{"jsonrpc":"2.0","id":1}` + "\n",
		`{"jsonrpc":"2.0","id":1,"method":"localnames.set","params":{"names":["` + strings.Repeat("a", MaxLine) + `"]}}` + "\n",
	} {
		if _, err := ReadRequest(strings.NewReader(bad)); err == nil {
			t.Errorf("%.80q was read", bad)
		}
	}
}

func TestADescriptorIsNeverOpenAcrossAFork(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	wfd := int(w.Fd())
	read, send := make(chan struct{}), make(chan struct{})
	path := fakeHelper(t, func(req Request, uc *net.UnixConn) {
		close(read)
		<-send
		WriteReply(uc, req, Tunnel{Interface: "utun9", Mac: "198.19.3.1", Peer: "198.19.3.2", MTU: 1500}, nil, wfd)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		f   *os.File
		err error
	}
	done := make(chan result, 1)
	go func() {
		var tun Tunnel
		f, err := Call(ctx, path, MethodTunnelOpen, TunnelParams{MTU: 1500}, &tun)
		done <- result{f, err}
	}()
	<-read
	syscall.ForkLock.Lock()
	close(send)
	select {
	case res := <-done:
		syscall.ForkLock.Unlock()
		if res.f != nil {
			res.f.Close()
		}
		t.Fatalf("the descriptor was received while a fork held the lock (%v)", res.err)
	case <-time.After(300 * time.Millisecond):
	}
	syscall.ForkLock.Unlock()
	res := <-done
	if res.err != nil || res.f == nil {
		t.Fatalf("tunnel.open: %v %v", res.f, res.err)
	}
	defer res.f.Close()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, res.f.Fd(), syscall.F_GETFD, 0)
	if errno != 0 || flags&syscall.FD_CLOEXEC == 0 {
		t.Errorf("the passed descriptor's flags: %#x %v", flags, errno)
	}
}

func TestOnlyTwoLabelNamesJoinTheSearchList(t *testing.T) {
	for name, want := range map[string]bool{
		"fev.local": true, "shop.local": true, "shop.local.": true,
		"web.fev.local": false, "x.shop.local": false, "a.b.c.local": false,
	} {
		if got := StoreSearches(name); got != want {
			t.Errorf("StoreSearches(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRoutesTakeTheNAT64TwinsOfTheBaseAlone(t *testing.T) {
	plan := netplan.Default()
	p6, _ := netplan.Parse6("fd5b:6e2a:91c0::/48")
	bases := Bases{plan.Base(), p6.Prefix()}
	pref := netip.MustParsePrefix("64:ff9b::/96")
	other := netip.MustParsePrefix("2001:db8:122:344::/64")
	var twins []string
	for _, p := range plan.Routed() {
		twins = append(twins, netplan.NAT64Prefix(pref, p).String(), netplan.NAT64Prefix(other, p).String())
	}
	if _, err := CheckRoutesNAT64(bases, []netip.Prefix{pref, other}, twins); err != nil {
		t.Fatalf("the plan's twins: %v", err)
	}
	if _, err := CheckRoutesNAT64(bases, []netip.Prefix{pref}, twins); err == nil {
		t.Error("a twin under a NAT64 prefix the Mac does not have was taken")
	}
	if _, err := CheckRoutes(bases, twins[:1]); err == nil {
		t.Error("CheckRoutes took a twin with no NAT64 prefix")
	}
	for _, s := range []string{
		netplan.NAT64Prefix(pref, netip.MustParsePrefix("10.0.0.0/16")).String(),
		netplan.NAT64Prefix(pref, netip.MustParsePrefix("198.18.0.0/15")).String(),
		"64:ff9b::/96",
		"64:ff9b::c613:4000/113",
		"64:ff9b:1::c613:4000/114",
	} {
		if _, err := CheckRoutesNAT64(bases, []netip.Prefix{pref}, []string{s}); err == nil {
			t.Errorf("%s was taken", s)
		}
	}
}
