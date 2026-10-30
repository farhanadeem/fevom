package daemon

import (
	"context"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	api "fev/proto/daemon"
	"fev/proto/netplan"
	priv "fev/proto/privhelper"
)

type tunnelSpy struct {
	HelperClient
	mu    sync.Mutex
	opens []priv.TunnelParams
}

func (s *tunnelSpy) Call(ctx context.Context, method string, params, result any) (*os.File, error) {
	if method == priv.MethodTunnelOpen {
		s.mu.Lock()
		switch p := params.(type) {
		case priv.TunnelParams:
			s.opens = append(s.opens, p)
		case *priv.TunnelParams:
			s.opens = append(s.opens, *p)
		default:
			s.opens = append(s.opens, priv.TunnelParams{IPv6: true, MTU: -1})
		}
		s.mu.Unlock()
	}
	return s.HelperClient.Call(ctx, method, params, result)
}

func TestTheTunnelCarriesIPv6WhenTheHelperRecordedThePlans48(t *testing.T) {
	p6, _ := netplan.New6(StandInBase6)
	want6 := append(routed(), prefixStrings6(p6.Routed6())...)
	for _, c := range []struct {
		name, words string
		ipv6        bool
	}{
		{"the plan's /48", "", true},
		{"another /48", "base6=fd00:1:2::/48", false},
		{"none recorded", "base6=none", false},
		{"the /128 refused", "refuse6", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, n, h, _, splice := direct(t, c.words)
			spy := &tunnelSpy{HelperClient: n.Helper}
			n.Helper = spy
			if _, err := f.client().ConfigSet(bg, map[string]any{api.KeyNetworkSubnet6: StandInBase6.String()}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
				t.Fatal(err)
			}
			block := f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOn && len(b.Routes) > 0 })
			spy.mu.Lock()
			opens := slices.Clone(spy.opens)
			spy.mu.Unlock()
			switch {
			case c.words == "refuse6":
				if len(opens) != 2 || !opens[0].IPv6 || opens[1].IPv6 {
					t.Fatalf("tunnel.open asked %+v", opens)
				}
			case len(opens) != 1 || opens[0].IPv6 != c.ipv6:
				t.Fatalf("tunnel.open asked %+v", opens)
			}
			splice.mu.Lock()
			ends := splice.ends
			splice.mu.Unlock()
			if c.ipv6 {
				if ends.Mac6 != p6.TunnelMac6() || ends.Peer6 != p6.TunnelPeer6() || ends.Mac46 != netplan.Default().TunnelMac46() {
					t.Errorf("the splice was given %+v", ends)
				}
				if !slices.Equal(block.Routes, want6) || !slices.Equal(h.Routes(), want6) || block.Address6 != p6.TunnelMac6().String() {
					t.Errorf("routes %v (the helper %v), address6 %q", block.Routes, h.Routes(), block.Address6)
				}
				return
			}
			if ends.Mac6.IsValid() || ends.Mac46.IsValid() || !slices.Equal(block.Routes, routed()) || block.Address6 != "" {
				t.Errorf("IPv4 alone: %+v, routes %v, address6 %q", ends, block.Routes, block.Address6)
			}
		})
	}
}

func prefixStrings6(ps []netip.Prefix) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}

func TestTheNAT64TwinsFollowTheMacsNAT64Prefix(t *testing.T) {
	pref := "64:ff9b::/96"
	p6, _ := netplan.New6(StandInBase6)
	base := append(routed(), prefixStrings6(p6.Routed6())...)
	twins := func(withdrawn string) []string {
		var out []string
		for _, p := range netplan.Default().Routed() {
			if p.String() != withdrawn {
				out = append(out, netplan.NAT64Prefix(netip.MustParsePrefix(pref), p).String())
			}
		}
		return out
	}
	start := func(t *testing.T, words string) (*fixture, *StandInHelper, *fakeMac, *fakeSplice) {
		f, _, h, mac, splice := direct(t, words)
		if _, err := f.client().ConfigSet(bg, map[string]any{api.KeyNetworkSubnet6: StandInBase6.String()}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
			t.Fatal(err)
		}
		f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOn && len(b.Routes) == len(base) })
		return f, h, mac, splice
	}
	nat := func(s *fakeSplice) []netip.Prefix {
		s.mu.Lock()
		defer s.mu.Unlock()
		return slices.Clone(s.nat64)
	}
	t.Run("followed", func(t *testing.T) {
		f, h, mac, splice := start(t, "nat64="+pref)
		mac.setNAT64(pref)
		want := append(slices.Clone(base), twins("")...)
		block := f.network(func(b *api.NetworkStatus) bool { return len(b.Routes) == len(want) })
		if !slices.Equal(block.Routes, want) || !slices.Equal(h.Routes(), want) || !slices.Equal(block.NAT64, []string{pref}) ||
			!slices.Equal(nat(splice), []netip.Prefix{netip.MustParsePrefix(pref)}) {
			t.Fatalf("routes %v, nat64 %v, the splice's %v", block.Routes, block.NAT64, nat(splice))
		}
		mac.set("198.19.64.0/24", "utun4")
		var cut []string
		for _, r := range base {
			if r != "198.19.64.0/18" {
				cut = append(cut, r)
			}
		}
		want = append(cut, twins("198.19.64.0/18")...)
		block = f.network(func(b *api.NetworkStatus) bool {
			return b.DirectAccess == api.DirectDegraded && len(b.Routes) == len(want)
		})
		if !slices.Equal(h.Routes(), want) {
			t.Fatalf("degraded: the helper routes %v", h.Routes())
		}
		mac.set("", "")
		mac.setNAT64()
		block = f.network(func(b *api.NetworkStatus) bool { return len(b.Routes) == len(base) && len(b.NAT64) == 0 })
		if !slices.Equal(h.Routes(), base) || len(nat(splice)) != 0 {
			t.Fatalf("the prefix gone: the helper routes %v, the splice translates %v", h.Routes(), nat(splice))
		}
	})
	t.Run("the helper reads none", func(t *testing.T) {
		f, h, mac, splice := start(t, "")
		calls := len(h.Calls())
		mac.setNAT64(pref)
		deadline := time.Now().Add(5 * time.Second)
		for len(h.Calls()) < calls+2 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		block := f.network(func(b *api.NetworkStatus) bool { return true })
		if got := h.Calls()[calls:]; strings.Join(got, " ") != "routes.set routes.set" {
			t.Errorf("calls %v", got)
		}
		if !slices.Equal(h.Routes(), base) || len(block.NAT64) != 0 || len(nat(splice)) != 0 || block.DirectAccess != api.DirectOn {
			t.Fatalf("the helper routes %v, nat64 %v, the splice's %v, %s", h.Routes(), block.NAT64, nat(splice), block.DirectAccess)
		}
		if strings.Contains(strings.Join(f.d.hub.Status().Warnings, "\n"), "did not set the tunnel's routes") {
			t.Errorf("warned: %q", f.d.hub.Status().Warnings)
		}
	})
	t.Run("a helper built before them", func(t *testing.T) {
		_, h, mac, _ := start(t, "nat64="+pref+" prenat64")
		calls := len(h.Calls())
		mac.setNAT64(pref)
		time.Sleep(100 * time.Millisecond)
		if got := h.Calls()[calls:]; len(got) != 0 || !slices.Equal(h.Routes(), base) {
			t.Fatalf("calls %v, the helper routes %v", got, h.Routes())
		}
	})
}
