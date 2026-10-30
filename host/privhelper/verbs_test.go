package privhelper_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"fev/host/privhelper"
	"fev/host/privhelper/fakesys"
	api "fev/proto/privhelper"
)

type rig struct {
	t    *testing.T
	sys  *fakesys.System
	root string
	home string
	sock string
	srv  *privhelper.Server
	now  time.Time
	mu   sync.Mutex
	peer privhelper.Peer
	form string
}

func newRig(t *testing.T, edit func(*api.InstallRecord)) *rig {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	r := &rig{t: t, sys: fakesys.New(), root: filepath.Join(dir, "root"), home: filepath.Join(dir, "home"), sock: filepath.Join(dir, "p.sock"), now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	for _, d := range []string{r.root, r.home, filepath.Join(r.root, "var/run"), filepath.Join(r.root, "etc")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rec := api.InstallRecord{UID: os.Getuid(), Home: r.home, Base: "198.19.0.0/16", Base6: "fd5b:6e2a:91c0::/48", ResolverPort: 15353, ResolverForm: api.ResolverFormStore}
	if edit != nil {
		edit(&rec)
	}
	r.form = rec.ResolverForm
	r.peer = privhelper.Peer{UID: os.Getuid(), PID: os.Getpid()}
	r.srv, err = privhelper.New(privhelper.Config{
		Record: rec, Root: r.root, Sys: r.sys,
		Now: func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now },
		PeerOf: func(uc *net.UnixConn) (privhelper.Peer, error) {
			real, err := privhelper.PeerOf(uc)
			if err != nil {
				return real, err
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			p := r.peer
			p.Path = real.Path
			return p, nil
		},
		Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: r.sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			go r.srv.ServeConn(c)
		}
	}()
	return r
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

func (r *rig) call(method string, params, result any) (*os.File, error) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return api.Call(ctx, r.sock, method, params, result)
}

func refusal(t *testing.T, err error, arg string) api.Refusal {
	t.Helper()
	ref, ok := api.AsRefusal(err)
	if !ok {
		t.Fatalf("not a refusal: %v", err)
	}
	if arg != "" && !strings.HasPrefix(ref.Arg, arg) {
		t.Errorf("refusal of %q, want %q: %v", ref.Arg, arg, ref)
	}
	return ref
}

func (r *rig) openTunnel() string {
	r.t.Helper()
	var tun api.Tunnel
	f, err := r.call(api.MethodTunnelOpen, api.TunnelParams{MTU: 1500}, &tun)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { f.Close() })
	return tun.Interface
}

func (r *rig) routesOn(ifname string) []string {
	r.t.Helper()
	ifs, _ := r.sys.Interfaces()
	idx := -1
	for _, i := range ifs {
		if i.Name == ifname {
			idx = i.Index
		}
	}
	tab, _ := r.sys.Routes()
	var out []string
	for _, rt := range tab {
		if rt.Index == idx && rt.Flags&privhelper.RTFStatic != 0 {
			out = append(out, rt.Prefix.String())
		}
	}
	slices.Sort(out)
	return out
}

func TestTunnelOpenPassesTheTunnelAndRefusesASecond(t *testing.T) {
	r := newRig(t, nil)
	var tun api.Tunnel
	f, err := r.call(api.MethodTunnelOpen, api.TunnelParams{MTU: 1500}, &tun)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if tun.Interface != "utun0" || tun.Mac != "198.19.3.1" || tun.Peer != "198.19.3.2" || tun.MTU != 1500 || tun.Mac6 != "" {
		t.Errorf("tunnel: %+v", tun)
	}
	if _, err := f.Write([]byte("packet")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 64)
	n, err := syscall.Read(r.sys.PeerOf("utun0"), b)
	if err != nil || string(b[:n]) != "packet" {
		t.Errorf("the tunnel's far end read %q %v", b[:n], err)
	}

	_, err = r.call(api.MethodTunnelOpen, api.TunnelParams{MTU: 1500}, &tun)
	if ref := refusal(t, err, "tunnel"); !strings.Contains(ref.Reason, "utun0") {
		t.Errorf("the second tunnel: %v", ref)
	}
	_, err = r.call(api.MethodTunnelOpen, api.TunnelParams{MTU: 1279}, &tun)
	refusal(t, err, "mtu")

	r.sys.Detach("utun0")
	var t6 api.Tunnel
	f6, err := r.call(api.MethodTunnelOpen, api.TunnelParams{MTU: 16384, IPv6: true}, &t6)
	if err != nil {
		t.Fatal(err)
	}
	f6.Close()
	if t6.Mac6 != "fd5b:6e2a:91c0:2::1" || t6.MTU != 16384 {
		t.Errorf("the IPv6 tunnel: %+v", t6)
	}

	r4 := newRig(t, func(rec *api.InstallRecord) { rec.Base6 = "" })
	_, err = r4.call(api.MethodTunnelOpen, api.TunnelParams{MTU: 1500, IPv6: true}, &tun)
	refusal(t, err, "ipv6")
}

func TestARacingSecondTunnelIsGivenUp(t *testing.T) {
	r := newRig(t, nil)
	r.sys.With(func(s *fakesys.System) {
		s.AfterOpen = func(s *fakesys.System) {
			s.Ifaces = append(s.Ifaces, privhelper.Interface{Name: "utun7", Index: 70, Addrs: []netip.Prefix{netip.MustParsePrefix("198.19.3.1/32")}})
		}
	})
	_, err := r.call(api.MethodTunnelOpen, api.TunnelParams{MTU: 1500}, nil)
	if ref := refusal(t, err, "tunnel"); !strings.Contains(ref.Reason, "utun7") {
		t.Errorf("the race: %v", ref)
	}
}

func TestTunnelOpenTakesTheBaseOutOfTheIsolationTable(t *testing.T) {
	base, other := netip.MustParsePrefix("198.19.0.0/16"), netip.MustParsePrefix("10.183.0.0/16")
	r := newRig(t, nil)
	r.sys.With(func(s *fakesys.System) { s.Isolated = []netip.Prefix{other, base} })
	f, err := r.call(api.MethodTunnelOpen, api.TunnelParams{MTU: 1500}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	r.sys.With(func(s *fakesys.System) {
		if !slices.Equal(s.Isolated, []netip.Prefix{other}) {
			t.Errorf("the isolation table after tunnel.open: %v", s.Isolated)
		}
	})

	held := newRig(t, nil)
	held.sys.With(func(s *fakesys.System) {
		s.Isolated = []netip.Prefix{base}
		s.Ifaces = append(s.Ifaces, privhelper.Interface{Name: "bridge101", Index: 90, Addrs: []netip.Prefix{netip.MustParsePrefix("198.19.200.1/16")}})
	})
	f, err = held.call(api.MethodTunnelOpen, api.TunnelParams{MTU: 1500}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	held.sys.With(func(s *fakesys.System) {
		if len(s.Isolated) != 1 {
			t.Errorf("a live network's entry taken out: %v", s.Isolated)
		}
	})

	failing := newRig(t, nil)
	failing.sys.With(func(s *fakesys.System) { s.Fail = map[string]error{"unisolate": errors.New("pfctl: no")} })
	f, err = failing.call(api.MethodTunnelOpen, api.TunnelParams{MTU: 1500}, nil)
	if err != nil {
		t.Fatalf("a failing table edit failed the tunnel: %v", err)
	}
	f.Close()
}

func TestRoutesSetMakesExactlyTheListInsideTheBase(t *testing.T) {
	r := newRig(t, nil)
	tun := r.openTunnel()
	all := []string{"198.19.0.0/24", "198.19.2.0/24", "198.19.4.0/22", "198.19.64.0/18", "198.19.128.0/18"}
	var res api.Routes
	if _, err := r.call(api.MethodRoutesSet, api.RoutesParams{Interface: tun, Prefixes: all}, &res); err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Added, " ") != strings.Join(all, " ") || len(res.Removed) != 0 {
		t.Errorf("first set: %+v", res)
	}
	want := slices.Clone(all)
	slices.Sort(want)
	if got := r.routesOn(tun); !slices.Equal(got, want) {
		t.Errorf("routes %v, want %v", got, want)
	}
	less := []string{"198.19.0.0/24", "198.19.2.0/24", "198.19.4.0/22", "198.19.128.0/18"}
	res = api.Routes{}
	if _, err := r.call(api.MethodRoutesSet, api.RoutesParams{Interface: tun, Prefixes: less}, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 0 || strings.Join(res.Removed, " ") != "198.19.64.0/18" {
		t.Errorf("withdrawn: %+v", res)
	}
	res = api.Routes{}
	if _, err := r.call(api.MethodRoutesSet, api.RoutesParams{Interface: tun, Prefixes: less}, &res); err != nil || len(res.Added)+len(res.Removed) != 0 {
		t.Errorf("the same list again changed something: %+v %v", res, err)
	}
	res = api.Routes{}
	if _, err := r.call(api.MethodRoutesSet, api.RoutesParams{Interface: tun, Prefixes: []string{}}, &res); err != nil || len(res.Removed) != 4 || res.Prefixes == nil {
		t.Errorf("the empty list: %+v %v", res, err)
	}
	tab, _ := r.sys.Routes()
	if !slices.ContainsFunc(tab, func(rt privhelper.Route) bool { return rt.Prefix.String() == "198.19.3.2/32" }) {
		t.Error("the peer's route went with fev's")
	}

	before := r.routesOn(tun)
	tooMany := []string{}
	for i := range api.MaxRoutes + 1 {
		tooMany = append(tooMany, fmt.Sprintf("198.19.%d.0/24", 64+i))
	}
	for _, c := range []struct {
		iface    string
		prefixes []string
		arg      string
	}{
		{tun, []string{"8.8.8.0/24"}, "prefixes[0]"},
		{tun, []string{"198.19.0.0/24", "10.123.0.0/24"}, "prefixes[1]"},
		{tun, []string{"172.16.30.0/24"}, "prefixes[0]"},
		{tun, []string{"0.0.0.0/0"}, "prefixes[0]"},
		{tun, []string{"::/0"}, "prefixes[0]"},
		{tun, []string{"198.18.0.0/15"}, "prefixes[0]"},
		{tun, []string{"127.0.0.0/8"}, "prefixes[0]"},
		{tun, []string{"fe80::/64"}, "prefixes[0]"},
		{tun, []string{"/etc/resolver"}, "prefixes[0]"},
		{tun, tooMany, "prefixes"},
		{"en0", []string{"198.19.0.0/24"}, "interface"},
		{"lo0", []string{"198.19.0.0/24"}, "interface"},
		{"../utun0", []string{"198.19.0.0/24"}, "interface"},
		{"utun0/x", []string{"198.19.0.0/24"}, "interface"},
		{"utun0 ", []string{"198.19.0.0/24"}, "interface"},
		{"utun99", []string{"198.19.0.0/24"}, "interface"},
	} {
		_, err := r.call(api.MethodRoutesSet, api.RoutesParams{Interface: c.iface, Prefixes: c.prefixes}, &res)
		refusal(t, err, c.arg)
	}
	if got := r.routesOn(tun); !slices.Equal(got, before) {
		t.Errorf("a refusal changed the routes: %v, then %v", before, got)
	}

	r.sys.With(func(s *fakesys.System) {
		s.Ifaces[1].Addrs = append(s.Ifaces[1].Addrs, netip.MustParsePrefix("198.19.5.20/24"))
	})
	_, err := r.call(api.MethodRoutesSet, api.RoutesParams{Interface: tun, Prefixes: []string{"198.19.4.0/22"}}, &res)
	if ref := refusal(t, err, "prefixes[0]"); !strings.Contains(ref.Reason, "en0") || !strings.Contains(ref.Reason, "198.19.5.0/24") {
		t.Errorf("the connected network: %v", ref)
	}
	r.sys.With(func(s *fakesys.System) {
		s.Ifaces = append(s.Ifaces, privhelper.Interface{Name: "utun5", Index: 50, Addrs: []netip.Prefix{netip.MustParsePrefix("100.101.1.2/32")}})
		s.Table = append(s.Table,
			privhelper.Route{Prefix: netip.MustParsePrefix("198.19.70.0/24"), Index: 50, Flags: privhelper.RTFUp | privhelper.RTFStatic},
			privhelper.Route{Prefix: netip.MustParsePrefix("198.19.2.0/24"), Index: 50, Flags: privhelper.RTFUp | privhelper.RTFStatic | privhelper.RTFIfScope})
	})
	_, err = r.call(api.MethodRoutesSet, api.RoutesParams{Interface: tun, Prefixes: []string{"198.19.0.0/24", "198.19.64.0/18"}}, &res)
	if ref := refusal(t, err, "prefixes[1]"); !strings.Contains(ref.Reason, "198.19.70.0/24") || !strings.Contains(ref.Reason, "utun5") {
		t.Errorf("the more specific route: %v", ref)
	}
	_, err = r.call(api.MethodRoutesSet, api.RoutesParams{Interface: tun, Prefixes: []string{"198.19.2.0/24"}}, &res)
	refusal(t, err, "prefixes[0]")

	r.sys.With(func(s *fakesys.System) {
		s.Table = append(s.Table,
			privhelper.Route{Prefix: netip.MustParsePrefix("198.18.0.0/15"), Index: 50, Flags: privhelper.RTFUp | privhelper.RTFStatic},
			privhelper.Route{Prefix: netip.MustParsePrefix("198.19.0.9/32"), Index: 4, Flags: privhelper.RTFUp | privhelper.RTFHost | privhelper.RTFGateway | privhelper.RTFWasCloned})
	})
	if _, err := r.call(api.MethodRoutesSet, api.RoutesParams{Interface: tun, Prefixes: []string{"198.19.0.0/24", "198.19.128.0/18"}}, &res); err != nil {
		t.Errorf("under a covering route: %v", err)
	}
}

func TestABaseUnderAVPNsCoveringRoute(t *testing.T) {
	r := newRig(t, func(rec *api.InstallRecord) { rec.Base = "10.198.0.0/16"; rec.Base6 = "" })
	tun := r.openTunnel()
	r.sys.With(func(s *fakesys.System) {
		s.Ifaces = append(s.Ifaces, privhelper.Interface{Name: "utun4", Index: 40, Addrs: []netip.Prefix{netip.MustParsePrefix("10.8.0.2/32")}})
		s.Table = append(s.Table,
			privhelper.Route{Prefix: netip.MustParsePrefix("10.0.0.0/8"), Index: 40, Flags: privhelper.RTFUp | privhelper.RTFStatic},
			privhelper.Route{Prefix: netip.MustParsePrefix("10.198.4.0/24"), Index: 40, Flags: privhelper.RTFUp | privhelper.RTFStatic})
	})
	var res api.Routes
	if _, err := r.call(api.MethodRoutesSet, api.RoutesParams{Interface: tun, Prefixes: []string{"10.198.0.0/24", "10.198.64.0/18"}}, &res); err != nil {
		t.Errorf("inside the base under 10.0.0.0/8: %v", err)
	}
	_, err := r.call(api.MethodRoutesSet, api.RoutesParams{Interface: tun, Prefixes: []string{"10.198.4.0/22"}}, &res)
	refusal(t, err, "prefixes[0]")
	_, err = r.call(api.MethodRoutesSet, api.RoutesParams{Interface: tun, Prefixes: []string{"10.8.0.0/24"}}, &res)
	if ref := refusal(t, err, "prefixes[0]"); !strings.Contains(ref.Reason, "outside the address base") {
		t.Errorf("outside the base: %v", ref)
	}
}

func TestOnlyTheUserAndRootAreLetInAndParamsAreStrict(t *testing.T) {
	r := newRig(t, nil)
	var hello api.HelloResult
	if _, err := r.call(api.MethodHello, api.Empty{}, &hello); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.peer.UID = 0
	r.mu.Unlock()
	if _, err := r.call(api.MethodHello, api.Empty{}, &hello); err != nil {
		t.Errorf("root: %v", err)
	}
	r.mu.Lock()
	r.peer.UID = os.Getuid() + 1
	r.mu.Unlock()
	_, err := r.call(api.MethodHello, api.Empty{}, &hello)
	if err == nil || !strings.Contains(err.Error(), "without a reply") {
		t.Errorf("another uid: %v", err)
	}
	_, err = r.call(api.MethodTunnelOpen, api.TunnelParams{MTU: 1500}, nil)
	if err == nil || !strings.Contains(err.Error(), "without a reply") {
		t.Errorf("another uid's tunnel.open: %v", err)
	}
	if r.sys.PeerOf("utun0") >= 0 {
		t.Error("another uid's tunnel.open made a tunnel")
	}
	r.mu.Lock()
	r.peer.UID = os.Getuid()
	r.mu.Unlock()

	for _, c := range []struct {
		method string
		params string
	}{
		{api.MethodTunnelOpen, `{"mtu":1500,"path":"/etc/hosts"}`},
		{api.MethodHello, `{"command":"id"}`},
		{api.MethodRoutesSet, `["198.19.0.0/24"]`},
		{api.MethodPortBind, `{"proto":"tcp","family":4,"port":80,"address":"0.0.0.0"}`},
		{api.MethodLocalNamesSet, `{"names":["a.local"],"server":"10.0.0.1"}`},
		{api.MethodDockerSockLink, `{"target":"/etc/master.passwd"}`},
	} {
		_, err := r.call(c.method, json.RawMessage(c.params), nil)
		refusal(t, err, "params")
	}
	var e *api.Error
	if _, err := r.call(api.MethodResolverSet, api.Empty{}, nil); !errors.As(err, &e) || e.Code != api.CodeNotImplemented {
		t.Errorf("a pending verb: %v", err)
	}
	if _, err := r.call("exec", json.RawMessage(`{"argv":["/bin/sh"]}`), nil); !errors.As(err, &e) || e.Code != -32601 {
		t.Errorf("an unknown verb: %v", err)
	}
}

func TestPortBindIsLowPortsOnLoopback(t *testing.T) {
	r := newRig(t, nil)
	for _, p := range []api.PortParams{{Proto: "tcp", Family: 4, Port: 80}, {Proto: "udp", Family: 6, Port: 53}} {
		var b api.Bound
		f, err := r.call(api.MethodPortBind, p, &b)
		if err != nil {
			t.Fatalf("%+v: %v", p, err)
		}
		sa, err := syscall.Getsockname(int(f.Fd()))
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		switch a := sa.(type) {
		case *syscall.SockaddrInet4:
			if a.Addr != [4]byte{127, 0, 0, 1} || b.Address != "127.0.0.1:80" {
				t.Errorf("%+v: %v %+v", p, a.Addr, b)
			}
		case *syscall.SockaddrInet6:
			if netip.AddrFrom16(a.Addr) != netip.IPv6Loopback() || b.Address != "[::1]:53" {
				t.Errorf("%+v: %v %+v", p, a.Addr, b)
			}
		default:
			t.Errorf("%+v: %T", p, sa)
		}
	}
	for _, p := range []api.PortParams{{Proto: "tcp", Family: 4, Port: 0}, {Proto: "tcp", Family: 4, Port: 1024}, {Proto: "tcp", Family: 4, Port: 8080}, {Proto: "sctp", Family: 4, Port: 80}, {Proto: "tcp", Family: 5, Port: 80}} {
		_, err := r.call(api.MethodPortBind, p, nil)
		refusal(t, err, "")
	}
	if len(r.sys.Bound) != 2 {
		t.Errorf("bound %v", r.sys.Bound)
	}
}

func TestTheDockerSockLinkIsFevomsOrLeftAlone(t *testing.T) {
	r := newRig(t, func(rec *api.InstallRecord) { rec.DockerSockLink = true })
	link := filepath.Join(r.root, api.DockerSockPath)
	target := filepath.Join(r.home, ".fev/run/docker.sock")
	var st api.DockerSock
	rn := newRig(t, nil)
	_, err := rn.call(api.MethodDockerSockLink, api.Empty{}, &st)
	refusal(t, err, "docker_sock")
	if _, err := os.Lstat(filepath.Join(rn.root, api.DockerSockPath)); err == nil {
		t.Error("a link the install was not asked for was made")
	}
	rnLink := filepath.Join(rn.root, api.DockerSockPath)
	os.MkdirAll(filepath.Dir(rnLink), 0o755)
	os.Symlink(filepath.Join(rn.home, ".fev/run/docker.sock"), rnLink)
	_, err = rn.call(api.MethodDockerSockUnlink, api.Empty{}, &st)
	refusal(t, err, "docker_sock")
	if to, _ := os.Readlink(rnLink); to != filepath.Join(rn.home, ".fev/run/docker.sock") {
		t.Errorf("unlink removed a link the install was not asked for: %q", to)
	}
	if _, err := r.call(api.MethodDockerSockLink, api.Empty{}, &st); err != nil || st.State != api.StateFev || st.Target != target {
		t.Fatalf("link: %+v %v", st, err)
	}
	if to, _ := os.Readlink(link); to != target {
		t.Errorf("the link points at %q", to)
	}
	if _, err := r.call(api.MethodDockerSockUnlink, api.Empty{}, &st); err != nil || st.State != api.StateAbsent {
		t.Errorf("unlink: %+v %v", st, err)
	}
	other := "/Users/someone/.otherproduct/run/docker.sock"
	os.Symlink(other, link)
	_, err = r.call(api.MethodDockerSockLink, api.Empty{}, &st)
	refusal(t, err, "docker_sock")
	if _, err := r.call(api.MethodDockerSockUnlink, api.Empty{}, &st); err != nil || st.State != api.StateForeign || st.Target != other {
		t.Errorf("unlink of another's: %+v %v", st, err)
	}
	if to, _ := os.Readlink(link); to != other {
		t.Errorf("another product's link was changed to %q", to)
	}
	rc := newRig(t, func(rec *api.InstallRecord) { rec.DockerSockLink, rec.DockerSockConsent = true, true })
	link2 := filepath.Join(rc.root, api.DockerSockPath)
	os.Symlink(other, link2)
	if _, err := rc.call(api.MethodDockerSockLink, api.Empty{}, &st); err != nil || st.State != api.StateFev {
		t.Errorf("with consent: %+v %v", st, err)
	}
	os.Remove(link2)
	os.WriteFile(link2, nil, 0o600)
	_, err = rc.call(api.MethodDockerSockLink, api.Empty{}, &st)
	refusal(t, err, "docker_sock")
	if fi, err := os.Lstat(link2); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("a path that is not a link was replaced: %v %v", fi, err)
	}
	if des, _ := os.ReadDir(filepath.Dir(link2)); len(des) != 1 {
		t.Errorf("left beside the link: %v", des)
	}
}

func entryOf(t *testing.T, r *rig, name string) string {
	t.Helper()
	if r.form == api.ResolverFormStore {
		b, err := os.ReadFile(filepath.Join(r.root, privhelper.StoreStandIn, name+".json"))
		if err != nil {
			return ""
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(m["key"], m["SupplementalMatchDomains"], m["SupplementalMatchDomainsNoSearch"], m["ServerAddresses"], m["ServerPort"])
	}
	b, err := os.ReadFile(filepath.Join(r.root, "etc/resolver", name))
	if err != nil {
		return ""
	}
	return string(b)
}

func wantEntry(r *rig, name string) string {
	if r.form == api.ResolverFormStore {
		return fmt.Sprint(api.StoreKey(name), []any{name}, wantNoSearch(name), []any{"127.0.0.1"}, float64(15353))
	}
	return api.ResolverHeader + "\nnameserver 127.0.0.1\nport 15353\n"
}

func wantNoSearch(name string) any {
	if api.StoreSearches(name) {
		return nil
	}
	return float64(1)
}

func entryTime(t *testing.T, r *rig, name string) time.Time {
	t.Helper()
	if r.form == api.ResolverFormStore {
		es, err := privhelper.DirStore{Dir: filepath.Join(r.root, privhelper.StoreStandIn)}.Entries()
		if err != nil {
			t.Fatal(err)
		}
		return es[name].Written
	}
	fi, err := os.Stat(filepath.Join(r.root, "etc/resolver", name))
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

func withForm(form string) func(*api.InstallRecord) {
	return func(rec *api.InstallRecord) { rec.ResolverForm = form }
}

func bothForms(t *testing.T, f func(t *testing.T, form string)) {
	for _, form := range []string{api.ResolverFormStore, api.ResolverFormFile} {
		t.Run(form, func(t *testing.T) { f(t, form) })
	}
}

func names(ls api.LocalNames) string {
	var out []string
	for _, n := range ls.Names {
		s := n.Name + "=" + n.State
		if n.AnsweredBy != "" {
			s += "@" + n.AnsweredBy
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func TestLocalNamesAreProbedWrittenAndRemoved(t *testing.T) {
	bothForms(t, testLocalNamesAreProbedWrittenAndRemoved)
}

func testLocalNamesAreProbedWrittenAndRemoved(t *testing.T, form string) {
	r := newRig(t, withForm(form))
	r.sys.With(func(s *fakesys.System) {
		s.Answers["nas.local"] = []privhelper.Answer{{Kind: privhelper.ProbeMulticast, Type: "A", Addr: "172.16.30.20", Ifname: "en0"}}
		s.Answers["corp-dc.local"] = []privhelper.Answer{{Kind: privhelper.ProbeZone, Type: "SOA"}}
		s.Answers["ours.local"] = []privhelper.Answer{
			{Kind: privhelper.ProbeSystem, Type: "A", Addr: "198.19.4.9", Ifname: "en0"},
			{Kind: privhelper.ProbeMulticast, Type: "A", Addr: "10.0.0.1", Ifname: "utun0"},
		}
	})
	r.openTunnel()
	var res api.LocalNames
	if _, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{"App-Custom.local.", "nas.local", "corp-dc.local", "ours.local", "app-custom.local"}}, &res); err != nil {
		t.Fatal(err)
	}
	if got := names(res); got != "app-custom.local=present corp-dc.local=refused nas.local=refused@172.16.30.20 ours.local=present" {
		t.Errorf("result: %s", got)
	}
	for _, n := range res.Names {
		if n.State == api.NameRefused && n.Reason == "" {
			t.Errorf("refused with no reason: %+v", n)
		}
	}
	if got, want := entryOf(t, r, "app-custom.local"), wantEntry(r, "app-custom.local"); got != want {
		t.Errorf("the entry is %q", got)
	}
	if entryOf(t, r, "nas.local") != "" || entryOf(t, r, "corp-dc.local") != "" {
		t.Error("a refused name has an entry")
	}
	if p := r.sys.Probes()[0]; len(p.Kinds) != 3 || len(p.Names) != 4 {
		t.Errorf("the first probe: %+v", p)
	}

	os.MkdirAll(filepath.Join(r.root, "etc/resolver"), 0o755)
	os.WriteFile(filepath.Join(r.root, "etc/resolver/vpn.local"), []byte("nameserver 10.8.0.1\n"), 0o644)
	if _, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{}}, &res); err != nil {
		t.Fatal(err)
	}
	if got := names(res); got != "app-custom.local=removed ours.local=removed" {
		t.Errorf("removal: %s", got)
	}
	des, _ := os.ReadDir(filepath.Join(r.root, "etc/resolver"))
	if len(des) != 1 || des[0].Name() != "vpn.local" {
		t.Errorf("left in the directory: %v", des)
	}
	if entryOf(t, r, "app-custom.local") != "" || entryOf(t, r, "ours.local") != "" {
		t.Error("an entry stayed after []")
	}
	if len(r.sys.Probes()) != 1 {
		t.Errorf("a removal probed: %+v", r.sys.Probes())
	}
}

func TestMalformedLocalNamesRefuseTheCall(t *testing.T) {
	bothForms(t, testMalformedLocalNamesRefuseTheCall)
}

func testMalformedLocalNamesRefuseTheCall(t *testing.T, form string) {
	r := newRig(t, withForm(form))
	var res api.LocalNames
	if _, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{"keep.local"}}, &res); err != nil {
		t.Fatal(err)
	}
	r.advance(2 * time.Second)
	for _, bad := range []string{"_tcp.local", "_http._tcp.local", "web.fev.local", "fev.local", "Fev-Test-Mac.local", "app.test", "example.com", "local", "../x.local", "a b.local", "*.app.local", ""} {
		_, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{"fine.local", bad}}, &res)
		if ref := refusal(t, err, "names[1]"); ref.Value != bad {
			t.Errorf("%q: %+v", bad, ref)
		}
	}
	var many []string
	for i := range api.MaxLocalNames + 1 {
		many = append(many, fmt.Sprintf("n%d.local", i))
	}
	_, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: many}, &res)
	refusal(t, err, "names")
	if entryOf(t, r, "keep.local") == "" || entryOf(t, r, "fine.local") != "" {
		t.Error("a refused call changed the entries")
	}
}

