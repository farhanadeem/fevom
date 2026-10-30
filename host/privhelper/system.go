package privhelper

import (
	"context"
	"net/netip"
	"time"
)

type System interface {
	Interfaces() ([]Interface, error)
	NAT64Prefixes() ([]netip.Prefix, error)
	Routes() ([]Route, error)
	OpenTunnel(mtu int, mac, peer, mac6 netip.Addr) (fd int, name string, err error)
	AddRoute(ifname string, p netip.Prefix) error
	DeleteRoute(ifname string, p netip.Prefix) error
	AddRejectRoute(p netip.Prefix) error
	DeleteRejectRoute(p netip.Prefix) error
	Unisolate(p netip.Prefix) (bool, error)
	BindPort(proto string, ap netip.AddrPort) (int, error)
	CloseFD(fd int)
	DNSResolvers() ([]Resolver, error)
	LocalHostName() (string, error)
	Probe(ctx context.Context, names []string, kinds []ProbeKind, d time.Duration) (map[string][]Answer, error)
}

type Interface struct {
	Name  string
	Index int
	Addrs []netip.Prefix
}

func (i Interface) Has(a netip.Addr) bool {
	for _, p := range i.Addrs {
		if p.Addr() == a {
			return true
		}
	}
	return false
}

type Route struct {
	Prefix  netip.Prefix
	Index   int
	Flags   int
	Gateway string
}

const (
	RTFUp        = 0x1
	RTFGateway   = 0x2
	RTFHost      = 0x4
	RTFReject    = 0x8
	RTFDynamic   = 0x10
	RTFStatic    = 0x800
	RTFBlackhole = 0x1000
	RTFWasCloned = 0x20000
	RTFIfScope   = 0x1000000
)

type Resolver struct {
	Source  string
	Domains []string
	Search  []string
}

type ProbeKind int

const (
	ProbeSystem ProbeKind = iota
	ProbeZone
	ProbeMulticast
)

func (k ProbeKind) String() string {
	switch k {
	case ProbeSystem:
		return "the Mac's resolvers"
	case ProbeZone:
		return "a zone of the Mac's resolvers"
	case ProbeMulticast:
		return "mDNS"
	}
	return "?"
}

type Answer struct {
	Kind   ProbeKind
	Type   string
	Addr   string
	Ifname string
}
