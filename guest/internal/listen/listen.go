package listen

import "net/netip"

const (
	TCP = "tcp"
	UDP = "udp"
)

type Listener struct {
	Proto  string
	Addr   netip.Addr
	Port   uint16
	V6     bool
	NetNS  uint64
	Cookie uint64
}

func (l Listener) Wildcard() bool { return l.Addr.IsUnspecified() }

type Event struct {
	Listener
	Added bool
}