func TestLocalNamesOverlappingAnotherResolverAreRefused(t *testing.T) {
	r := newRig(t, nil)
	r.sys.With(func(s *fakesys.System) {
		s.Resolvers = []privhelper.Resolver{
			{Source: "State:/Network/Service/VPN/DNS", Domains: []string{"corp.local."}, Search: []string{"Office.Local"}},
			{Source: "State:/Network/Global/DNS", Domains: []string{"local"}, Search: []string{"localdomain", "tail4c6299.ts.net"}},
			{Source: "Setup:/Network/Service/X/DNS", Domains: []string{"a.deep.local"}},
		}
	})
	os.MkdirAll(filepath.Join(r.root, "etc/resolver"), 0o755)
	os.WriteFile(filepath.Join(r.root, "etc/resolver/nas.local"), []byte("nameserver 10.0.0.2\nsearch home.local\n"), 0o644)
	var res api.LocalNames
	ask := []string{"corp.local", "dc1.corp.local", "office.local", "printer.office.local", "deep.local", "nas.local", "x.home.local", "free.local", "localdomain.local"}
	if _, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: ask}, &res); err != nil {
		t.Fatal(err)
	}
	for _, n := range res.Names {
		wantFree := n.Name == "free.local" || n.Name == "localdomain.local"
		if (n.State == api.NamePresent) != wantFree {
			t.Errorf("%s: %s (%s)", n.Name, n.State, n.Reason)
		}
		if !wantFree && !strings.Contains(n.Reason, "configured") {
			t.Errorf("%s: reason %q", n.Name, n.Reason)
		}
	}
	if p := r.sys.Probes()[0]; strings.Join(p.Names, " ") != "free.local localdomain.local" {
		t.Errorf("probed %v: a name refused by the configuration was probed", p.Names)
	}
}

