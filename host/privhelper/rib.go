package privhelper

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"net/netip"
)

const (
	rtmVersion    = 5
	rtMsghdrSize  = 92
	rtaDst        = 0x1
	rtaGateway    = 0x2
	rtaNetmask    = 0x4
	rtaMaxBits    = 8
	afInet        = 2
	afLink        = 18
	afInet6       = 30
	sockaddrIn4Of = 4
	sockaddrIn6Of = 8
)

func ParseRIB(b []byte) ([]Route, error) {
	var out []Route
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, fmt.Errorf("a routing message of %d bytes", len(b))
		}
		n := int(binary.NativeEndian.Uint16(b[0:2]))
		if n < rtMsghdrSize || n > len(b) {
			return nil, fmt.Errorf("a routing message claims %d bytes of %d", n, len(b))
		}
		msg := b[:n]
		b = b[n:]
		if msg[2] != rtmVersion {
			return nil, fmt.Errorf("a routing message of version %d", msg[2])
		}
		r, ok, err := parseRoute(msg)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func parseRoute(msg []byte) (Route, bool, error) {
	index := int(binary.NativeEndian.Uint16(msg[4:6]))
	flags := int(int32(binary.NativeEndian.Uint32(msg[8:12])))
	addrs := int(int32(binary.NativeEndian.Uint32(msg[12:16])))
	rest := msg[rtMsghdrSize:]
	var dst, gw, mask []byte
	for i := 0; i < rtaMaxBits; i++ {
		if addrs&(1<<i) == 0 {
			continue
		}
		if len(rest) == 0 {
			return Route{}, false, fmt.Errorf("a routing message ends before its sockaddr %d", i)
		}
		l := int(rest[0])
		adv := 4
		if l > 0 {
			adv = (l + 3) &^ 3
		}
		if l > len(rest) {
			return Route{}, false, fmt.Errorf("a sockaddr of %d bytes in %d", l, len(rest))
		}
		sa := rest[:l]
		switch 1 << i {
		case rtaDst:
			dst = sa
		case rtaGateway:
			gw = sa
		case rtaNetmask:
			mask = sa
		}
		if adv > len(rest) {
			adv = len(rest)
		}
		rest = rest[adv:]
	}
	if len(dst) < 2 {
		return Route{}, false, nil
	}
	var addr netip.Addr
	var off, size int
	switch dst[1] {
	case afInet:
		if len(dst) < sockaddrIn4Of+4 {
			return Route{}, false, fmt.Errorf("an IPv4 destination of %d bytes", len(dst))
		}
		addr = netip.AddrFrom4([4]byte(dst[sockaddrIn4Of : sockaddrIn4Of+4]))
		off, size = sockaddrIn4Of, 4
	case afInet6:
		if len(dst) < sockaddrIn6Of+16 {
			return Route{}, false, fmt.Errorf("an IPv6 destination of %d bytes", len(dst))
		}
		a := [16]byte(dst[sockaddrIn6Of : sockaddrIn6Of+16])
		if a[0] == 0xfe && a[1]&0xc0 == 0x80 {
			a[2], a[3] = 0, 0
		}
		addr = netip.AddrFrom16(a)
		off, size = sockaddrIn6Of, 16
	default:
		return Route{}, false, nil
	}
	length := size * 8
	if mask != nil && flags&RTFHost == 0 {
		length = 0
		for i := off; i < off+size && i < len(mask); i++ {
			length += bits.OnesCount8(mask[i])
		}
	}
	p, err := addr.Prefix(length)
	if err != nil {
		return Route{}, false, err
	}
	return Route{Prefix: p, Index: index, Flags: flags, Gateway: gateway(gw)}, true, nil
}

func gateway(sa []byte) string {
	if len(sa) < 2 {
		return ""
	}
	switch sa[1] {
	case afLink:
		if len(sa) >= 4 {
			return fmt.Sprintf("link#%d", binary.NativeEndian.Uint16(sa[2:4]))
		}
	case afInet:
		if len(sa) >= sockaddrIn4Of+4 {
			return netip.AddrFrom4([4]byte(sa[sockaddrIn4Of : sockaddrIn4Of+4])).String()
		}
	case afInet6:
		if len(sa) >= sockaddrIn6Of+16 {
			a := [16]byte(sa[sockaddrIn6Of : sockaddrIn6Of+16])
			if a[0] == 0xfe && a[1]&0xc0 == 0x80 {
				a[2], a[3] = 0, 0
			}
			return netip.AddrFrom16(a).String()
		}
	}
	return ""
}
