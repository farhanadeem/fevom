package netstack

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"syscall"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

type MacHost struct {
	macResolver
}

func (MacHost) DialTCP(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network("tcp", addr.Addr()), addr.String())
}

func (MacHost) DialUDP(ctx context.Context, addr netip.AddrPort) (UDPConn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, network("udp", addr.Addr()), addr.String())
	if err != nil {
		return nil, err
	}
	conn := c.(*net.UDPConn)
	raw, err := conn.SyscallConn()
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &macUDPConn{UDPConn: conn, raw: rawControl{raw}, v6: !addr.Addr().Unmap().Is4()}, nil
}

func network(kind string, a netip.Addr) string {
	if a.Unmap().Is4() {
		return kind + "4"
	}
	return kind + "6"
}

type macUDPConn struct {
	*net.UDPConn
	raw rawControl
	v6  bool
}

func (c *macUDPConn) SetTTL(ttl int) error {
	if c.v6 {
		return setHops6(c.raw, ttl)
	}
	return setTTL(c.raw, ttl)
}

func (c *macUDPConn) LocalPort() uint16 {
	return uint16(c.LocalAddr().(*net.UDPAddr).Port)
}

func (MacHost) ListenICMP() (PingConn, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, syscall.IPPROTO_ICMP)
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{}); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "icmp")
	defer f.Close()
	pc, err := net.FilePacketConn(f)
	if err != nil {
		return nil, err
	}
	conn, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close()
		return nil, errors.New("netstack: the ICMP socket is not the kind expected")
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &hostPingConn{conn: conn, raw: rawControl{raw}, ttl: -1}, nil
}

func (MacHost) ListenICMP6() (PingConn, error) {
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_DGRAM, syscall.IPPROTO_ICMPV6)
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet6{}); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "icmp6")
	defer f.Close()
	pc, err := net.FilePacketConn(f)
	if err != nil {
		return nil, err
	}
	conn, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close()
		return nil, errors.New("netstack: the ICMPv6 socket is not the kind expected")
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &hostPingConn6{conn: conn, raw: rawControl{raw}, hops: -1}, nil
}

type hostPingConn6 struct {
	conn *net.UDPConn
	raw  rawControl
	hops int
}

func (c *hostPingConn6) Send(dst netip.Addr, ident, seq uint16, payload []byte, hops int) error {
	if hops != c.hops {
		if err := setHops6(c.raw, hops); err != nil {
			return err
		}
		c.hops = hops
	}
	msg := make([]byte, header.ICMPv6EchoMinimumSize+len(payload))
	msg[0] = byte(header.ICMPv6EchoRequest)
	binary.BigEndian.PutUint16(msg[4:], ident)
	binary.BigEndian.PutUint16(msg[6:], seq)
	copy(msg[header.ICMPv6EchoMinimumSize:], payload)
	_, err := c.conn.WriteToUDPAddrPort(msg, netip.AddrPortFrom(dst, 0))
	return err
}

func (c *hostPingConn6) Recv(buf []byte) (int, netip.Addr, error) {
	n, from, err := c.conn.ReadFromUDPAddrPort(buf)
	if err != nil {
		return 0, netip.Addr{}, err
	}
	return n, from.Addr().WithZone(""), nil
}

func (c *hostPingConn6) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }
func (c *hostPingConn6) Close() error                      { return c.conn.Close() }

func setHops6(raw interface{ Control(func(fd uintptr)) error }, hops int) error {
	var serr error
	err := raw.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_UNICAST_HOPS, hops)
	})
	if err != nil {
		return err
	}
	return serr
}

type rawControl struct{ c syscall.RawConn }

func (r rawControl) Control(fn func(fd uintptr)) error { return r.c.Control(fn) }

func setTTL(raw interface{ Control(func(fd uintptr)) error }, ttl int) error {
	var serr error
	err := raw.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
	})
	if err != nil {
		return err
	}
	return serr
}

type hostPingConn struct {
	conn *net.UDPConn
	raw  interface {
		Control(func(fd uintptr)) error
	}
	ttl int
}

func (c *hostPingConn) Send(dst netip.Addr, ident, seq uint16, payload []byte, ttl int) error {
	if ttl != c.ttl {
		if err := setTTL(c.raw, ttl); err != nil {
			return err
		}
		c.ttl = ttl
	}
	msg := make([]byte, header.ICMPv4MinimumSize+len(payload))
	msg[0] = byte(header.ICMPv4Echo)
	binary.BigEndian.PutUint16(msg[4:], ident)
	binary.BigEndian.PutUint16(msg[6:], seq)
	copy(msg[header.ICMPv4MinimumSize:], payload)
	binary.BigEndian.PutUint16(msg[2:], ^checksum.Checksum(msg, 0))
	_, err := c.conn.WriteToUDPAddrPort(msg, netip.AddrPortFrom(dst, 0))
	return err
}

func (c *hostPingConn) Recv(buf []byte) (int, netip.Addr, error) {
	n, from, err := c.conn.ReadFromUDPAddrPort(buf)
	if err != nil {
		return 0, netip.Addr{}, err
	}
	return stripIPv4Header(buf, n), from.Addr().Unmap(), nil
}

func (c *hostPingConn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }
func (c *hostPingConn) Close() error                      { return c.conn.Close() }
