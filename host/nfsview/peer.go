package nfsview

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"golang.org/x/sys/unix"
)

const (
	pcbSysctl    = "net.inet.tcp.pcblist_n"
	xsoSocket    = 0x001
	xsoInpcb     = 0x010
	inpcbFport   = 16
	inpcbLport   = 18
	inpcbVflag   = 44
	inpcbFaddr4  = 60
	inpcbLaddr4  = 76
	inpIPv4      = 0x1
	socketUID    = 64
	recordHeader = 8
)

func peerUID(local, remote *net.TCPAddr) (uint32, error) {
	tab, err := unix.SysctlRaw(pcbSysctl)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", pcbSysctl, err)
	}
	return uidInTable(tab, remote.AddrPort(), local.AddrPort())
}

var errNoPeer = errors.New("no connection with those addresses and ports in the kernel's table")

func uidInTable(tab []byte, self, other netip.AddrPort) (uint32, error) {
	if len(tab) < 4 {
		return 0, errors.New("the kernel's table of connections is empty")
	}
	self, other = netip.AddrPortFrom(self.Addr().Unmap(), self.Port()), netip.AddrPortFrom(other.Addr().Unmap(), other.Port())
	if !self.Addr().Is4() || !other.Addr().Is4() {
		return 0, errNoPeer
	}
	laddr, faddr := self.Addr().As4(), other.Addr().As4()
	off := int(binary.LittleEndian.Uint32(tab))
	match, found := false, false
	var uid uint32
	for off+recordHeader <= len(tab) {
		n := int(binary.LittleEndian.Uint32(tab[off:]))
		kind := binary.LittleEndian.Uint32(tab[off+4:])
		if n < recordHeader || off+n > len(tab) {
			break
		}
		rec := tab[off : off+n]
		switch kind {
		case xsoInpcb:
			match = n >= inpcbLaddr4+4 &&
				rec[inpcbVflag]&inpIPv4 != 0 &&
				binary.BigEndian.Uint16(rec[inpcbLport:]) == self.Port() &&
				binary.BigEndian.Uint16(rec[inpcbFport:]) == other.Port() &&
				[4]byte(rec[inpcbLaddr4:inpcbLaddr4+4]) == laddr &&
				[4]byte(rec[inpcbFaddr4:inpcbFaddr4+4]) == faddr
		case xsoSocket:
			if match && n >= socketUID+4 {
				u := binary.LittleEndian.Uint32(rec[socketUID:])
				if found && u != uid {
					return 0, fmt.Errorf("two connections %v to %v in the kernel's table, of uids %d and %d", self, other, uid, u)
				}
				uid, found = u, true
			}
			match = false
		}
		off += (n + 7) &^ 7
	}
	if !found {
		return 0, errNoPeer
	}
	return uid, nil
}
