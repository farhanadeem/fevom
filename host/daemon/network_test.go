package daemon

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	api "fev/proto/daemon"
	"fev/proto/netplan"
	priv "fev/proto/privhelper"
)

type fakeMac struct {
	mu        sync.Mutex
	foreign   map[netip.Prefix]string
	nat64     []netip.Prefix
	subs      []func()
	refreshes int
}

func (m *fakeMac) NAT64() []netip.Prefix {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.nat64)
}

func (m *fakeMac) setNAT64(ps ...string) {
	m.mu.Lock()
	m.nat64 = nil
	for _, p := range ps {
		m.nat64 = append(m.nat64, netip.MustParsePrefix(p))
	}
	subs := slices.Clone(m.subs)
	m.mu.Unlock()
	for _, fn := range subs {
		fn()
	}
}

func (m *fakeMac) Check(ours []netip.Prefix, tunnel string) ([]netip.Prefix, []api.RouteConflict, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keep []netip.Prefix
	var conflicts []api.RouteConflict
	var warnings []string
	for _, p := range ours {
		withdrawn := false
		for r, iface := range m.foreign {
			if !r.Overlaps(p) {
				continue
			}
			in := r.Bits() >= p.Bits()
			withdrawn = withdrawn || in
			conflicts = append(conflicts, api.RouteConflict{Prefix: p.String(), Route: r.String(), Interface: iface, Withdrawn: in})
			warnings = append(warnings, "the Mac's route "+r.String()+" on "+iface+" meets fev's "+p.String())
		}
		if !withdrawn {
			keep = append(keep, p)
		}
	}
	return keep, conflicts, warnings
}

func (m *fakeMac) Taken(string) []netip.Prefix {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []netip.Prefix
	for r := range m.foreign {
		out = append(out, r)
	}
	return out
}

func (m *fakeMac) Subscribe(fn func()) func() {
	m.mu.Lock()
	m.subs = append(m.subs, fn)
	m.mu.Unlock()
	return func() {}
}

func (m *fakeMac) Refresh(string) {
	m.mu.Lock()
	m.refreshes++
	m.mu.Unlock()
}

func (m *fakeMac) set(route, iface string) {
	m.mu.Lock()
	if route == "" {
		m.foreign = map[netip.Prefix]string{}
	} else {
		m.foreign[netip.MustParsePrefix(route)] = iface
	}
	subs := slices.Clone(m.subs)
	m.mu.Unlock()
	for _, fn := range subs {
		fn()
	}
}

type fakeSplice struct {
	mu       sync.Mutex
	attached string
	ends     TunnelEnds
	mtu      int
	detached int
	file     *os.File
	prefixes []netip.Prefix
	nat64    []netip.Prefix
}

func (s *fakeSplice) Attach(f *os.File, e TunnelEnds) {
	s.mu.Lock()
	s.attached = e.Interface + " " + e.Mac.String() + " " + e.Peer.String()
	s.ends = e
	s.mtu = e.MTU
	s.file = f
	s.mu.Unlock()
}

func (s *fakeSplice) SetNAT64(ps []netip.Prefix) {
	s.mu.Lock()
	s.nat64 = append([]netip.Prefix(nil), ps...)
	s.mu.Unlock()
}

func (s *fakeSplice) SetPrefixes(ps []netip.Prefix) {
	s.mu.Lock()
	s.prefixes = append([]netip.Prefix(nil), ps...)
	s.mu.Unlock()
}

func (s *fakeSplice) Detach() {
	s.mu.Lock()
	s.detached++
	if s.file != nil {
		s.file.Close()
		s.file = nil
	}
	s.mu.Unlock()
}

func direct(t *testing.T, words string) (*fixture, *Network, *StandInHelper, *fakeMac, *fakeSplice) {
	t.Helper()
	return directMTU(t, words, 0)
}

