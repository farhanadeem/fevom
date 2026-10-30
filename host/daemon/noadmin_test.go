package daemon

import (
	"fmt"
	"log/slog"
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
)

type fakeNoAdmin struct {
	mu       sync.Mutex
	opens    int
	closes   int
	open     bool
	err      error
	prefixes []netip.Prefix
	stopped  func()
	plans    []string
}

func (p *fakeNoAdmin) Open(plan netplan.Plan, _ *slog.Logger, stopped func()) (*os.File, string, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.plans = append(p.plans, plan.Base().String())
	if p.err != nil {
		return nil, "", 0, p.err
	}
	if p.open {
		return nil, "", 0, fmt.Errorf("opened twice")
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, "", 0, err
	}
	syscall.Close(fds[1])
	p.opens++
	p.open = true
	p.stopped = stopped
	return os.NewFile(uintptr(fds[0]), "fake-vmnet"), "bridge77", 16384, nil
}

func (p *fakeNoAdmin) SetPrefixes(ps []netip.Prefix) {
	p.mu.Lock()
	p.prefixes = slices.Clone(ps)
	p.mu.Unlock()
}

func (p *fakeNoAdmin) Close() {
	p.mu.Lock()
	p.closes++
	p.open = false
	p.mu.Unlock()
}

func (p *fakeNoAdmin) state() (opens, closes int, open bool, ps []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, x := range p.prefixes {
		ps = append(ps, x.String())
	}
	return p.opens, p.closes, p.open, ps
}

func noAdmin(t *testing.T) (*fixture, string, *fakeNoAdmin, *fakeMac, *fakeSplice) {
	t.Helper()
	f := newFixture(t)
	sock := filepath.Join(f.layout.RunDir(), "ph.sock")
	os.MkdirAll(filepath.Dir(sock), 0o700)
	p := &fakeNoAdmin{}
	mac := &fakeMac{foreign: map[netip.Prefix]string{}}
	splice := &fakeSplice{}
	f.opts.Modules = []Module{&Network{Helper: SocketHelper{Path: sock}, Mac: mac, Tunnel: splice, NoAdmin: p}}
	f.start()
	return f, sock, p, mac, splice
}

func helperAt(t *testing.T, sock, words string) *StandInHelper {
	t.Helper()
	h, err := NewStandInHelper(words)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Listen(sock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func TestWithNoHelperDirectAccessGoesByTheNoAdminPath(t *testing.T) {
	f, _, p, mac, splice := noAdmin(t)
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	block := f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOn && len(b.Routes) > 0 })
	if block.Via != api.ViaVmnet || block.NoHelper != api.DirectNoHelper || block.Interface != "bridge77" || block.Reason != "" ||
		!slices.Equal(block.Routes, routed()) {
		t.Errorf("on: %+v", block)
	}
	if _, _, open, ps := p.state(); !open || !slices.Equal(ps, routed()) {
		t.Errorf("the shim answers for %v (open %v)", ps, open)
	}
	if splice.attached != "bridge77 198.19.3.1 198.19.3.2" || splice.mtu != 16384 {
		t.Errorf("the splice has %q at %d", splice.attached, splice.mtu)
	}
	if w := f.d.hub.Status().Warnings; len(w) != 0 {
		t.Errorf("warnings %q", w)
	}
	mac.set("198.19.64.0/24", "utun4")
	f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectDegraded })
	if _, _, _, ps := p.state(); slices.Contains(ps, "198.19.64.0/18") || len(ps) != len(routed())-1 {
		t.Errorf("withdrawn: the shim answers for %v", ps)
	}
	mac.set("", "")
	f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOn && len(b.Conflicts) == 0 })

	f.client().VMStop(bg, api.StopParams{TimeoutMS: 5000})
	f.client().VMStart(bg, api.StartParams{})
	time.Sleep(100 * time.Millisecond)
	if opens, closes, open, _ := p.state(); opens != 1 || closes != 0 || !open {
		t.Errorf("across a restart: %d opens, %d closes", opens, closes)
	}

	f.client().ConfigSet(bg, map[string]any{api.KeyNetworkDirectAccess: false})
	block = f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOff })
	if _, closes, open, _ := p.state(); closes != 1 || open || splice.detached != 1 || block.Reason != api.DirectSetting || block.Via != "" {
		t.Errorf("off: %+v; %d closes, detached %d", block, closes, splice.detached)
	}
	f.client().ConfigSet(bg, map[string]any{api.KeyNetworkDirectAccess: true})
	f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOn && b.Via == api.ViaVmnet })
	f.d.Close()
	if _, closes, open, _ := p.state(); closes != 2 || open {
		t.Errorf("the daemon's exit left the path open (%d closes)", closes)
	}
}

