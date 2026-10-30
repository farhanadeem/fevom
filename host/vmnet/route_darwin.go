//go:build darwin

package vmnet

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"syscall"
	"time"

	"golang.org/x/net/route"
)

type Route struct {
	Interface string
	Reject    bool
	Prefix    netip.Prefix
}

func RouteOf(addr netip.Addr) (Route, error) {
	if !addr.Is4() {
		return Route{}, fmt.Errorf("vmnet: %s is not an IPv4 address", addr)
	}
	fd, err := syscall.Socket(syscall.AF_ROUTE, syscall.SOCK_RAW, syscall.AF_UNSPEC)
	if err != nil {
		return Route{}, err
	}
	defer syscall.Close(fd)
	tv := syscall.NsecToTimeval(int64(2 * time.Second))
	syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	seq := int(time.Now().UnixNano() & 0x7fffffff)
	m := &route.RouteMessage{
		Version: syscall.RTM_VERSION, Type: syscall.RTM_GET, Flags: syscall.RTF_UP | syscall.RTF_HOST, ID: uintptr(os.Getpid()), Seq: seq,
		Addrs: []route.Addr{syscall.RTAX_DST: &route.Inet4Addr{IP: addr.As4()}},
	}
	b, err := m.Marshal()
	if err != nil {
		return Route{}, err
	}
	if _, err := syscall.Write(fd, b); err != nil {
		return Route{}, fmt.Errorf("vmnet: RTM_GET %s: %w", addr, err)
	}
	buf := make([]byte, 2048)
	for range 64 {
		n, err := syscall.Read(fd, buf)
		if err != nil {
			return Route{}, fmt.Errorf("vmnet: RTM_GET %s: %w", addr, err)
		}
		msgs, err := route.ParseRIB(route.RIBTypeRoute, buf[:n])
		if err != nil {
			continue
		}
		for _, msg := range msgs {
			rm, ok := msg.(*route.RouteMessage)
			if !ok || rm.Seq != seq || rm.ID != uintptr(os.Getpid()) || rm.Type != syscall.RTM_GET {
				continue
			}
			if rm.Err != nil {
				return Route{}, fmt.Errorf("vmnet: RTM_GET %s: %w", addr, rm.Err)
			}
			r := Route{Reject: rm.Flags&(syscall.RTF_REJECT|syscall.RTF_BLACKHOLE) != 0}
			if ifc, err := net.InterfaceByIndex(rm.Index); err == nil {
				r.Interface = ifc.Name
			}
			if len(rm.Addrs) > syscall.RTAX_NETMASK {
				if dst, ok := rm.Addrs[syscall.RTAX_DST].(*route.Inet4Addr); ok {
					bits := 32
					if rm.Flags&syscall.RTF_HOST == 0 {
						bits = 0
						if m, ok := rm.Addrs[syscall.RTAX_NETMASK].(*route.Inet4Addr); ok {
							bits, _ = net.IPMask(m.IP[:]).Size()
						}
					}
					r.Prefix = netip.PrefixFrom(netip.AddrFrom4(dst.IP), bits)
				}
			}
			return r, nil
		}
	}
	return Route{}, fmt.Errorf("vmnet: RTM_GET %s: no answer", addr)
}