func directMTU(t *testing.T, words string, mtu int) (*fixture, *Network, *StandInHelper, *fakeMac, *fakeSplice) {
	t.Helper()
	f := newFixture(t)
	h, err := NewStandInHelper(words)
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(f.layout.RunDir(), "ph.sock")
	os.MkdirAll(filepath.Dir(sock), 0o700)
	if err := h.Listen(sock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	mac := &fakeMac{foreign: map[netip.Prefix]string{}}
	splice := &fakeSplice{}
	n := &Network{Helper: SocketHelper{Path: sock}, Mac: mac, Tunnel: splice, MTU: mtu}
	f.opts.Modules = []Module{n}
	f.start()
	return f, n, h, mac, splice
}

func (f *fixture) network(ok func(*api.NetworkStatus) bool) *api.NetworkStatus {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := f.d.hub.Status()
		if st.Network != nil && ok(st.Network) {
			return st.Network
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("the network block is %+v\n%s", st.Network, f.logFile("fevd.log"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func routed() []string {
	var out []string
	for _, p := range netplan.Default().Routed() {
		out = append(out, p.String())
	}
	return out
}

func TestDirectAccessFollowsTheHelperTheMacAndTheSetting(t *testing.T) {
	f, n, h, mac, splice := direct(t, "")
	if st := f.d.hub.Status(); st.Network != nil {
		t.Errorf("before a VM start: %+v", st.Network)
	}
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	block := f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOn && len(b.Routes) > 0 })
	if !slices.Equal(block.Routes, routed()) || block.Interface != "utun99" || block.Subnet != "198.19.0.0/16" || block.Helper == nil || block.Helper.Outdated {
		t.Errorf("on: %+v", block)
	}
	if !slices.Equal(h.Routes(), routed()) || splice.attached != "utun99 198.19.3.1 198.19.3.2" {
		t.Errorf("the helper routes %v; the splice has %q", h.Routes(), splice.attached)
	}
	if got := strings.Join(h.Calls(), " "); got != "hello tunnel.open routes.set" {
		t.Errorf("calls %q", got)
	}

	mac.set("198.19.64.0/24", "utun4")
	block = f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectDegraded })
	if slices.Contains(block.Routes, "198.19.64.0/18") || len(block.Routes) != len(routed())-1 || slices.Contains(h.Routes(), "198.19.64.0/18") ||
		len(block.Conflicts) != 1 || !block.Conflicts[0].Withdrawn || block.Conflicts[0].Route != "198.19.64.0/24" {
		t.Errorf("degraded: %+v; the helper routes %v", block, h.Routes())
	}
	if w := strings.Join(f.d.hub.Status().Warnings, "\n"); !strings.Contains(w, "198.19.64.0/24 on utun4 meets fev's 198.19.64.0/18") {
		t.Errorf("warnings %q", w)
	}
	mac.set("", "")
	mac.set("198.18.0.0/15", "utun5")
	block = f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOn && len(b.Conflicts) > 0 })
	if !slices.Equal(h.Routes(), routed()) || block.Conflicts[0].Withdrawn {
		t.Errorf("a covering route: %+v; the helper routes %v", block, h.Routes())
	}
	mac.set("", "")
	f.network(func(b *api.NetworkStatus) bool { return len(b.Conflicts) == 0 })
	if w := f.d.hub.Status().Warnings; len(w) != 0 {
		t.Errorf("warnings left: %q", w)
	}

	f.client().VMStop(bg, api.StopParams{TimeoutMS: 5000})
	if b := f.d.hub.Status().Network; b.DirectAccess != api.DirectOn || h.TunnelClosed() {
		t.Errorf("after a VM stop: %+v", b)
	}
	if _, err := f.client().ConfigSet(bg, map[string]any{api.KeyNetworkDirectAccess: false}); err != nil {
		t.Fatal(err)
	}
	block = f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOff })
	if block.Reason != api.DirectSetting || block.Interface != "" || len(block.Routes) != 0 || !h.TunnelClosed() || splice.detached != 1 {
		t.Errorf("off: %+v; closed %v", block, h.TunnelClosed())
	}
	calls := len(h.Calls())
	if _, err := f.client().ConfigSet(bg, map[string]any{api.KeyNetworkDirectAccess: true}); err != nil {
		t.Fatal(err)
	}
	f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOn && len(b.Routes) > 0 })
	if got := h.Calls()[calls:]; strings.Join(got, " ") != "hello tunnel.open routes.set" {
		t.Errorf("on again: %v", got)
	}
	calls = len(h.Calls())
	time.Sleep(100 * time.Millisecond)
	if len(h.Calls()) != calls {
		t.Errorf("calls nobody caused: %v", h.Calls()[calls:])
	}
	if _, err := f.client().ConfigSet(bg, map[string]any{api.KeyNetworkSubnet: "10.198.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client().VMRestart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	block = f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOff })
	if block.Reason != api.DirectFailed || !h.TunnelClosed() ||
		!strings.Contains(strings.Join(f.d.hub.Status().Warnings, "\n"), `installed for the addresses 198.19.0.0/16 and fev now uses 10.198.0.0/16: run "sudo fev setup --admin" again`) {
		t.Errorf("a new base: %+v %q", block, f.d.hub.Status().Warnings)
	}
	f.client().ConfigReset(bg, api.KeyNetworkSubnet)
	f.client().VMRestart(bg, api.StartParams{})
	f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOn && len(b.Routes) > 0 })
	f.d.Close()
	if !h.TunnelClosed() {
		t.Error("the daemon's exit left its tunnel open")
	}
	_ = n.directRoutes()
}