func TestANetworkChangeChecksPresentNamesAgain(t *testing.T) {
	bothForms(t, testANetworkChangeChecksPresentNamesAgain)
}

func testANetworkChangeChecksPresentNamesAgain(t *testing.T, form string) {
	r := newRig(t, withForm(form))
	var res api.LocalNames
	if _, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{"app.local", "shop.local", "cam.local"}}, &res); err != nil {
		t.Fatal(err)
	}
	r.advance(5 * time.Second)
	if _, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{"app.local", "shop.local", "cam.local"}}, &res); err != nil || len(r.sys.Probes()) != 1 {
		t.Errorf("an unchanged set probed again: %v %+v", err, r.sys.Probes())
	}
	r.sys.With(func(s *fakesys.System) {
		s.Resolvers = []privhelper.Resolver{{Source: "State:/Network/Service/VPN/DNS", Domains: []string{"shop.local"}}}
		s.Answers["cam.local"] = []privhelper.Answer{{Kind: privhelper.ProbeMulticast, Type: "AAAA", Addr: "fe80::1", Ifname: "en0"}}
		s.Answers["app.local"] = []privhelper.Answer{{Kind: privhelper.ProbeSystem, Type: "A", Addr: "192.168.1.9"}}
	})
	if _, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{"app.local", "shop.local", "cam.local"}, NetworkChanged: true}, &res); err != nil {
		t.Fatal(err)
	}
	if got := names(res); got != "app.local=present cam.local=removed@fe80::1 shop.local=removed" {
		t.Errorf("after the change: %s", got)
	}
	probes := r.sys.Probes()
	last := probes[len(probes)-1]
	if len(last.Kinds) != 1 || last.Kinds[0] != privhelper.ProbeMulticast || strings.Join(last.Names, " ") != "app.local cam.local" {
		t.Errorf("the re-probe: %+v", last)
	}
	if entryOf(t, r, "app.local") == "" || entryOf(t, r, "cam.local") != "" || entryOf(t, r, "shop.local") != "" {
		t.Error("the entries do not follow the re-check")
	}
}

