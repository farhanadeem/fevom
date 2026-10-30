package machnet

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"fev/guest/internal/logbuf"
)

func listenDHCP(bridge string) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			for _, o := range [][2]int{{unix.SOL_SOCKET, unix.SO_REUSEADDR}, {unix.SOL_SOCKET, unix.SO_BROADCAST}} {
				if serr = unix.SetsockoptInt(int(fd), o[0], o[1], 1); serr != nil {
					return
				}
			}
			serr = unix.BindToDevice(int(fd), bridge)
		})
		if err != nil {
			return err
		}
		return serr
	}}
	c, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:67")
	if err != nil {
		return nil, err
	}
	return c.(*net.UDPConn), nil
}

func (n *Network) serveDHCP(c *net.UDPConn) {
	buf := make([]byte, 1500)
	for {
		k, _, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			n.logf(logbuf.Warning, "DHCP: %v", err)
			continue
		}
		req, err := parseRequest(buf[:k])
		if err != nil {
			continue
		}
		n.mu.Lock()
		m := n.byMAC[req.chaddr.String()]
		var addr netip.Addr
		if m != nil {
			addr = m.addr
		}
		n.mu.Unlock()
		out, acked := n.lan.answer(req, addr)
		if out == nil {
			continue
		}
		if _, err := c.WriteToUDPAddrPort(out, destination(req)); err != nil {
			n.logf(logbuf.Warning, "DHCP: reply to %s: %v", req.chaddr, err)
			continue
		}
		n.logf(logbuf.Debug, "DHCP: %s from %s (%s): %s %s", messageName(req.typ), req.chaddr, m.name, messageName(out[dhcpTypeAt]), addr)
		if acked {
			m.leaseOnce.Do(func() {
				n.logf(logbuf.Info, "%s has %s", m.name, addr)
				close(m.leased)
			})
		}
	}
}

func listenDNS(gateway netip.Addr) (*net.UDPConn, *net.TCPListener, error) {
	at := netip.AddrPortFrom(gateway, 53)
	u, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(at))
	if err != nil {
		return nil, nil, err
	}
	t, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(at))
	if err != nil {
		u.Close()
		return nil, nil, err
	}
	return u, t, nil
}

const (
	forwardBound = 5 * time.Second
	maxInFlight  = 128
	maxTCP       = 32
	tcpIdle      = 10 * time.Second
	maxMessage   = 4096
)

func (n *Network) serveDNS(c *net.UDPConn) {
	slots := make(chan struct{}, maxInFlight)
	for {
		buf := make([]byte, maxMessage)
		k, from, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		q := buf[:k]
		n.refresh6(from.Addr())
		a, fallback := n.res.decide(q)
		if a != nil {
			c.WriteToUDPAddrPort(a, from)
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			continue
		}
		go func() {
			defer func() { <-slots }()
			a := n.forwardUDP(q)
			if fallback != nil && useFallback(a) {
				a = fallback
			}
			if a != nil {
				c.WriteToUDPAddrPort(a, from)
			}
		}()
	}
}

func (n *Network) forwardUDP(q []byte) []byte {
	up, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(n.o.Upstream))
	if err != nil {
		return nil
	}
	defer up.Close()
	up.SetDeadline(time.Now().Add(forwardBound))
	if _, err := up.Write(q); err != nil {
		return nil
	}
	buf := make([]byte, maxMessage)
	k, err := up.Read(buf)
	if err != nil || k < 2 || buf[0] != q[0] || buf[1] != q[1] {
		return nil
	}
	return buf[:k]
}

func (n *Network) serveDNSTCP(l *net.TCPListener) {
	slots := make(chan struct{}, maxTCP)
	for {
		c, err := l.AcceptTCP()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			defer c.Close()
			if ra, ok := c.RemoteAddr().(*net.TCPAddr); ok {
				n.refresh6(ra.AddrPort().Addr())
			}
			for {
				c.SetReadDeadline(time.Now().Add(tcpIdle))
				q, err := readTCPMessage(c)
				if err != nil {
					return
				}
				c.SetReadDeadline(time.Time{})
				a, fallback := n.res.decide(q)
				if a == nil {
					if a = n.forwardTCP(q); fallback != nil && useFallback(a) {
						a = fallback
					}
					if a == nil {
						return
					}
				}
				if err := writeTCPMessage(c, a); err != nil {
					return
				}
			}
		}()
	}
}

func (n *Network) forwardTCP(q []byte) []byte {
	up, err := net.DialTimeout("tcp4", n.o.Upstream.String(), forwardBound)
	if err != nil {
		return nil
	}
	defer up.Close()
	up.SetDeadline(time.Now().Add(forwardBound))
	if writeTCPMessage(up, q) != nil {
		return nil
	}
	a, err := readTCPMessage(up)
	if err != nil {
		return nil
	}
	return a
}

func readTCPMessage(r io.Reader) ([]byte, error) {
	var l [2]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

func writeTCPMessage(w io.Writer, b []byte) error {
	out := make([]byte, 2+len(b))
	binary.BigEndian.PutUint16(out, uint16(len(b)))
	copy(out[2:], b)
	_, err := w.Write(out)
	return err
}
