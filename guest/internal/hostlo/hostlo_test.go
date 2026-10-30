package hostlo

import (
	"net/netip"
	"strings"
	"testing"
)

func TestTheLateChainComesAfterTheGuestsNatAndTakesOnlyLabelledFlows(t *testing.T) {
	got := Rules("eth0", netip.MustParseAddr("198.19.1.2"), netip.MustParseAddr("198.19.1.253"), netip.MustParseAddr("198.19.1.254"))
	for _, want := range []string{
		"table ip fev-hostlo\ndelete table ip fev-hostlo\n",
		"type nat hook output priority -99;",
		"ip daddr 127.0.0.1 meta mark & 0x01000000 == 0x01000000 counter dnat to 198.19.1.254\n",
		"ip daddr 127.0.0.1 ip saddr 198.19.1.253 counter dnat to 198.19.1.254\n",
		"type nat hook postrouting priority 99;",
		"ip daddr 198.19.1.254 ip saddr { 127.0.0.0/8, 198.19.1.253 } counter snat to 198.19.1.2\n",
		"type filter hook prerouting priority -300;",
		"iifname \"eth0\" ip daddr 127.0.0.0/8 counter drop\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rules lack %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "dnat to"); n != 2 {
		t.Errorf("%d DNATs, want 2:\n%s", n, got)
	}
}