func TestTheHelperTakesOverFromTheNoAdminPath(t *testing.T) {
	f, sock, p, _, splice := noAdmin(t)
	h := helperAt(t, sock, "")
	f.client().ConfigSet(bg, map[string]any{api.KeyAdminEnabled: false})
	f.client().VMStart(bg, api.StartParams{})
	block := f.network(func(b *api.NetworkStatus) bool { return b.Via == api.ViaVmnet })
	time.Sleep(50 * time.Millisecond)
	if block.NoHelper != api.DirectAdminOff || len(h.Calls()) != 0 {
		t.Errorf("admin off: %+v, calls %v", block, h.Calls())
	}
	f.client().ConfigSet(bg, map[string]any{api.KeyAdminEnabled: true})
	block = f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOn && b.Interface == "utun99" })
	if _, closes, open, _ := p.state(); closes != 1 || open || block.Via != "" || block.NoHelper != "" || splice.attached != "utun99 198.19.3.1 198.19.3.2" {
		t.Errorf("admin on: %+v; %d closes", block, closes)
	}
	f.client().ConfigSet(bg, map[string]any{api.KeyAdminEnabled: false})
	f.network(func(b *api.NetworkStatus) bool { return b.Via == api.ViaVmnet })
	if !h.TunnelClosed() {
		t.Error("the helper's tunnel left open")
	}

	f2, sock2, p2, _, _ := noAdmin(t)
	f2.client().VMStart(bg, api.StartParams{})
	f2.network(func(b *api.NetworkStatus) bool { return b.Via == api.ViaVmnet && b.NoHelper == api.DirectNoHelper })
	helperAt(t, sock2, "")
	f2.client().VMStop(bg, api.StopParams{TimeoutMS: 5000})
	f2.client().VMStart(bg, api.StartParams{})
	f2.network(func(b *api.NetworkStatus) bool { return b.Interface == "utun99" })
	if _, closes, open, _ := p2.state(); closes != 1 || open {
		t.Errorf("installed meanwhile: %d closes, open %v", closes, open)
	}
}

func TestTheNoAdminPathSaysWhyAndRecovers(t *testing.T) {
	f, sock, _, _, _ := noAdmin(t)
	helperAt(t, sock, "version=99")
	f.client().VMStart(bg, api.StartParams{})
	block := f.network(func(b *api.NetworkStatus) bool { return b.Via == api.ViaVmnet })
	if block.NoHelper != api.DirectOutdated || !strings.Contains(strings.Join(f.d.hub.Status().Warnings, "\n"), "admin helper out of date: direct access goes without it") {
		t.Errorf("outdated: %+v %q", block, f.d.hub.Status().Warnings)
	}

	for _, words := range []string{"refuse=tunnel.open", "base=10.198.0.0/16"} {
		fw, sockw, _, _, _ := noAdmin(t)
		helperAt(t, sockw, words)
		fw.client().VMStart(bg, api.StartParams{})
		block := fw.network(func(b *api.NetworkStatus) bool { return b.Via == api.ViaVmnet })
		if block.NoHelper != api.DirectFailed || len(fw.d.hub.Status().Warnings) == 0 {
			t.Errorf("%s: %+v %q", words, block, fw.d.hub.Status().Warnings)
		}
	}

	f2, _, p2, _, _ := noAdmin(t)
	p2.err = fmt.Errorf("%w (198.19.0.0/16 on lo0)", ErrHelperHeld)
	f2.client().VMStart(bg, api.StartParams{})
	block = f2.network(func(b *api.NetworkStatus) bool { return b.Reason != "" })
	w := strings.Join(f2.d.hub.Status().Warnings, "\n")
	if block.Reason != api.DirectHelperHeld || block.DirectAccess != api.DirectOff || block.NoHelper != api.DirectNoHelper ||
		!strings.Contains(w, `sudo fev setup --admin --undo`) {
		t.Errorf("held: %+v %q", block, w)
	}

	f3, _, p3, _, splice := noAdmin(t)
	f3.client().VMStart(bg, api.StartParams{})
	f3.network(func(b *api.NetworkStatus) bool { return b.Via == api.ViaVmnet })
	p3.mu.Lock()
	stopped := p3.stopped
	p3.mu.Unlock()
	stopped()
	block = f3.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOff })
	if block.Reason != api.DirectFailed || !strings.Contains(strings.Join(f3.d.hub.Status().Warnings, "\n"), "vmnet stopped") || splice.detached != 1 {
		t.Errorf("stopped: %+v %q", block, f3.d.hub.Status().Warnings)
	}
	f3.client().VMStop(bg, api.StopParams{TimeoutMS: 5000})
	f3.client().VMStart(bg, api.StartParams{})
	f3.network(func(b *api.NetworkStatus) bool { return b.Via == api.ViaVmnet })
	if w := f3.d.hub.Status().Warnings; len(w) != 0 {
		t.Errorf("warnings left: %q", w)
	}
}