func TestAddingNamesIsRateLimitedAndAFailedProbeTakesNothing(t *testing.T) {
	bothForms(t, testAddingNamesIsRateLimitedAndAFailedProbeTakesNothing)
}

func testAddingNamesIsRateLimitedAndAFailedProbeTakesNothing(t *testing.T, form string) {
	r := newRig(t, withForm(form))
	r.mu.Lock()
	r.now = time.Now()
	r.mu.Unlock()
	var res api.LocalNames
	if _, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{"a.local"}}, &res); err != nil {
		t.Fatal(err)
	}
	written := entryTime(t, r, "a.local")
	r.mu.Lock()
	r.now = written.Add(300 * time.Millisecond)
	r.mu.Unlock()
	var e *api.Error
	if _, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{"a.local", "b.local"}}, &res); !errors.As(err, &e) || e.Code != api.CodeTooSoon {
		t.Errorf("too soon: %v", err)
	}
	if _, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{"a.local"}}, &res); err != nil {
		t.Errorf("the same set at once: %v", err)
	}
	r.advance(time.Second)
	r.sys.With(func(s *fakesys.System) { s.ProbeErr = errors.New("mDNSResponder is not there") })
	if _, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{"a.local", "b.local"}}, &res); !errors.As(err, &e) || e.Code != api.CodeFailed {
		t.Errorf("a failed probe: %v", err)
	}
	if entryOf(t, r, "b.local") != "" {
		t.Error("a name was taken without a probe")
	}
	if _, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{}}, &res); err != nil {
		t.Errorf("removing: %v", err)
	}
}

