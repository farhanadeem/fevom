package machnet

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"fev/guest/internal/logbuf"
)

func bridgeQuiet6(dev string) error {
	for _, kv := range [][2]string{{"accept_ra", "0"}, {"router_solicitations", "0"}, {"dad_transmits", "0"}, {"accept_dad", "0"}} {
		if err := os.WriteFile("/proc/sys/net/ipv6/conf/"+dev+"/"+kv[0], []byte(kv[1]), 0); err != nil {
			return fmt.Errorf("set %s's %s: %w", dev, kv[0], err)
		}
	}
	return nil
}

func address6(br netlink.Link, gateway6 netip.Addr, prefix6 netip.Prefix) error {
	a := &netlink.Addr{IPNet: &net.IPNet{IP: gateway6.AsSlice(), Mask: net.CIDRMask(prefix6.Bits(), 128)}, Flags: unix.IFA_F_NODAD}
	return netlink.AddrReplace(br, a)
}

func listenRS(bridge string, index int) (*net.IPConn, error) {
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.IPPROTO_ICMPV6)
	if err != nil {
		return nil, err
	}
	setup := func() error {
		if err := unix.BindToDevice(fd, bridge); err != nil {
			return err
		}
		var f unix.ICMPv6Filter
		for i := range f.Data {
			f.Data[i] = 0xffffffff
		}
		f.Data[icmp6RouterSolicitation>>5] &^= 1 << (icmp6RouterSolicitation & 31)
		if err := unix.SetsockoptICMPv6Filter(fd, unix.SOL_ICMPV6, unix.ICMPV6_FILTER, &f); err != nil {
			return err
		}
		for _, o := range [][2]int{{unix.IPV6_RECVHOPLIMIT, 1}, {unix.IPV6_UNICAST_HOPS, 255}, {unix.IPV6_MULTICAST_HOPS, 255}, {unix.IPV6_MULTICAST_LOOP, 0}} {
			if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, o[0], o[1]); err != nil {
				return err
			}
		}
		mreq := &unix.IPv6Mreq{Interface: uint32(index)}
		copy(mreq.Multiaddr[:], net.ParseIP("ff02::2").To16())
		if err := unix.SetsockoptIPv6Mreq(fd, unix.IPPROTO_IPV6, unix.IPV6_JOIN_GROUP, mreq); err != nil && !errors.Is(err, unix.EADDRINUSE) {
			return err
		}
		return nil
	}
	if err := setup(); err != nil {
		unix.Close(fd)
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "icmp6:"+bridge)
	defer f.Close()
	c, err := net.FilePacketConn(f)
	if err != nil {
		return nil, err
	}
	ic, ok := c.(*net.IPConn)
	if !ok {
		c.Close()
		return nil, fmt.Errorf("not an IP socket: %T", c)
	}
	return ic, nil
}

func hopLimit(oob []byte) int {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return -1
	}
	for _, m := range msgs {
		if m.Header.Level == unix.IPPROTO_IPV6 && m.Header.Type == unix.IPV6_HOPLIMIT && len(m.Data) >= 4 {
			return int(int32(nativeUint32(m.Data[:4])))
		}
	}
	return -1
}

func nativeUint32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func (n *Network) serveRS(c *net.IPConn) {
	ra := n.ra
	buf := make([]byte, 1500)
	oob := make([]byte, 128)
	for {
		k, ok, _, from, err := c.ReadMsgIP(buf, oob)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			n.logf(logbuf.Warning, "router solicitations: %v", err)
			continue
		}
		src, _ := netip.AddrFromSlice(from.IP)
		if !isRouterSolicitation(buf[:k]) || hopLimit(oob[:ok]) != 255 || !src.IsLinkLocalUnicast() {
			continue
		}
		var m *member
		if mac := solicitationMAC(buf[:k]); mac != nil {
			n.mu.Lock()
			if m = n.byMAC[mac.String()]; m != nil {
				m.ll6, m.lastRA = src, time.Now()
			}
			n.mu.Unlock()
		}
		if _, _, err := c.WriteMsgIP(ra, nil, &net.IPAddr{IP: from.IP, Zone: BridgeName}); err != nil {
			n.logf(logbuf.Warning, "router advertisement to %s: %v", src, err)
			continue
		}
		n.logf(logbuf.Debug, "router advertisement of %s to %s (asked)", n.prefix6, src)
	}
}

