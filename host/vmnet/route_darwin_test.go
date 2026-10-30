//go:build darwin

package vmnet

import (
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
)

func TestRouteOfIsRouteGets(t *testing.T) {
	for _, a := range []string{"1.1.1.1", "127.0.0.1", "198.19.64.5"} {
		got, err := RouteOf(netip.MustParseAddr(a))
		out, _ := exec.Command("/sbin/route", "-n", "get", a).CombinedOutput()
		want := ""
		for _, l := range strings.Split(string(out), "\n") {
			if f := strings.Fields(l); len(f) == 2 && f[0] == "interface:" {
				want = f[1]
			}
		}
		reject := strings.Contains(string(out), "REJECT") || strings.Contains(string(out), "BLACKHOLE")
		dst, mask := "", ""
		for _, l := range strings.Split(string(out), "\n") {
			if f := strings.Fields(l); len(f) == 2 && f[0] == "destination:" {
				dst = f[1]
			} else if len(f) == 2 && f[0] == "mask:" {
				mask = f[1]
			}
		}
		if dst == "default" {
			dst, mask = "0.0.0.0", "0.0.0.0"
		}
		if d, err := netip.ParseAddr(dst); err == nil {
			bits := 32
			if m, err := netip.ParseAddr(mask); err == nil {
				bits, _ = net.IPMask(m.AsSlice()).Size()
			}
			if want := netip.PrefixFrom(d, bits); got.Prefix != want {
				t.Errorf("%s: RouteOf's prefix %s, route -n get %s mask %s", a, got.Prefix, dst, mask)
			}
		}
		if want == "" {
			if err == nil && got.Interface != "" {
				t.Errorf("%s: route -n get has none, RouteOf %+v", a, got)
			}
			continue
		}
		if err != nil || got.Interface != want || got.Reject != reject {
			t.Errorf("%s: RouteOf %+v %v, route -n get %s reject %v", a, got, err, want, reject)
		}
		t.Logf("%s: %+v", a, got)
	}
}