func TestHelloSaysWhatIsInstalledAndPresent(t *testing.T) {
	r := newRig(t, func(rec *api.InstallRecord) { rec.DockerSockLink = true; rec.ResolverForm = api.ResolverFormFile })
	var h api.HelloResult
	if _, err := r.call(api.MethodHello, api.Empty{}, &h); err != nil {
		t.Fatal(err)
	}
	if h.ProtocolVersion != api.ProtocolVersion || h.UID != os.Getuid() || h.Base != "198.19.0.0/16" || h.Resolver != api.StateAbsent || h.DockerSock != api.StateAbsent || !h.DockerSockLink || len(h.LocalNames) != 0 || h.LocalNames == nil {
		t.Errorf("hello: %+v", h)
	}
	os.MkdirAll(filepath.Join(r.root, "etc/resolver"), 0o755)
	os.WriteFile(filepath.Join(r.root, api.ResolverPath), privhelper.ResolverFileText(15353), 0o644)
	var res api.LocalNames
	r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{"z.local", "a.local"}}, &res)
	r.call(api.MethodDockerSockLink, api.Empty{}, nil)
	if _, err := r.call(api.MethodHello, api.Empty{}, &h); err != nil {
		t.Fatal(err)
	}
	if h.Resolver != api.StateFev || h.DockerSock != api.StateFev || strings.Join(h.LocalNames, " ") != "a.local z.local" {
		t.Errorf("hello: %+v", h)
	}
	os.WriteFile(filepath.Join(r.root, api.ResolverPath), []byte("nameserver 1.1.1.1\n"), 0o644)
	if _, err := r.call(api.MethodHello, api.Empty{}, &h); err != nil || h.Resolver != api.StateForeign {
		t.Errorf("a foreign resolver file: %+v %v", h, err)
	}
}