func (n *Network) refresh6(from netip.Addr) {
	if n.rs == nil {
		return
	}
	from = from.Unmap()
	now := time.Now()
	n.mu.Lock()
	var to netip.Addr
	for _, m := range n.byName {
		if m.addr == from {
			if m.ll6.IsValid() && now.Sub(m.lastRA) >= n.refreshAfter {
				to, m.lastRA = m.ll6, now
			}
			break
		}
	}
	n.mu.Unlock()
	if !to.IsValid() {
		return
	}
	if _, _, err := n.rs.WriteMsgIP(n.ra, nil, &net.IPAddr{IP: to.AsSlice(), Zone: BridgeName}); err != nil {
		n.logf(logbuf.Warning, "router advertisement to %s: %v", to, err)
		return
	}
	n.logf(logbuf.Debug, "router advertisement of %s to %s (refreshed at a lookup)", n.prefix6, to)
}

func (n *Network) followAddrs6(netns int, changed func(addr, ll netip.Addr)) (func(), error) {
	f, err := addrSocketIn(netns)
	if err != nil {
		return nil, err
	}
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, err
	}
	go func() {
		defer f.Close()
		have := newMachineAddrs(n.prefix6)
		var best, ll netip.Addr
		buf := make([]byte, 32<<10)
		for {
			var k int
			var rerr error
			err := rc.Read(func(fd uintptr) bool {
				k, _, rerr = unix.Recvfrom(int(fd), buf, 0)
				return !errors.Is(rerr, unix.EAGAIN)
			})
			if err != nil {
				return
			}
			if rerr != nil {
				if errors.Is(rerr, unix.ENOBUFS) {
					have = newMachineAddrs(n.prefix6)
					askAddrs(f)
					continue
				}
				if errors.Is(rerr, unix.EINTR) {
					continue
				}
				n.logf(logbuf.Warning, "a machine's IPv6 addresses: %v", rerr)
				return
			}
			have.apply(addrEvents(buf[:k]))
			if b, l := have.best(); b != best || l != ll {
				best, ll = b, l
				changed(b, l)
			}
		}
	}()
	return func() { f.Close() }, nil
}

func addrSocketIn(netns int) (*os.File, error) {
	type result struct {
		fd  int
		err error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		own, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			runtime.UnlockOSThread()
			done <- result{-1, err}
			return
		}
		defer unix.Close(own)
		if err := unix.Setns(netns, unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			done <- result{-1, fmt.Errorf("enter the machine's network namespace: %w", err)}
			return
		}
		fd, serr := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.NETLINK_ROUTE)
		if err := unix.Setns(own, unix.CLONE_NEWNET); err != nil {
			if serr == nil {
				unix.Close(fd)
			}
			done <- result{-1, fmt.Errorf("leave the machine's network namespace: %w", err)}
			return
		}
		runtime.UnlockOSThread()
		done <- result{fd, serr}
	}()
	r := <-done
	if r.err != nil {
		return nil, r.err
	}
	if err := unix.Bind(r.fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: 1 << (unix.RTNLGRP_IPV6_IFADDR - 1)}); err != nil {
		unix.Close(r.fd)
		return nil, err
	}
	f := os.NewFile(uintptr(r.fd), "netlink:addresses")
	if err := askAddrs(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func askAddrs(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	req := make([]byte, nlmsgHdrLen+ifaddrmsgLen)
	putLE32(req[0:4], uint32(len(req)))
	putLE16(req[4:6], unix.RTM_GETADDR)
	putLE16(req[6:8], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	putLE32(req[8:12], 1)
	req[nlmsgHdrLen] = afInet6
	var serr error
	if err := rc.Write(func(fd uintptr) bool {
		serr = unix.Sendto(int(fd), req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK})
		return !errors.Is(serr, unix.EAGAIN)
	}); err != nil {
		return err
	}
	return serr
}

func putLE16(b []byte, v uint16) { b[0], b[1] = byte(v), byte(v>>8) }
func putLE32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}
