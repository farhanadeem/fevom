package fw

import (
	"net/netip"
	"strings"
	"testing"
)

func isolation() Isolation {
	return Isolation{
		Uplink: "eth0", Gateway: netip.MustParseAddr("198.19.0.1"),
		Machines: netip.MustParsePrefix("198.19.0.0/24"), Base: netip.MustParsePrefix("198.19.0.0/16"),
		Mac: []netip.Addr{netip.MustParseAddr("192.168.1.20"), netip.MustParseAddr("fd5c::1"), netip.MustParseAddr("100.101.102.103"), netip.MustParseAddr("192.168.1.20")},
		Members: []IsolatedMember{
			{Veth: "bm2222222222", Address: netip.MustParseAddr("198.19.0.12")},
			{Veth: "bm1111111111", Address: netip.MustParseAddr("198.19.0.11")},
		},
	}
}

func TestAnIsolatedNetworksRulesAreKeyedOnTheVeth(t *testing.T) {
	s := isolation().Script()
	for _, want := range []string{
		"table bridge fev_isolate\ndelete table bridge fev_isolate\ntable inet fev_isolate\ndelete table inet fev_isolate\n",
		"type filter hook prerouting priority -300; policy accept;\n\t\tiifname \"bm1111111111\" jump m0\n\t\tiifname \"bm2222222222\" jump m1\n",
		"\tchain m0 {\n\t\tmeta protocol != { ip, arp } drop\n\t\tarp saddr ip != { 0.0.0.0, 198.19.0.11 } drop\n" +
			"\t\tip saddr 0.0.0.0 udp sport 68 udp dport 67 accept\n\t\tip saddr != 198.19.0.11 drop\n\t\tip daddr 198.19.0.1 accept\n",
		"\t\tip daddr 198.19.0.0/24 drop\n",
		"type filter hook forward priority -10; policy accept;\n\t\tip saddr 198.19.0.11 jump f0\n",
		"\tchain f0 {\n\t\tct state established,related accept\n\t\toifname != \"eth0\" drop\n\t\tip daddr 198.19.0.0/16 drop\n" +
			"\t\tip daddr { 100.101.102.103, 192.168.1.20 } drop\n\t}\n",
		"\tchain i1 {\n\t\tct state established,related accept\n\t\tip daddr 198.19.0.1 udp dport { 53, 67 } accept\n" +
			"\t\tip daddr 198.19.0.1 tcp dport 53 accept\n\t\tip daddr 198.19.0.1 icmp type echo-request accept\n\t\tdrop\n\t}\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("want\n%s\nin:\n%s", want, s)
		}
	}
	if strings.Contains(s, "fd5c") {
		t.Errorf("an IPv6 address of the Mac's in IPv4 rules:\n%s", s)
	}
	i := isolation()
	i.Members = nil
	if s := i.Script(); s != "table bridge fev_isolate\ndelete table bridge fev_isolate\ntable inet fev_isolate\ndelete table inet fev_isolate\n" {
		t.Errorf("with no member:\n%s", s)
	}
}