func TestTheBootPassRemovesEntriesAndMakesTheLinkAgain(t *testing.T) {
	r := newRig(t, func(rec *api.InstallRecord) { rec.DockerSockLink = true; rec.ResolverForm = api.ResolverFormFile })
	var res api.LocalNames
	r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{"a.local", "b.local"}}, &res)
	os.WriteFile(filepath.Join(r.root, "etc/resolver/vpn.local"), []byte("nameserver 10.8.0.1\n"), 0o644)
	os.WriteFile(filepath.Join(r.root, api.ResolverPath), privhelper.ResolverFileText(15353), 0o644)
	if err := r.srv.BootPass(); err != nil {
		t.Fatal(err)
	}
	des, _ := os.ReadDir(filepath.Join(r.root, "etc/resolver"))
	var left []string
	for _, d := range des {
		left = append(left, d.Name())
	}
	if strings.Join(left, " ") != "fev.local vpn.local" {
		t.Errorf("left: %v", left)
	}
	if to, _ := os.Readlink(filepath.Join(r.root, api.DockerSockPath)); to != filepath.Join(r.home, ".fev/run/docker.sock") {
		t.Errorf("the link is %q", to)
	}
	r2 := newRig(t, nil)
	if err := r2.srv.BootPass(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(r2.root, api.DockerSockPath)); err == nil {
		t.Error("the boot pass made a link the user did not ask for")
	}
}

