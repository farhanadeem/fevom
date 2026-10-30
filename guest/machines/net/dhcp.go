package machnet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
)

const (
	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpDecline  = 4
	dhcpAck      = 5
	dhcpNak      = 6
	dhcpRelease  = 7
	dhcpInform   = 8
)

const (
	optPad        = 0
	optSubnetMask = 1
	optRouter     = 3
	optDNS        = 6
	optRequested  = 50
	optLeaseTime  = 51
	optType       = 53
	optServerID   = 54
	optEnd        = 255
)

var dhcpMagic = [4]byte{99, 130, 83, 99}

var LeaseSeconds uint32 = infiniteLease

const infiniteLease = 0xffffffff

const headerLen = 240

const minReply = 300

const dhcpTypeAt = headerLen + 2

func messageName(t byte) string {
	names := map[byte]string{dhcpDiscover: "DISCOVER", dhcpOffer: "OFFER", dhcpRequest: "REQUEST", dhcpDecline: "DECLINE",
		dhcpAck: "ACK", dhcpNak: "NAK", dhcpRelease: "RELEASE", dhcpInform: "INFORM"}
	if n, ok := names[t]; ok {
		return n
	}
	return fmt.Sprintf("type %d", t)
}

type request struct {
	xid       [4]byte
	flags     uint16
	ciaddr    netip.Addr
	giaddr    [4]byte
	chaddr    net.HardwareAddr
	typ       byte
	requested netip.Addr
	serverID  netip.Addr
	hasServer bool
}

var errNotRequest = errors.New("not a DHCP request")

func parseRequest(b []byte) (*request, error) {
	if len(b) < headerLen || b[0] != 1 || b[1] != 1 || b[2] != 6 || [4]byte(b[236:240]) != dhcpMagic {
		return nil, errNotRequest
	}
	r := &request{
		flags:  binary.BigEndian.Uint16(b[10:12]),
		ciaddr: netip.AddrFrom4([4]byte(b[12:16])),
		giaddr: [4]byte(b[24:28]),
		chaddr: net.HardwareAddr(append([]byte(nil), b[28:34]...)),
	}
	copy(r.xid[:], b[4:8])
	opts := b[headerLen:]
	for len(opts) > 0 {
		code := opts[0]
		if code == optPad {
			opts = opts[1:]
			continue
		}
		if code == optEnd || len(opts) < 2 {
			break
		}
		n := int(opts[1])
		if len(opts) < 2+n {
			return nil, errors.New("an option runs past the message")
		}
		v := opts[2 : 2+n]
		switch code {
		case optType:
			if n == 1 {
				r.typ = v[0]
			}
		case optRequested:
			if n == 4 {
				r.requested = netip.AddrFrom4([4]byte(v))
			}
		case optServerID:
			if n == 4 {
				r.serverID, r.hasServer = netip.AddrFrom4([4]byte(v)), true
			}
		}
		opts = opts[2+n:]
	}
	if r.typ == 0 {
		return nil, errNotRequest
	}
	return r, nil
}

type lan struct {
	server netip.Addr
	prefix netip.Prefix
	lease  uint32
}

func (l lan) reply(r *request, typ byte, addr netip.Addr) []byte {
	b := make([]byte, headerLen, minReply+64)
	b[0], b[1], b[2] = 2, 1, 6
	copy(b[4:8], r.xid[:])
	binary.BigEndian.PutUint16(b[10:12], r.flags)
	if typ != dhcpNak && r.ciaddr.IsValid() && !r.ciaddr.IsUnspecified() {
		ci := r.ciaddr.As4()
		copy(b[12:16], ci[:])
	}
	if addr.IsValid() {
		yi := addr.As4()
		copy(b[16:20], yi[:])
	}
	copy(b[24:28], r.giaddr[:])
	copy(b[28:34], r.chaddr)
	copy(b[236:240], dhcpMagic[:])
	opt := func(code byte, v []byte) { b = append(append(b, code, byte(len(v))), v...) }
	addr4 := func(a netip.Addr) []byte { v := a.As4(); return v[:] }
	opt(optType, []byte{typ})
	opt(optServerID, addr4(l.server))
	if typ != dhcpNak {
		if typ != dhcpAck || addr.IsValid() {
			lease := make([]byte, 4)
			binary.BigEndian.PutUint32(lease, l.lease)
			opt(optLeaseTime, lease)
		}
		mask := net.CIDRMask(l.prefix.Bits(), 32)
		opt(optSubnetMask, mask)
		opt(optRouter, addr4(l.server))
		opt(optDNS, addr4(l.server))
	}
	b = append(b, optEnd)
	for len(b) < minReply {
		b = append(b, optPad)
	}
	return b
}

func (l lan) answer(r *request, addr netip.Addr) (reply []byte, acked bool) {
	if !addr.IsValid() {
		return nil, false
	}
	switch r.typ {
	case dhcpDiscover:
		return l.reply(r, dhcpOffer, addr), false
	case dhcpRequest:
		if r.hasServer && r.serverID != l.server {
			return nil, false
		}
		want := r.requested
		if !want.IsValid() {
			want = r.ciaddr
		}
		if want != addr {
			return l.reply(r, dhcpNak, netip.Addr{}), false
		}
		return l.reply(r, dhcpAck, addr), true
	case dhcpInform:
		return l.reply(r, dhcpAck, netip.Addr{}), false
	}
	return nil, false
}

func destination(r *request) netip.AddrPort {
	if r.ciaddr.IsValid() && !r.ciaddr.IsUnspecified() {
		return netip.AddrPortFrom(r.ciaddr, 68)
	}
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{255, 255, 255, 255}), 68)
}
