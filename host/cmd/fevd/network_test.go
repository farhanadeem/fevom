package main

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"fev/host/macnet"
	priv "fev/proto/privhelper"
)

func TestTheHeldRoutesAreTheInstalledRecords(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, rec priv.InstallRecord) string {
		b, _ := json.Marshal(rec)
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	want := []macnet.Held{{Prefix: netip.MustParsePrefix("198.19.0.0/16"), Interface: "lo0"}, {Prefix: netip.MustParsePrefix("fd5b:6e2a:91c0::/48"), Interface: "lo0"}}
	rec := priv.InstallRecord{Base: "198.19.0.0/16", Base6: "fd5b:6e2a:91c0::/48"}
	if got := macnet.HeldIn(write("old.json", rec), os.Getuid()); !slices.Equal(got, want) {
		t.Errorf("a record without Held: %v, want %v", got, want)
	}
	rec.Held = priv.HeldFor([]netip.Prefix{netip.MustParsePrefix("10.7.0.0/16")})
	p := write("new.json", rec)
	if got := macnet.HeldIn(p, os.Getuid()); !slices.Equal(got, []macnet.Held{{Prefix: netip.MustParsePrefix("10.7.0.0/16"), Interface: "lo0"}}) {
		t.Errorf("a record with Held: %v", got)
	}
	if got := macnet.HeldIn(p, os.Getuid()+1); got != nil {
		t.Errorf("a record of another's: %v", got)
	}
	link := filepath.Join(dir, "link.json")
	os.Symlink(p, link)
	if got := macnet.HeldIn(link, os.Getuid()); got != nil {
		t.Errorf("a link: %v", got)
	}
	if got := macnet.HeldIn(filepath.Join(dir, "none.json"), os.Getuid()); got != nil {
		t.Errorf("no record: %v", got)
	}
}

func TestTheRecordedIPv6BaseIsThisUsersOnly(t *testing.T) {
	dir := t.TempDir()
	b, _ := json.Marshal(priv.InstallRecord{UID: os.Getuid(), Base: "198.19.0.0/16", Base6: "fd5b:6e2a:91c0::/48"})
	p := filepath.Join(dir, "install.json")
	os.WriteFile(p, b, 0o644)
	if got := recorded6(p, os.Getuid(), os.Getuid()); got != "fd5b:6e2a:91c0::/48" {
		t.Errorf("this user's record: %q", got)
	}
	if got := recorded6(p, os.Getuid(), os.Getuid()+1); got != "" {
		t.Errorf("another user's record: %q", got)
	}
	if got := recorded6(p, os.Getuid()+1, os.Getuid()); got != "" {
		t.Errorf("a record not of its owner's: %q", got)
	}
}

func TestADNSOnlyChangeIsNotTheMacsNetworkChanging(t *testing.T) {
	routes := []macnet.Route{{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "en0"}}
	w := macnet.NewWithReader(nil, func() (macnet.Snapshot, error) {
		return macnet.Snapshot{Routes: slices.Clone(routes)}, nil
	})
	w.Refresh("start")
	m := &macNetwork{w: w}
	calls := 0
	defer m.Subscribe(func() { calls++ })()
	for i := 0; i < 5; i++ {
		w.Notify("State:/Network/Global/DNS")
	}
	if calls != 0 {
		t.Fatalf("a DNS key alone reached the readers %d times", calls)
	}
	w.Notify("State:/Network/Global/Proxies")
	if calls != 1 {
		t.Fatalf("a proxy change reached the readers %d times, want 1", calls)
	}
	routes = append(routes, macnet.Route{Prefix: netip.MustParsePrefix("10.8.0.0/16"), Interface: "utun4"})
	w.Notify("State:/Network/Global/IPv4")
	if calls != 2 {
		t.Fatalf("a route change reached the readers %d times, want 2 in all", calls-1)
	}
}