func TestABadRecordMakesNoServer(t *testing.T) {
	good := api.InstallRecord{UID: 501, Home: "/Users/x", Base: "198.19.0.0/16", ResolverPort: 15353}
	for _, edit := range []func(*api.InstallRecord){
		func(r *api.InstallRecord) { r.UID = 0 },
		func(r *api.InstallRecord) { r.UID = -1 },
		func(r *api.InstallRecord) { r.Home = "Users/x" },
		func(r *api.InstallRecord) { r.Home = "/Users/x/../root" },
		func(r *api.InstallRecord) { r.ResolverPort = 53 },
		func(r *api.InstallRecord) { r.Base = "8.8.0.0/16" },
		func(r *api.InstallRecord) { r.Base = "198.19.0.0/15" },
		func(r *api.InstallRecord) { r.Base = "" },
		func(r *api.InstallRecord) { r.Base6 = "2001:db8::/48" },
		func(r *api.InstallRecord) { r.ResolverForm = api.ResolverFormTunnel },
		func(r *api.InstallRecord) { r.ResolverForm = "../x" },
	} {
		r := good
		edit(&r)
		if _, err := privhelper.New(privhelper.Config{Record: r, Sys: fakesys.New()}); err == nil {
			t.Errorf("%+v made a server", r)
		}
	}
	if _, err := privhelper.New(privhelper.Config{Record: good, Sys: fakesys.New()}); err != nil {
		t.Error(err)
	}
	for _, form := range []string{api.ResolverFormFile, api.ResolverFormStore} {
		good.ResolverForm = form
		if _, err := privhelper.New(privhelper.Config{Record: good, Sys: fakesys.New()}); err != nil {
			t.Error(err)
		}
	}
}

