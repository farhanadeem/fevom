package forward

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"

	"fev/host/netstack"
	"fev/proto/relay"
)

const (
	maxSessions  = 1024
	sessionQueue = 128
)

type udpListener struct {
	t         *Table
	r         *row
	pc        *net.UDPConn
	localOnly bool
	wildcard  bool

	mu       sync.Mutex
	sessions map[flow]*udpSession
	closed   bool
}

type flow struct {
	client netip.AddrPort
	local  netip.Addr
}

type udpSession struct {
	u *udpListener
	flow
	from  []byte
	queue chan []byte
	idle  *netstack.Idle
	done  chan struct{}
	once  sync.Once
}

func (u *udpListener) serve() {
	defer u.t.wg.Done()
	buf := make([]byte, relay.MaxDatagram)
	var oob []byte
	if u.wildcard {
		oob = make([]byte, unix.CmsgSpace(unix.SizeofInet6Pktinfo))
	}
	for {
		n, oobn, _, from, err := u.pc.ReadMsgUDPAddrPort(buf, oob)
		if err != nil {
			break
		}
		if u.localOnly && !from.Addr().Unmap().IsLoopback() {
			continue
		}
		s := u.session(flow{from, arrivedAt(oob[:oobn])})
		if s == nil {
			continue
		}
		s.idle.Sent()
		select {
		case s.queue <- append([]byte(nil), buf[:n]...):
		default:
		}
	}
	u.mu.Lock()
	u.closed = true
	sessions := u.sessions
	u.sessions = map[flow]*udpSession{}
	u.mu.Unlock()
	for _, s := range sessions {
		s.end()
	}
}

func (u *udpListener) session(f flow) *udpSession {
	u.mu.Lock()
	defer u.mu.Unlock()
	if s, ok := u.sessions[f]; ok {
		return s
	}
	if len(u.sessions) >= maxSessions {
		return nil
	}
	u.t.mu.Lock()
	opener, guest := u.t.opener, u.r.guest
	u.t.mu.Unlock()
	if opener == nil {
		return nil
	}
	s := &udpSession{u: u, flow: f, from: leaveFrom(f.local), queue: make(chan []byte, sessionQueue), done: make(chan struct{})}
	s.idle = netstack.NewIdle(u.t.unreplied, u.t.assured, s.end)
	u.sessions[f] = s
	u.t.wg.Add(1)
	go s.run(opener, guest)
	return s
}

func (s *udpSession) end() {
	s.once.Do(func() {
		close(s.done)
		s.idle.Stop()
		s.u.mu.Lock()
		if s.u.sessions[s.flow] == s {
			delete(s.u.sessions, s.flow)
		}
		s.u.mu.Unlock()
	})
}

func (s *udpSession) run(opener Opener, guest netip.AddrPort) {
	defer s.u.t.wg.Done()
	defer s.end()
	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	stream, err := opener.Open(ctx, relay.Request{Kind: relay.UDP, Target: guest.String()})
	cancel()
	if err != nil {
		s.u.t.log.Debug("port forward: the guest did not take the datagrams", "id", s.u.r.id, "guest", guest, "err", err)
		return
	}
	defer stream.Close()

	back := make(chan struct{})
	go func() {
		defer close(back)
		buf := make([]byte, relay.MaxDatagram)
		for {
			n, err := relay.ReadDatagram(stream, buf)
			if err != nil {
				s.end()
				return
			}
			s.idle.Replied()
			if _, _, err := s.u.pc.WriteMsgUDPAddrPort(buf[:n], s.from, s.client); err != nil && s.from != nil {
				s.u.pc.WriteToUDPAddrPort(buf[:n], s.client)
			}
		}
	}()
	for {
		select {
		case datagram := <-s.queue:
			if relay.WriteDatagram(stream, datagram) != nil {
				stream.Close()
				<-back
				return
			}
		case <-s.done:
			stream.Close()
			<-back
			return
		}
	}
}

func tellArrival(pc *net.UDPConn, six bool) error {
	raw, err := pc.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		if six {
			serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_RECVPKTINFO, 1)
		} else {
			serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_RECVPKTINFO, 1)
		}
	}); err != nil {
		return err
	}
	return serr
}

func arrivedAt(oob []byte) netip.Addr {
	if len(oob) == 0 {
		return netip.Addr{}
	}
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return netip.Addr{}
	}
	for _, m := range msgs {
		switch {
		case m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_PKTINFO && len(m.Data) >= unix.SizeofInet4Pktinfo:
			info := (*unix.Inet4Pktinfo)(unsafe.Pointer(&m.Data[0]))
			return netip.AddrFrom4(info.Addr)
		case m.Header.Level == unix.IPPROTO_IPV6 && m.Header.Type == unix.IPV6_PKTINFO && len(m.Data) >= unix.SizeofInet6Pktinfo:
			info := (*unix.Inet6Pktinfo)(unsafe.Pointer(&m.Data[0]))
			addr := netip.AddrFrom16(info.Addr)
			if addr.IsLinkLocalUnicast() {
				if ifi, err := net.InterfaceByIndex(int(info.Ifindex)); err == nil {
					addr = addr.WithZone(ifi.Name)
				}
			}
			return addr
		}
	}
	return netip.Addr{}
}

func leaveFrom(addr netip.Addr) []byte {
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsMulticast() {
		return nil
	}
	if addr.Is4() {
		b := make([]byte, unix.CmsgSpace(unix.SizeofInet4Pktinfo))
		h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
		h.Level, h.Type = unix.IPPROTO_IP, unix.IP_PKTINFO
		h.SetLen(unix.CmsgLen(unix.SizeofInet4Pktinfo))
		info := (*unix.Inet4Pktinfo)(unsafe.Pointer(&b[unix.CmsgLen(0)]))
		info.Spec_dst = addr.As4()
		return b
	}
	b := make([]byte, unix.CmsgSpace(unix.SizeofInet6Pktinfo))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level, h.Type = unix.IPPROTO_IPV6, unix.IPV6_PKTINFO
	h.SetLen(unix.CmsgLen(unix.SizeofInet6Pktinfo))
	info := (*unix.Inet6Pktinfo)(unsafe.Pointer(&b[unix.CmsgLen(0)]))
	info.Addr = addr.As16()
	if zone := addr.Zone(); zone != "" {
		if ifi, err := net.InterfaceByName(zone); err == nil {
			info.Ifindex = uint32(ifi.Index)
		}
	}
	return b
}
