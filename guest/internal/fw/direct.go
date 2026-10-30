package fw

import (
	"fmt"
	"net/netip"
	"strings"
)

const (
	DirectChain = "FEV-DIRECT"
	TunnelChain = "FEV-TUNNEL"
	NamesChain  = "FEV-NAMES"
	WebChain    = "FEV-WEB"
)

type Direct struct {
	Uplink                 string
	TunnelMac, TunnelMac46 netip.Addr
	Names                  netip.Prefix
	Index                  netip.Addr
	WebHTTP, WebHTTPS      uint16
	Web                    bool
	HTTPS                  bool
	Targets                []NameTarget
	Machines               netip.Prefix
	MachineTargets         []netip.Addr
	Unredirected           map[netip.AddrPort]bool

	TunnelMac6      netip.Addr
	Names6          netip.Prefix
	Index6          netip.Addr
	Targets6        []NameTarget
	Machines6       netip.Prefix
	MachineTargets6 []MachineTarget6
}

type NameTarget struct {
	Address, Target netip.Addr
}

type MachineTarget6 struct {
	Address6, Address netip.Addr
}

func host(a netip.Addr) string { return netip.PrefixFrom(a, a.BitLen()).String() }

type family struct {
	uplink      string
	sources     []netip.Addr
	icmp        string
	unreachable string
	names       netip.Prefix
	index       netip.Addr
	targets     []NameTarget
	machines    netip.Prefix
	machineWeb  [][2]netip.Addr
}

func (d Direct) Script(now Tables) string {
	f := family{uplink: d.Uplink, sources: []netip.Addr{d.TunnelMac}, icmp: "icmp", unreachable: "icmp-port-unreachable",
		names: d.Names, index: d.Index, targets: d.Targets, machines: d.Machines}
	if d.TunnelMac46.IsValid() {
		f.sources = append(f.sources, d.TunnelMac46)
	}
	for _, m := range d.MachineTargets {
		f.machineWeb = append(f.machineWeb, [2]netip.Addr{m, m})
	}
	return d.script(now, f)
}

func (d Direct) Script6(now Tables) string {
	if !d.TunnelMac6.IsValid() {
		return ""
	}
	f := family{uplink: d.Uplink, sources: []netip.Addr{d.TunnelMac6}, icmp: "ipv6-icmp", unreachable: "icmp6-port-unreachable",
		names: d.Names6, index: d.Index6, targets: d.Targets6, machines: d.Machines6}
	for _, m := range d.MachineTargets6 {
		f.machineWeb = append(f.machineWeb, [2]netip.Addr{m.Address6, m.Address})
	}
	return d.script(now, f)
}

func (d Direct) script(now Tables, f family) string {
	var b strings.Builder
	up := f.uplink

	b.WriteString("*filter\n")
	fmt.Fprintf(&b, ":%s - [0:0]\n:%s - [0:0]\n", DirectChain, TunnelChain)
	declare(&b, now, "filter", UserChain)
	for _, src := range f.sources {
		fmt.Fprintf(&b, "-A %s -s %s -i %s -j ACCEPT\n", DirectChain, host(src), up)
		fmt.Fprintf(&b, "-A %s -d %s -o %s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n", DirectChain, host(src), up)
	}
	if jump := "-A " + UserChain + " -j " + DirectChain; !now.Has("filter", jump) {
		b.WriteString(jump + "\n")
	}
	fmt.Fprintf(&b, "-A %s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n", TunnelChain)
	fmt.Fprintf(&b, "-A %s -p %s -j ACCEPT\n", TunnelChain, f.icmp)
	fmt.Fprintf(&b, "-A %s -p tcp -m multiport --dports %d,%d -j ACCEPT\n", TunnelChain, d.WebHTTP, d.WebHTTPS)
	fmt.Fprintf(&b, "-A %s -p tcp -j REJECT --reject-with tcp-reset\n", TunnelChain)
	fmt.Fprintf(&b, "-A %s -j REJECT --reject-with %s\n", TunnelChain, f.unreachable)
	for _, src := range f.sources {
		if !now.Has("filter", fmt.Sprintf("-A INPUT -s %s -i %s -j %s", host(src), up, TunnelChain)) {
			fmt.Fprintf(&b, "-I INPUT 1 -s %s -i %s -j %s\n", host(src), up, TunnelChain)
		}
	}
	b.WriteString("COMMIT\n")

	b.WriteString("*nat\n")
	fmt.Fprintf(&b, ":%s - [0:0]\n:%s - [0:0]\n:%s - [0:0]\n", DirectChain, NamesChain, WebChain)
	for _, src := range f.sources {
		for _, br := range now.InternalBridges() {
			fmt.Fprintf(&b, "-A %s -s %s -o %s -j MASQUERADE\n", DirectChain, host(src), br)
		}
	}
	for _, src := range f.sources {
		if jump := fmt.Sprintf("-A POSTROUTING -s %s -j %s", host(src), DirectChain); !now.Has("nat", jump) {
			b.WriteString(jump + "\n")
		}
	}
	web := func(addr netip.Addr) {
		if !d.Web {
			return
		}
		fmt.Fprintf(&b, "-A %s -d %s -p tcp -m tcp --dport 80 -j REDIRECT --to-ports %d\n", NamesChain, host(addr), d.WebHTTP)
		if d.HTTPS {
			fmt.Fprintf(&b, "-A %s -d %s -p tcp -m tcp --dport 443 -j REDIRECT --to-ports %d\n", NamesChain, host(addr), d.WebHTTPS)
		}
	}
	if f.index.IsValid() {
		web(f.index)
	}
	for _, t := range f.targets {
		web(t.Address)
		if t.Target.IsValid() {
			fmt.Fprintf(&b, "-A %s -d %s -j DNAT --to-destination %s\n", NamesChain, host(t.Address), t.Target)
		}
	}
	if f.machines.IsValid() {
		for _, m := range f.machineWeb {
			for _, port := range []uint16{80, 443} {
				to := d.WebHTTP
				if port == 443 {
					to = d.WebHTTPS
				}
				if !d.Web || (port == 443 && !d.HTTPS) || d.Unredirected[netip.AddrPortFrom(m[1], port)] {
					continue
				}
				fmt.Fprintf(&b, "-A %s -d %s -p tcp -m tcp --dport %d -j REDIRECT --to-ports %d\n", WebChain, host(m[0]), port, to)
			}
		}
		if jump := fmt.Sprintf("-A PREROUTING -d %s -j %s", f.machines.Masked(), WebChain); !now.Has("nat", jump) {
			b.WriteString(jump + "\n")
		}
	}
	if f.names.IsValid() {
		for _, chain := range []string{"PREROUTING", "OUTPUT"} {
			jump := fmt.Sprintf("-A %s -d %s -j %s", chain, f.names.Masked(), NamesChain)
			if now.First("nat", chain) == jump {
				continue
			}
			if now.Has("nat", jump) {
				b.WriteString("-D" + jump[2:] + "\n")
			}
			fmt.Fprintf(&b, "-I %s 1 -d %s -j %s\n", chain, f.names.Masked(), NamesChain)
		}
	}
	b.WriteString("COMMIT\n")
	return b.String()
}
