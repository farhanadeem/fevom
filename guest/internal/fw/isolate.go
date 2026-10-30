package fw

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

const IsolateTable = "fev_isolate"

type Isolation struct {
	Uplink   string
	Gateway  netip.Addr
	Machines netip.Prefix
	Base     netip.Prefix
	Mac      []netip.Addr
	Members  []IsolatedMember
}

type IsolatedMember struct {
	Veth    string
	Address netip.Addr
}

func (i Isolation) Script() string {
	var b strings.Builder
	for _, fam := range []string{"bridge", "inet"} {
		fmt.Fprintf(&b, "table %s %s\ndelete table %s %s\n", fam, IsolateTable, fam, IsolateTable)
	}
	if len(i.Members) == 0 {
		return b.String()
	}
	members := append([]IsolatedMember{}, i.Members...)
	sort.Slice(members, func(a, c int) bool { return members[a].Veth < members[c].Veth })
	var mac []string
	for _, a := range i.Mac {
		if a = a.Unmap(); a.Is4() {
			mac = append(mac, a.String())
		}
	}
	sort.Strings(mac)
	mac = compactStrings(mac)

	fmt.Fprintf(&b, "table bridge %s {\n", IsolateTable)
	b.WriteString("\tchain isoprerouting {\n\t\ttype filter hook prerouting priority -300; policy accept;\n")
	for n, m := range members {
		fmt.Fprintf(&b, "\t\tiifname %q jump m%d\n", m.Veth, n)
	}
	b.WriteString("\t}\n")
	for n, m := range members {
		fmt.Fprintf(&b, "\tchain m%d {\n", n)
		b.WriteString("\t\tmeta protocol != { ip, arp } drop\n")
		fmt.Fprintf(&b, "\t\tarp saddr ip != { 0.0.0.0, %s } drop\n", m.Address)
		b.WriteString("\t\tip saddr 0.0.0.0 udp sport 68 udp dport 67 accept\n")
		fmt.Fprintf(&b, "\t\tip saddr != %s drop\n", m.Address)
		fmt.Fprintf(&b, "\t\tip daddr %s accept\n", i.Gateway)
		fmt.Fprintf(&b, "\t\tip daddr 255.255.255.255 udp dport 67 accept\n")
		fmt.Fprintf(&b, "\t\tip daddr %s drop\n", i.Machines.Masked())
		b.WriteString("\t\tip daddr { 224.0.0.0/4, 255.255.255.255 } drop\n")
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")

	fmt.Fprintf(&b, "table inet %s {\n", IsolateTable)
	b.WriteString("\tchain isoforward {\n\t\ttype filter hook forward priority -10; policy accept;\n")
	for n, m := range members {
		fmt.Fprintf(&b, "\t\tip saddr %s jump f%d\n", m.Address, n)
	}
	b.WriteString("\t}\n")
	b.WriteString("\tchain isoinput {\n\t\ttype filter hook input priority -10; policy accept;\n")
	for n, m := range members {
		fmt.Fprintf(&b, "\t\tip saddr %s jump i%d\n", m.Address, n)
	}
	b.WriteString("\t}\n")
	for n := range members {
		fmt.Fprintf(&b, "\tchain f%d {\n", n)
		b.WriteString("\t\tct state established,related accept\n")
		fmt.Fprintf(&b, "\t\toifname != %q drop\n", i.Uplink)
		fmt.Fprintf(&b, "\t\tip daddr %s drop\n", i.Base.Masked())
		if len(mac) > 0 {
			fmt.Fprintf(&b, "\t\tip daddr { %s } drop\n", strings.Join(mac, ", "))
		}
		b.WriteString("\t}\n")
		fmt.Fprintf(&b, "\tchain i%d {\n", n)
		b.WriteString("\t\tct state established,related accept\n")
		fmt.Fprintf(&b, "\t\tip daddr %s udp dport { 53, 67 } accept\n", i.Gateway)
		fmt.Fprintf(&b, "\t\tip daddr %s tcp dport 53 accept\n", i.Gateway)
		fmt.Fprintf(&b, "\t\tip daddr %s icmp type echo-request accept\n", i.Gateway)
		b.WriteString("\t\tdrop\n")
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
	return b.String()
}

func compactStrings(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
