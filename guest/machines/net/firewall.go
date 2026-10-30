package machnet

import (
	"fmt"
	"net/netip"
	"strings"

	"fev/guest/internal/fw"
)

const Chain = "FEV-MACHINES"

const (
	userChain     = fw.UserChain
	internalChain = fw.InternalChain
)

func parseSave(out string) fw.Tables { return fw.ParseSave(out) }

func restoreScript(now fw.Tables, bridge string, prefix netip.Prefix, uplink string) string {
	var b strings.Builder
	jump := "-A " + userChain + " -j " + Chain
	fmt.Fprintf(&b, "*filter\n:%s - [0:0]\n", Chain)
	for _, c := range []string{userChain, internalChain} {
		if !now.HasChain("filter", c) {
			fmt.Fprintf(&b, ":%s - [0:0]\n", c)
		}
	}
	fmt.Fprintf(&b, "-A %s -i %s -j %s\n", Chain, bridge, internalChain)
	fmt.Fprintf(&b, "-A %s -i %s -j ACCEPT\n", Chain, bridge)
	fmt.Fprintf(&b, "-A %s -o %s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n", Chain, bridge)
	if !now.Has("filter", jump) {
		b.WriteString(jump + "\n")
	}
	b.WriteString("COMMIT\n")
	fmt.Fprintf(&b, "*nat\n:%s - [0:0]\n", Chain)
	if uplink != "" {
		fmt.Fprintf(&b, "-A %s -s %s -o %s -j MASQUERADE\n", Chain, prefix.Masked(), uplink)
	}
	if !now.Has("nat", "-A POSTROUTING -j "+Chain) {
		fmt.Fprintf(&b, "-A POSTROUTING -j %s\n", Chain)
	}
	b.WriteString("COMMIT\n")
	return b.String()
}
