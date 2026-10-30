package daemon

import (
	"net/netip"
	"path/filepath"
	"testing"

	"fev/host/daemon/config"
	"fev/host/netstack"
	api "fev/proto/daemon"
)

type lanMacNet struct{ fakeMacNet }

func (*lanMacNet) LANs() []netip.Prefix {
	return []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}
}

func TestTheStacksTransparentProxyFollowsItsSettingAndTheMacsNetworks(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.json"), config.Keys(config.Host{CPUs: 4, MemMiB: 8192}), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	g := &GuestNet{Mac: &lanMacNet{}, cfg: cfg}
	var o netstack.Options
	g.Options(&o)
	if o.Proxy == nil || o.Proxy.Transparent == nil || !o.Proxy.Transparent() {
		t.Fatal("the transparent proxy is not on by default")
	}
	if l := o.Proxy.LANs; l == nil || len(l()) != 1 || l()[0].String() != "192.168.1.0/24" {
		t.Fatal("the Mac's networks are not given to the stack")
	}
	if _, err := cfg.Set(map[string]any{api.KeyNetworkProxyTransparent: false}); err != nil {
		t.Fatal(err)
	}
	if o.Proxy.Transparent() {
		t.Fatal("network.proxy.transparent false did not turn it off")
	}
}
