package daemon

import (
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	api "fev/proto/daemon"
	"fev/proto/netplan"
)

func planned(t *testing.T, taken []netip.Prefix, fresh *atomic.Bool) (*fixture, *Plan) {
	t.Helper()
	f := newFixture(t)
	p := &Plan{Taken: func() []netip.Prefix { return taken }, Fresh: fresh.Load}
	f.opts.Modules = []Module{p}
	f.start()
	return f, p
}

func TestTheFirstStartChoosesABaseAwayFromTheMacsNetworks(t *testing.T) {
	fresh := &atomic.Bool{}
	fresh.Store(true)
	taken := []netip.Prefix{netip.MustParsePrefix("198.19.64.0/20"), netip.MustParsePrefix("10.198.7.0/24"), netip.MustParsePrefix("0.0.0.0/0")}
	f, p := planned(t, taken, fresh)
	st, err := f.client().VMStart(bg, api.StartParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.PendingRestart) != 0 {
		t.Errorf("the plan the daemon chose for this boot waits for a restart: %v", st.PendingRestart)
	}
	values, _ := f.client().ConfigGet(bg, api.KeyNetworkSubnet, api.KeyNetworkSubnet6)
	if v := values[api.KeyNetworkSubnet]; v.Value != "10.234.0.0/16" || v.IsDefault || v.Pending {
		t.Errorf("network.subnet: %+v", v)
	}
	six, err := netplan.Parse6(values[api.KeyNetworkSubnet6].Value.(string))
	if err != nil {
		t.Errorf("network.subnet6: %v", err)
	}
	words := strings.Join(p.BootWords(), " ")
	if words != "fev.machnet=10.234.0.0/16 fev.net6="+six.Prefix().String() {
		t.Errorf("the command line's words: %q", words)
	}
	fresh.Store(false)
	taken = append(taken, netip.MustParsePrefix("10.234.0.0/16"))
	f.client().VMStop(bg, api.StopParams{TimeoutMS: 5000})
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	again, _ := f.client().ConfigGet(bg, api.KeyNetworkSubnet, api.KeyNetworkSubnet6)
	if again[api.KeyNetworkSubnet].Value != "10.234.0.0/16" || again[api.KeyNetworkSubnet6].Value != values[api.KeyNetworkSubnet6].Value {
		t.Errorf("a later boot changed the plan: %+v", again)
	}
	if st := f.d.hub.Status(); strings.Contains(strings.Join(st.Warnings, "\n"), "candidate") {
		t.Errorf("warnings: %q", st.Warnings)
	}
}

func TestTheNoAdminPathsFirstStartTakesAPrivateBase(t *testing.T) {
	for _, c := range []struct {
		name     string
		recorded string
		admin    bool
		want     string
	}{
		{"no helper", "", true, "10.198.0.0/16"},
		{"the helper, admin features off", "198.19.0.0/16", false, "10.198.0.0/16"},
		{"the helper in use", "198.19.0.0/16", true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			fresh := &atomic.Bool{}
			fresh.Store(true)
			f := newFixture(t)
			rec := c.recorded
			p := &Plan{Taken: func() []netip.Prefix { return nil }, Fresh: fresh.Load, NoAdminPath: true, Recorded4: func() string { return rec }}
			f.opts.Modules = []Module{p}
			f.start()
			if !c.admin {
				if _, err := f.client().ConfigSet(bg, map[string]any{api.KeyAdminEnabled: false}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
				t.Fatal(err)
			}
			values, _ := f.client().ConfigGet(bg, api.KeyNetworkSubnet)
			v := values[api.KeyNetworkSubnet]
			if c.want == "" {
				if !v.IsDefault {
					t.Errorf("network.subnet written with the helper in use: %+v", v)
				}
				return
			}
			if v.Value != c.want || v.IsDefault {
				t.Errorf("network.subnet: %+v, want %s", v, c.want)
			}
		})
	}
}

func TestTheDefaultStandsAndTheUsersBaseIsKept(t *testing.T) {
	fresh := &atomic.Bool{}
	fresh.Store(true)
	f, p := planned(t, []netip.Prefix{netip.MustParsePrefix("172.16.30.0/24")}, fresh)
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.client().ConfigGet(bg, api.KeyNetworkSubnet); !v[api.KeyNetworkSubnet].IsDefault {
		t.Errorf("network.subnet: %+v", v)
	}
	if !strings.HasPrefix(strings.Join(p.BootWords(), " "), "fev.machnet=198.19.0.0/16 fev.net6=fd") {
		t.Errorf("words %q", p.BootWords())
	}

	f2, _ := planned(t, []netip.Prefix{netip.MustParsePrefix("198.19.0.0/16")}, fresh)
	if _, err := f2.client().ConfigSet(bg, map[string]any{api.KeyNetworkSubnet: "192.168.240.0/16"}); err == nil {
		t.Fatal("a /16 with host bits was taken")
	}
	if _, err := f2.client().ConfigSet(bg, map[string]any{api.KeyNetworkSubnet: "172.20.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f2.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	if v, _ := f2.client().ConfigGet(bg, api.KeyNetworkSubnet); v[api.KeyNetworkSubnet].Value != "172.20.0.0/16" {
		t.Errorf("the user's base: %+v", v)
	}

	f3, _ := planned(t, netplan.Candidates, fresh)
	if _, err := f3.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	st, _ := f3.client().Status(bg)
	if v, _ := f3.client().ConfigGet(bg, api.KeyNetworkSubnet); !v[api.KeyNetworkSubnet].IsDefault ||
		!strings.Contains(strings.Join(st.Warnings, "\n"), "every candidate base") {
		t.Errorf("every candidate taken: %+v, warnings %q", v, st.Warnings)
	}
}

func TestAMachinesAddressFollowsThePlan(t *testing.T) {
	f := newFixture(t).start()
	m := &Machines{cfg: f.d.cfg}
	if got := m.planned("198.19.0.7"); got != "198.19.0.7" {
		t.Errorf("in the default plan: %s", got)
	}
	if _, err := f.d.cfg.Set(map[string]any{api.KeyNetworkSubnet: "10.198.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	if got := m.planned("198.19.0.7"); got != "10.198.0.7" {
		t.Errorf("in 10.198.0.0/16: %s", got)
	}
	if got := m.machinesPrefix(); got != netip.MustParsePrefix("10.198.0.0/24") {
		t.Errorf("prefix %s", got)
	}
}

func TestTheFirstBootTakesTheHelpersIPv6Base(t *testing.T) {
	fresh := &atomic.Bool{}
	fresh.Store(true)
	f := newFixture(t)
	recorded := "fd12:3456:789a::/48"
	p := &Plan{Fresh: fresh.Load, Recorded6: func() string { return recorded }}
	f.opts.Modules = []Module{p}
	f.start()
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	values, _ := f.client().ConfigGet(bg, api.KeyNetworkSubnet6)
	if v := values[api.KeyNetworkSubnet6].Value; v != recorded {
		t.Errorf("network.subnet6 %v, want the helper's %s", v, recorded)
	}
	recorded = "fdaa:bbbb:cccc::/48"
	f.client().VMStop(bg, api.StopParams{TimeoutMS: 5000})
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	values, _ = f.client().ConfigGet(bg, api.KeyNetworkSubnet6)
	if v := values[api.KeyNetworkSubnet6].Value; v != "fd12:3456:789a::/48" {
		t.Errorf("a set key moved to %v", v)
	}
}