func TestDirectAccessSaysWhyItIsOff(t *testing.T) {
	f, _, h, _, _ := direct(t, "")
	if _, err := f.client().ConfigSet(bg, map[string]any{api.KeyAdminEnabled: false}); err != nil {
		t.Fatal(err)
	}
	f.client().VMStart(bg, api.StartParams{})
	block := f.network(func(b *api.NetworkStatus) bool { return b.Reason == api.DirectAdminOff })
	time.Sleep(50 * time.Millisecond)
	if len(h.Calls()) != 0 || block.DirectAccess != api.DirectOff {
		t.Errorf("admin off: %+v, calls %v", block, h.Calls())
	}

	for _, c := range []struct {
		words, reason, warning string
	}{
		{"version=99", api.DirectOutdated, `admin helper out of date: run "sudo fev setup --admin"`},
		{"base=10.198.0.0/16", api.DirectFailed, "installed for the addresses 10.198.0.0/16 and fev now uses 198.19.0.0/16"},
		{"refuse=tunnel.open", api.DirectFailed, "did not open the tunnel"},
	} {
		f, _, _, _, _ := direct(t, c.words)
		f.client().VMStart(bg, api.StartParams{})
		block := f.network(func(b *api.NetworkStatus) bool { return b.Reason != "" })
		if block.Reason != c.reason || !strings.Contains(strings.Join(f.d.hub.Status().Warnings, "\n"), c.warning) {
			t.Errorf("%s: %+v, warnings %q", c.words, block, f.d.hub.Status().Warnings)
		}
	}
	f2, _, _, _, _ := direct(t, "level=0")
	f2.client().VMStart(bg, api.StartParams{})
	block = f2.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOn })
	if !block.Helper.Outdated || !strings.Contains(strings.Join(f2.d.hub.Status().Warnings, "\n"), "lacks a fix") {
		t.Errorf("an older level: %+v", block)
	}

	f3 := newFixture(t)
	sock := filepath.Join(f3.layout.RunDir(), "ph.sock")
	f3.opts.Modules = []Module{&Network{Helper: SocketHelper{Path: sock}}}
	f3.start()
	f3.client().VMStart(bg, api.StartParams{})
	block = f3.network(func(b *api.NetworkStatus) bool { return b.Reason != "" })
	if block.Reason != api.DirectNoHelper || len(f3.d.hub.Status().Warnings) != 0 {
		t.Errorf("no helper: %+v %q", block, f3.d.hub.Status().Warnings)
	}
	h3, _ := NewStandInHelper("")
	if err := h3.Listen(sock); err != nil {
		t.Fatal(err)
	}
	defer h3.Close()
	f3.client().VMStop(bg, api.StopParams{TimeoutMS: 5000})
	f3.client().VMStart(bg, api.StartParams{})
	f3.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOn })
	if !slices.Equal(h3.Routes(), routed()) {
		t.Errorf("routes %v", h3.Routes())
	}
	if helperReason(&os.SyscallError{Syscall: "connect", Err: errConnRefused}) != api.DirectHelperOff {
		t.Error("ECONNREFUSED")
	}
}

var errConnRefused = syscall.ECONNREFUSED

func TestTheTunnelIsAskedForAtTheDaemonsMTU(t *testing.T) {
	for _, mtu := range []int{0, 16384, 65535} {
		f, _, _, _, splice := directMTU(t, "", mtu)
		if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
			t.Fatal(err)
		}
		f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOn })
		want := mtu
		if want == 0 {
			want = tunnelMTU
		}
		splice.mu.Lock()
		got := splice.mtu
		splice.mu.Unlock()
		if got != want {
			t.Errorf("asked at %d: the splice was given a tunnel of MTU %d, want %d", mtu, got, want)
		}
	}
}

func TestTheStandInsTunnelTakesWhatAUtunTakes(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	h, err := NewStandInHelper("")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "ph.sock")
	if err := h.Listen(sock); err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var tun priv.Tunnel
	f, err := priv.Call(ctx, sock, priv.MethodTunnelOpen, priv.TunnelParams{MTU: 65535}, &tun)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(make([]byte, 4+65535)); err != nil {
		t.Errorf("a packet of 65,535 bytes written at the daemon's end: %v", err)
	}
	peer := h.lastPeer()
	if peer == nil {
		t.Fatal("no stand-in end")
	}
	raw, _ := peer.SyscallConn()
	for i := 0; i < 200; i++ {
		var werr error
		raw.Control(func(fd uintptr) { _, werr = syscall.Write(int(fd), make([]byte, 4+1500)) })
		if werr != nil {
			t.Fatalf("packet %d of a burst of 200 from the Mac's end: %v", i, werr)
		}
	}
}

func TestTheStandInsTunnelIsNeverInheritedByAChild(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	h, err := NewStandInHelper("")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "ph.sock")
	if err := h.Listen(sock); err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var tun priv.Tunnel
	f, err := priv.Call(ctx, sock, priv.MethodTunnelOpen, priv.TunnelParams{MTU: 1500}, &tun)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	peer := h.lastPeer()
	if peer == nil {
		t.Fatal("no stand-in end")
	}
	raw, err := peer.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags uintptr
	var errno syscall.Errno
	raw.Control(func(fd uintptr) {
		flags, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0)
	})
	if errno != 0 || flags&syscall.FD_CLOEXEC == 0 {
		t.Errorf("the stand-in's end of the tunnel: flags %#x %v, want close-on-exec", flags, errno)
	}
}
