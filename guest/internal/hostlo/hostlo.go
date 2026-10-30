package hostlo

import (
	"fmt"
	"net/netip"
)

const Mark = 0x01000000

const Table = "fev-hostlo"

func Rules(uplink string, guest, source, host netip.Addr) string {
	return fmt.Sprintf(`table ip %[1]s
delete table ip %[1]s
table ip %[1]s {
	chain in {
		type filter hook prerouting priority -300; policy accept;
		iifname %[6]q ip daddr 127.0.0.0/8 counter drop
	}
	chain out {
		type nat hook output priority -99; policy accept;
		ip daddr 127.0.0.1 meta mark & 0x%08[2]x == 0x%08[2]x counter dnat to %[3]s
		ip daddr 127.0.0.1 ip saddr %[4]s counter dnat to %[3]s
	}
	chain post {
		type nat hook postrouting priority 99; policy accept;
		ip daddr %[3]s ip saddr { 127.0.0.0/8, %[4]s } counter snat to %[5]s
	}
}
`, Table, Mark, host, source, guest, uplink)
}
