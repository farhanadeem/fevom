package machnet

import (
	"encoding/binary"
	"net"
	"net/netip"
	"sort"
	"time"
)

const routerLifetime = 0xffff

const RefreshAfter = time.Hour

func solicitationMAC(b []byte) net.HardwareAddr {
	for o := b[8:]; len(o) >= 8; {
		l := int(o[1]) * 8
		if l == 0 || l > len(o) {
			return nil
		}
		if o[0] == 1 && l == 8 {
			return net.HardwareAddr(append([]byte(nil), o[2:8]...))
		}
		o = o[l:]
	}
	return nil
}

const (
	icmp6RouterSolicitation  = 133
	icmp6RouterAdvertisement = 134
)

func Gateway6(prefix6 netip.Prefix) netip.Addr { return prefix6.Masked().Addr().Next() }

func routerAdvertisement(mac net.HardwareAddr, prefix6 netip.Prefix) []byte {
	b := make([]byte, 16, 16+8+32+8)
	b[0] = icmp6RouterAdvertisement
	b[4] = 64
	binary.BigEndian.PutUint16(b[6:8], routerLifetime)
	b = append(b, 1, 1)
	b = append(b, mac[:6]...)
	pi := make([]byte, 32)
	pi[0], pi[1], pi[2] = 3, 4, byte(prefix6.Bits())
	pi[3] = 0xc0
	binary.BigEndian.PutUint32(pi[4:8], 0xffffffff)
	binary.BigEndian.PutUint32(pi[8:12], 0xffffffff)
	a := prefix6.Masked().Addr().As16()
	copy(pi[16:32], a[:])
	b = append(b, pi...)
	ri := []byte{24, 1, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(ri[4:8], 0xffffffff)
	return append(b, ri...)
}

func isRouterSolicitation(b []byte) bool {
	return len(b) >= 8 && b[0] == icmp6RouterSolicitation && b[1] == 0
}

const (
	ifaTemporary  = 0x01
	ifaDADFailed  = 0x08
	ifaDeprecated = 0x20
	ifaTentative  = 0x40
)

type addrs6 map[netip.Addr]uint32

func (s addrs6) best() netip.Addr {
	var c []netip.Addr
	for a, f := range s {
		if f&(ifaTentative|ifaDADFailed|ifaDeprecated) == 0 {
			c = append(c, a)
		}
	}
	sort.Slice(c, func(i, j int) bool {
		ti, tj := s[c[i]]&ifaTemporary != 0, s[c[j]]&ifaTemporary != 0
		if ti != tj {
			return tj
		}
		return c[i].Less(c[j])
	})
	if len(c) == 0 {
		return netip.Addr{}
	}
	return c[0]
}

type machineAddrs struct {
	prefix6 netip.Prefix
	global  addrs6
	links   map[netip.Addr]uint32
	local   map[uint32]addrs6
}

func newMachineAddrs(prefix6 netip.Prefix) *machineAddrs {
	return &machineAddrs{prefix6: prefix6, global: addrs6{}, links: map[netip.Addr]uint32{}, local: map[uint32]addrs6{}}
}

func (a *machineAddrs) apply(events []addrEvent) {
	for _, e := range events {
		switch {
		case a.prefix6.Contains(e.addr):
			if e.gone {
				delete(a.global, e.addr)
				delete(a.links, e.addr)
			} else {
				a.global[e.addr], a.links[e.addr] = e.flags, e.link
			}
		case e.addr.IsLinkLocalUnicast():
			if a.local[e.link] == nil {
				a.local[e.link] = addrs6{}
			}
			if e.gone {
				delete(a.local[e.link], e.addr)
			} else {
				a.local[e.link][e.addr] = e.flags
			}
		}
	}
}

func (a *machineAddrs) best() (addr, ll netip.Addr) {
	addr = a.global.best()
	if addr.IsValid() {
		ll = a.local[a.links[addr]].best()
	}
	return addr, ll
}

const (
	nlmsgError   = 2
	nlmsgDone    = 3
	rtmNewAddr   = 20
	rtmDelAddr   = 21
	ifaAddress   = 1
	ifaLocal     = 2
	ifaFlags     = 8
	afInet6      = 10
	nlmsgHdrLen  = 16
	ifaddrmsgLen = 8
	scopeLink    = 253
)

type addrEvent struct {
	addr  netip.Addr
	flags uint32
	link  uint32
	gone  bool
}

func addrEvents(b []byte) (out []addrEvent) {
	le := binary.LittleEndian
	for len(b) >= nlmsgHdrLen {
		n := int(le.Uint32(b[0:4]))
		if n < nlmsgHdrLen || n > len(b) {
			return out
		}
		typ := le.Uint16(b[4:6])
		body := b[nlmsgHdrLen:n]
		if (typ == rtmNewAddr || typ == rtmDelAddr) && len(body) >= ifaddrmsgLen && body[0] == afInet6 && (body[3] == 0 || body[3] == scopeLink) {
			flags := uint32(body[2])
			var addr, local netip.Addr
			for at := body[ifaddrmsgLen:]; len(at) >= 4; {
				l := int(le.Uint16(at[0:2]))
				if l < 4 || l > len(at) {
					break
				}
				v := at[4:l]
				switch le.Uint16(at[2:4]) {
				case ifaAddress:
					if len(v) == 16 {
						addr = netip.AddrFrom16([16]byte(v))
					}
				case ifaLocal:
					if len(v) == 16 {
						local = netip.AddrFrom16([16]byte(v))
					}
				case ifaFlags:
					if len(v) == 4 {
						flags = le.Uint32(v)
					}
				}
				l = (l + 3) &^ 3
				if l > len(at) {
					break
				}
				at = at[l:]
			}
			if local.IsValid() {
				addr = local
			}
			if addr.IsValid() {
				out = append(out, addrEvent{addr: addr, flags: flags, link: le.Uint32(body[4:8]), gone: typ == rtmDelAddr})
			}
		}
		n = (n + 3) &^ 3
		if n > len(b) {
			return out
		}
		b = b[n:]
	}
	return out
}

func describe6(a netip.Addr) string {
	if !a.IsValid() {
		return "no IPv6 address"
	}
	return "the IPv6 address " + a.String()
}
