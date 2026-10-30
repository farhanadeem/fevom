package netplan

import (
	"fmt"
	"net/netip"
)

var nat64Lengths = map[int]bool{32: true, 40: true, 48: true, 56: true, 64: true, 96: true}

func CheckNAT64(p netip.Prefix) error {
	if !p.IsValid() || !p.Addr().Is6() || p.Addr().Is4In6() {
		return fmt.Errorf("%s is not an IPv6 prefix", p)
	}
	if !nat64Lengths[p.Bits()] {
		return fmt.Errorf("%s: a NAT64 prefix is a /32, /40, /48, /56, /64 or /96", p)
	}
	if p.Masked() != p {
		return fmt.Errorf("%s has bits set past its length", p)
	}
	if p.Addr().As16()[8] != 0 {
		return fmt.Errorf("%s: bits 64 to 71 of a NAT64 prefix are zero", p)
	}
	return nil
}

func nat64Bit(l, i int) int {
	b := l + i
	if l <= 64 && b >= 64 {
		b += 8
	}
	return b
}

func NAT64Address(p netip.Prefix, v4 netip.Addr) netip.Addr {
	out := p.Masked().Addr().As16()
	a := v4.As4()
	for i := 0; i < 32; i++ {
		if a[i/8]&(0x80>>(i%8)) != 0 {
			b := nat64Bit(p.Bits(), i)
			out[b/8] |= 0x80 >> (b % 8)
		}
	}
	return netip.AddrFrom16(out)
}

func NAT64V4(p netip.Prefix, a netip.Addr) (netip.Addr, bool) {
	if !a.Is6() || a.Is4In6() || !p.Contains(a) {
		return netip.Addr{}, false
	}
	b := a.As16()
	var v4 [4]byte
	for i := 0; i < 32; i++ {
		bit := nat64Bit(p.Bits(), i)
		if b[bit/8]&(0x80>>(bit%8)) != 0 {
			v4[i/8] |= 0x80 >> (i % 8)
		}
	}
	out := netip.AddrFrom4(v4)
	if NAT64Address(p, out) != a {
		return netip.Addr{}, false
	}
	return out, true
}

func NAT64Prefix(p, v4 netip.Prefix) netip.Prefix {
	v4 = v4.Masked()
	bits := p.Bits()
	if v4.Bits() > 0 {
		bits = nat64Bit(p.Bits(), v4.Bits()-1) + 1
	}
	return netip.PrefixFrom(NAT64Address(p, v4.Addr()), bits)
}
