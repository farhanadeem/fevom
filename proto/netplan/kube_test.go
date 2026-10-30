package netplan

import (
	"net/netip"
	"testing"
)

func TestKubeRangesAreTheBasesOwnAndClear(t *testing.T) {
	reference := []netip.Prefix{
		netip.MustParsePrefix("198.19.192.0/19"),
		netip.MustParsePrefix("198.19.249.0/24"),
	}
	for _, base := range Candidates {
		p, err := New(base)
		if err != nil {
			t.Fatal(err)
		}
		all := p.Prefixes()
		for _, k := range p.Kube() {
			if !p.Base().Contains(k.Addr()) || k.Bits() < BaseBits {
				t.Errorf("%s: %s is outside the base", base, k)
			}
			for _, q := range all {
				if q != k && q.Overlaps(k) {
					t.Errorf("%s: %s overlaps the plan's %s", base, k, q)
				}
			}
			for _, r := range reference {
				if k.Overlaps(r) {
					t.Errorf("%s: %s overlaps the documented %s", base, k, r)
				}
			}
		}
		if !p.KubeServices().Contains(p.KubeDNS()) {
			t.Errorf("%s: the DNS %s is not a service address", base, p.KubeDNS())
		}
		if p.KubePods().Bits() > KubePodsPerNode {
			t.Errorf("%s: the pods' %s is narrower than a node's /%d", base, p.KubePods(), KubePodsPerNode)
		}
	}
	d := Default()
	for want, got := range map[string]netip.Prefix{"198.19.8.0/22": d.KubeLoadBalancers(), "198.19.16.0/20": d.KubePods(), "198.19.32.0/20": d.KubeServices()} {
		if got.String() != want {
			t.Errorf("default plan: %s, want %s", got, want)
		}
	}
	if d.KubeDNS().String() != "198.19.32.10" {
		t.Errorf("cluster DNS %s", d.KubeDNS())
	}
}

func TestChooseSkipsABaseWhoseClusterRangesMeetTheMac(t *testing.T) {
	taken := []netip.Prefix{netip.MustParsePrefix("198.19.20.0/24")}
	p, ok := Choose(Candidates, taken)
	if !ok || p.Base() != Candidates[1] {
		t.Fatalf("chose %v %v, want %s", p.Base(), ok, Candidates[1])
	}
	if p.KubePods().String() != "10.198.16.0/20" || p.KubeServices().String() != "10.198.32.0/20" || p.KubeDNS().String() != "10.198.32.10" {
		t.Fatalf("moved ranges: %s %s %s", p.KubePods(), p.KubeServices(), p.KubeDNS())
	}
	ref := []netip.Prefix{netip.MustParsePrefix("198.19.192.0/19"), netip.MustParsePrefix("198.19.249.0/24")}
	if p, ok := Choose(Candidates, ref); !ok || p.Base() != DefaultBase {
		t.Fatalf("with those routes chose %v %v", p.Base(), ok)
	}
}
