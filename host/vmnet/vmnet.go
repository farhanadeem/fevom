package vmnet

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

type Config struct {
	Base     netip.Prefix
	Mac      netip.Addr
	Peer     netip.Addr
	Prefixes []netip.Prefix
	MTU      int
	TSO      bool
}

const (
	DefaultMTU = 16384
	MinMTU     = 1500
	MaxMTU     = 16384
)

func (c Config) mtu() int {
	if c.MTU == 0 {
		return DefaultMTU
	}
	return c.MTU
}

func (c Config) check() error {
	if !c.Base.Addr().Is4() || c.Base.Bits() < 8 || c.Base.Bits() > 30 {
		return fmt.Errorf("vmnet: the base %s is not an IPv4 prefix to connect", c.Base)
	}
	if !c.Mac.Is4() || !c.Base.Contains(c.Mac) || !c.Peer.Is4() || !c.Base.Contains(c.Peer) || c.Mac == c.Peer {
		return fmt.Errorf("vmnet: the Mac's address %s and the peer %s must be two addresses of %s", c.Mac, c.Peer, c.Base)
	}
	if m := c.mtu(); m < MinMTU || m > MaxMTU {
		return fmt.Errorf("vmnet: an MTU of %d is outside %d to %d", m, MinMTU, MaxMTU)
	}
	if !private(c.Base) {
		return fmt.Errorf("%w; fev's is %s", ErrNotPrivate, c.Base)
	}
	return nil
}

var ErrNotPrivate = errors.New("vmnet: the no-admin path's interface takes only a private base (10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16)")

func private(p netip.Prefix) bool {
	for _, s := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		q := netip.MustParsePrefix(s)
		if p.Addr().Is4() && q.Bits() <= p.Bits() && q.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

var ErrUnsupported = errors.New("vmnet: the no-admin path needs macOS and cgo")

type Stats struct {
	FramesIn, FramesOut, Wakeups            uint64
	ToTunnel, FromTunnel                    uint64
	ARPAnswered, ARPIgnored                 uint64
	MDNSUp, MDNSIgnored, MDNSDown           uint64
	DroppedIPv6, DroppedOther, DroppedNoMac uint64
	WriteFailed, Malformed                  uint64
}

func (s Stats) String() string {
	return fmt.Sprintf("frames-in=%d frames-out=%d wakeups=%d to-tunnel=%d from-tunnel=%d arp-answered=%d arp-ignored=%d mdns-up=%d mdns-ignored=%d mdns-down=%d dropped-ipv6=%d dropped-other=%d dropped-no-mac=%d write-failed=%d malformed=%d",
		s.FramesIn, s.FramesOut, s.Wakeups, s.ToTunnel, s.FromTunnel, s.ARPAnswered, s.ARPIgnored, s.MDNSUp, s.MDNSIgnored, s.MDNSDown,
		s.DroppedIPv6, s.DroppedOther, s.DroppedNoMac, s.WriteFailed, s.Malformed)
}

func prefixBytes(ps []netip.Prefix) (addrs, bits []byte) {
	for _, p := range ps {
		if !p.Addr().Is4() || len(bits) == 64 {
			continue
		}
		a := p.Masked().Addr().As4()
		addrs = append(addrs, a[:]...)
		bits = append(bits, byte(p.Bits()))
	}
	return addrs, bits
}

func zoneBytes(zones []string) []byte {
	var b []byte
	for _, z := range zones {
		z = strings.ToLower(strings.TrimSuffix(z, "."))
		if z == "" || strings.IndexByte(z, 0) >= 0 {
			continue
		}
		b = append(b, z...)
		b = append(b, 0)
	}
	return append(b, 0)
}