func TestBothOffOpensNothing(t *testing.T) {
	f, _, p, _, _ := noAdmin(t)
	f.client().ConfigSet(bg, map[string]any{api.KeyAdminEnabled: false, api.KeyNetworkDirectAccess: false})
	f.client().VMStart(bg, api.StartParams{})
	block := f.network(func(b *api.NetworkStatus) bool { return b.Reason != "" })
	time.Sleep(100 * time.Millisecond)
	if opens, _, _, _ := p.state(); opens != 0 || block.DirectAccess != api.DirectOff || block.Reason != api.DirectAdminOff {
		t.Errorf("both off: %+v, %d opens", block, opens)
	}
	f.client().ConfigSet(bg, map[string]any{api.KeyNetworkDirectAccess: true})
	f.network(func(b *api.NetworkStatus) bool { return b.Via == api.ViaVmnet })
	f.client().ConfigSet(bg, map[string]any{api.KeyNetworkDirectAccess: false})
	f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOff })
	if _, closes, open, _ := p.state(); closes != 1 || open {
		t.Errorf("direct access off with admin off: %d closes, open %v", closes, open)
	}
}

func TestTheHelpersReservedRouteIsSaidWithTheWayOutThatFits(t *testing.T) {
	held := fmt.Errorf("%w (198.19.0.0/16 on lo0)", ErrHelperHeld)
	for _, c := range []struct {
		name, words string
		adminOff    bool
		want, not   []string
	}{
		{name: "admin off", adminOff: true, want: []string{`fev config set admin.enabled true`, `sudo fev setup --admin --undo`}},
		{name: "no helper", want: []string{`sudo fev setup --admin --undo`}, not: []string{`admin.enabled`}},
		{name: "outdated", words: "version=99", want: []string{`out of date`, `"sudo fev setup --admin"`}, not: []string{`admin.enabled`}},
		{name: "refused", words: "refuse=tunnel.open", want: []string{`did not open the tunnel`, `reserved route`}, not: []string{`admin.enabled`}},
	} {
		f, sock, p, _, _ := noAdmin(t)
		if c.words != "" {
			helperAt(t, sock, c.words)
		}
		p.mu.Lock()
		p.err = held
		p.mu.Unlock()
		if c.adminOff {
			f.client().ConfigSet(bg, map[string]any{api.KeyAdminEnabled: false})
		}
		f.client().VMStart(bg, api.StartParams{})
		block := f.network(func(b *api.NetworkStatus) bool { return b.Reason == api.DirectHelperHeld })
		w := strings.Join(f.d.hub.Status().Warnings, "\n")
		for _, s := range c.want {
			if !strings.Contains(w, s) {
				t.Errorf("%s: the warning lacks %q: %q (%+v)", c.name, s, w, block)
			}
		}
		for _, s := range c.not {
			if strings.Contains(w, s) {
				t.Errorf("%s: the warning says %q: %q", c.name, s, w)
			}
		}
	}
}
