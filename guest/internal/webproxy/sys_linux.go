package webproxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	api "fev/proto/agent"
)

const ip6tSOOriginalDst = 80

func OriginalDst(c net.Conn) (netip.AddrPort, error) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return netip.AddrPort{}, errors.New("not a socket")
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var out netip.AddrPort
	var serr error
	if la, ok := c.LocalAddr().(*net.TCPAddr); ok && la.IP.To4() == nil {
		err = raw.Control(func(fd uintptr) {
			m, err := unix.GetsockoptIPv6MTUInfo(int(fd), unix.SOL_IPV6, ip6tSOOriginalDst)
			if err != nil {
				serr = err
				return
			}
			if m.Addr.Family != unix.AF_INET6 {
				serr = fmt.Errorf("an original destination of family %d", m.Addr.Family)
				return
			}
			port := (*[2]byte)(unsafe.Pointer(&m.Addr.Port))
			out = netip.AddrPortFrom(netip.AddrFrom16(m.Addr.Addr), binary.BigEndian.Uint16(port[:]))
		})
		if err == nil {
			err = serr
		}
		return out, err
	}
	err = raw.Control(func(fd uintptr) {
		m, err := unix.GetsockoptIPv6Mreq(int(fd), unix.IPPROTO_IP, unix.SO_ORIGINAL_DST)
		if err != nil {
			serr = err
			return
		}
		b := m.Multiaddr
		if binary.LittleEndian.Uint16(b[0:2]) != unix.AF_INET {
			serr = fmt.Errorf("an original destination of family %d", binary.LittleEndian.Uint16(b[0:2]))
			return
		}
		out = netip.AddrPortFrom(netip.AddrFrom4([4]byte{b[4], b[5], b[6], b[7]}), binary.BigEndian.Uint16(b[2:4]))
	})
	if err == nil {
		err = serr
	}
	return out, err
}

type NSLister struct {
	NS *Namespaces
}

const tcpListen = 10

func (l NSLister) Listening(t api.DomainTarget) ([]Listener, error) {
	path, err := l.NS.Path(t)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	runtime.LockOSThread()
	own, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		runtime.UnlockOSThread()
		return nil, err
	}
	defer unix.Close(own)
	if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("enter %s: %w", path, err)
	}
	var out []Listener
	var derr error
	for _, fam := range []uint8{unix.AF_INET, unix.AF_INET6} {
		ss, err := netlink.SocketDiagTCP(fam)
		if err != nil {
			derr = err
			continue
		}
		for _, s := range ss {
			if s.State != tcpListen {
				continue
			}
			a, ok := netip.AddrFromSlice(s.ID.Source)
			if !ok {
				continue
			}
			out = append(out, Listener{Addr: a.Unmap(), Port: int(s.ID.SourcePort)})
		}
	}
	if err := unix.Setns(own, unix.CLONE_NEWNET); err != nil {
		return nil, fmt.Errorf("leave %s: %w", path, err)
	}
	runtime.UnlockOSThread()
	if len(out) == 0 && derr != nil {
		return nil, derr
	}
	return out, nil
}

func Main(args []string) {
	logger := log.New(os.Stderr, "", 0)
	ctrl := os.NewFile(ControlFD, "control")
	rep := &Reporter{W: ctrl}
	fail := func(format string, a ...any) {
		rep.Send(Report{Error: fmt.Sprintf(format, a...)})
		os.Exit(1)
	}
	if len(args) < 2 {
		fail("no tls.issue socket given")
	}
	var l80, l443 net.Listener
	var more []net.Listener
	if fds := os.Getenv(ListenFDsEnv); fds == "" {
		var err error
		if l80, l443, err = Listeners(""); err != nil {
			fail("%v", err)
		}
	} else {
		pairs, err := ListenerPairs(fds)
		if err != nil {
			fail("%v", err)
		}
		l80, l443 = pairs[0][0], pairs[0][1]
		for _, pr := range pairs[1:] {
			more = append(more, pr[0], pr[1])
		}
	}
	ns := &Namespaces{}
	p := New(Config{
		OriginalDst: OriginalDst,
		Lister:      NSLister{NS: ns},
		Issuer:      SocketIssuer{Path: args[1]},
		Unredirect:  func(a netip.AddrPort) { rep.Send(Report{Unredirect: addrPort(a)}) },
		Logf:        logger.Printf,
	})
	if err := Serve(ctrl, rep, l80, l443, p, ns, more...); err != nil {
		logger.Printf("webproxy: %v", err)
		os.Exit(1)
	}
	os.Exit(0)
}