func TestTheStoreFormKeepsFevomLocalsEntry(t *testing.T) {
	r := newRig(t, func(rec *api.InstallRecord) { rec.DockerSockLink = true })
	store := privhelper.DirStore{Dir: filepath.Join(r.root, privhelper.StoreStandIn)}
	var h api.HelloResult
	if _, err := r.call(api.MethodHello, api.Empty{}, &h); err != nil || h.Resolver != api.StateAbsent {
		t.Fatalf("hello before the boot pass: %+v %v", h, err)
	}
	if err := r.srv.BootPass(); err != nil {
		t.Fatal(err)
	}
	if got, want := entryOf(t, r, "fev.local"), wantEntry(r, "fev.local"); got != want {
		t.Errorf("the boot pass's fev.local entry: %s, want %s", got, want)
	}
	if _, err := r.call(api.MethodHello, api.Empty{}, &h); err != nil || h.Resolver != api.StateFev || len(h.LocalNames) != 0 {
		t.Errorf("hello after the boot pass: %+v %v", h, err)
	}
	if to, _ := os.Readlink(filepath.Join(r.root, api.DockerSockPath)); to != filepath.Join(r.home, ".fev/run/docker.sock") {
		t.Errorf("the link is %q", to)
	}

	r.sys.With(func(s *fakesys.System) {
		s.Resolvers = []privhelper.Resolver{
			{Source: api.StoreKey("fev.local"), Domains: []string{"fev.local"}},
			{Source: api.StoreKey("app.local"), Domains: []string{"app.local"}},
			{Source: api.StoreKey("x.app.local"), Domains: []string{"x.app.local"}},
			{Source: "State:/Network/Global/DNS", Search: []string{"corp.example", "fev.local", "app.local"}},
		}
	})
	var res api.LocalNames
	if _, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{"app.local", "x.app.local"}}, &res); err != nil {
		t.Fatal(err)
	}
	if got := names(res); got != "app.local=present x.app.local=present" {
		t.Errorf("fev's own entries counted as another's: %s", got)
	}
	r.advance(2 * time.Second)
	if _, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{"app.local", "x.app.local"}, NetworkChanged: true}, &res); err != nil {
		t.Fatal(err)
	}
	if got := names(res); got != "app.local=present x.app.local=present" {
		t.Errorf("after a network change, fev's own entries counted as another's: %s", got)
	}
	for _, n := range []string{"app.local", "x.app.local"} {
		if got, want := entryOf(t, r, n), wantEntry(r, n); got != want {
			t.Errorf("%s's entry: %s, want %s", n, got, want)
		}
	}
	if _, err := r.call(api.MethodHello, api.Empty{}, &h); err != nil || strings.Join(h.LocalNames, " ") != "app.local x.app.local" {
		t.Errorf("hello's names: %+v %v", h, err)
	}

	store.Remove("fev.local")
	r.advance(2 * time.Second)
	if _, err := r.call(api.MethodLocalNamesSet, api.LocalNamesParams{Names: []string{"app.local", "x.app.local"}}, &res); err != nil {
		t.Fatal(err)
	}
	if entryOf(t, r, "fev.local") == "" {
		t.Error("localnames.set did not put fev.local's entry back")
	}

	if err := r.srv.BootPass(); err != nil {
		t.Fatal(err)
	}
	if es, _ := store.Entries(); len(es) != 1 || es["fev.local"].Port != 15353 {
		t.Errorf("after the boot pass: %v", es)
	}

	store.Remove("fev.local")
	r.sys.With(func(s *fakesys.System) {
		s.Resolvers = []privhelper.Resolver{{Source: "State:/Network/Service/VPN/DNS", Domains: []string{"Fev.Local."}}}
	})
	if err := r.srv.BootPass(); err != nil {
		t.Fatal(err)
	}
	if entryOf(t, r, "fev.local") != "" {
		t.Error("fev.local's entry made beside another program's resolver for it")
	}
	if _, err := r.call(api.MethodHello, api.Empty{}, &h); err != nil || h.Resolver != api.StateForeign {
		t.Errorf("hello beside another's store entry: %+v %v", h, err)
	}
	r.sys.With(func(s *fakesys.System) { s.Resolvers = nil })
	os.MkdirAll(filepath.Join(r.root, "etc/resolver"), 0o755)
	os.WriteFile(filepath.Join(r.root, api.ResolverPath), []byte("nameserver 1.1.1.1\n"), 0o644)
	if err := r.srv.BootPass(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.call(api.MethodHello, api.Empty{}, &h); err != nil || h.Resolver != api.StateForeign || entryOf(t, r, "fev.local") != "" {
		t.Errorf("hello beside another's file: %+v %v", h, err)
	}
}

func TestTheStandInStoreRoundTrips(t *testing.T) {
	dir := t.TempDir()
	st := privhelper.DirStore{Dir: filepath.Join(dir, "store")}
	at := time.Date(2026, 10, 17, 9, 0, 0, 123456789, time.UTC)
	if err := st.Set("shop.local", 15354, at); err != nil {
		t.Fatal(err)
	}
	es, err := st.Entries()
	if err != nil || len(es) != 1 || es["shop.local"].Port != 15354 || !es["shop.local"].Written.Equal(at) {
		t.Errorf("entries: %v %v", es, err)
	}
	if err := st.Remove("shop.local"); err != nil {
		t.Fatal(err)
	}
	if err := st.Remove("shop.local"); err != nil {
		t.Errorf("removing twice: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "store", "x.local.json"), []byte(`{"key":"State:/Network/Service/VPN/DNS"}`), 0o644)
	if _, err := st.Entries(); err == nil {
		t.Error("a file that is not an entry of fev's was read as one")
	}
}
