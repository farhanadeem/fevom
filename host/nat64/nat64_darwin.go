//go:build darwin

package nat64

import (
	"errors"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/unix"
)

type ifNAT64Req struct {
	name     [unix.IFNAMSIZ]byte
	prefixes [4]struct {
		addr [16]byte
		len  uint32
	}
}

const siocGIFNAT64Prefix = 0xc0000000 | (uint32(unsafe.Sizeof(ifNAT64Req{}))&0x1fff)<<16 | 'i'<<8 | 193

func Of(name string) ([]netip.Prefix, error) {
	if len(name) >= unix.IFNAMSIZ {
		return nil, errors.New("an interface name too long")
	}
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	defer unix.Close(fd)
	var r ifNAT64Req
	copy(r.name[:], name)
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(siocGIFNAT64Prefix), uintptr(unsafe.Pointer(&r)))
	switch e {
	case 0:
	case unix.ENOENT:
		return nil, nil
	default:
		return nil, e
	}
	var out []netip.Prefix
	for _, p := range r.prefixes {
		if p.len == 0 {
			continue
		}
		if pf, err := netip.AddrFrom16(p.addr).Prefix(int(p.len) * 8); err == nil {
			out = append(out, pf)
		}
	}
	return out, nil
}
