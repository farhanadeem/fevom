//go:build darwin

package privhelper

/*
#cgo LDFLAGS: -framework CoreFoundation -framework SystemConfiguration
#include <sys/types.h>
#include <sys/socket.h>
#include <sys/ioctl.h>
#include <sys/sockio.h>
#include <net/if.h>
#include <net/if_dl.h>
#include <net/route.h>
#include <netinet/in.h>
#include <netinet6/in6_var.h>
#include <netinet6/nd6.h>
#include <ifaddrs.h>
#include <errno.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static int fev_if_config(const char *ifname, const unsigned char *addr, const unsigned char *peer, int mtu) {
	int s = socket(AF_INET, SOCK_DGRAM, 0);
	if (s < 0) return errno;
	struct ifaliasreq ifra;
	memset(&ifra, 0, sizeof ifra);
	strlcpy(ifra.ifra_name, ifname, sizeof ifra.ifra_name);
	struct sockaddr_in *a = (struct sockaddr_in *)&ifra.ifra_addr;
	a->sin_len = sizeof *a; a->sin_family = AF_INET; memcpy(&a->sin_addr, addr, 4);
	struct sockaddr_in *d = (struct sockaddr_in *)&ifra.ifra_broadaddr;
	d->sin_len = sizeof *d; d->sin_family = AF_INET; memcpy(&d->sin_addr, peer, 4);
	struct sockaddr_in *m = (struct sockaddr_in *)&ifra.ifra_mask;
	m->sin_len = sizeof *m; m->sin_family = AF_INET; m->sin_addr.s_addr = 0xffffffff;
	int e = 0;
	if (ioctl(s, SIOCAIFADDR, &ifra) < 0) { e = errno; goto out; }
	struct ifreq ifr;
	memset(&ifr, 0, sizeof ifr);
	strlcpy(ifr.ifr_name, ifname, sizeof ifr.ifr_name);
	ifr.ifr_mtu = mtu;
	if (ioctl(s, SIOCSIFMTU, &ifr) < 0) { e = errno; goto out; }
	memset(&ifr, 0, sizeof ifr);
	strlcpy(ifr.ifr_name, ifname, sizeof ifr.ifr_name);
	if (ioctl(s, SIOCGIFFLAGS, &ifr) < 0) { e = errno; goto out; }
	ifr.ifr_flags |= IFF_UP;
	if (ioctl(s, SIOCSIFFLAGS, &ifr) < 0) { e = errno; goto out; }
out:
	close(s);
	return e;
}

static int fev_if_inet6(const char *ifname, const unsigned char *addr) {
	int s = socket(AF_INET6, SOCK_DGRAM, 0);
	if (s < 0) return errno;
	struct in6_aliasreq ifra;
	memset(&ifra, 0, sizeof ifra);
	strlcpy(ifra.ifra_name, ifname, sizeof ifra.ifra_name);
	ifra.ifra_addr.sin6_len = sizeof ifra.ifra_addr;
	ifra.ifra_addr.sin6_family = AF_INET6;
	memcpy(&ifra.ifra_addr.sin6_addr, addr, 16);
	ifra.ifra_prefixmask.sin6_len = sizeof ifra.ifra_prefixmask;
	ifra.ifra_prefixmask.sin6_family = AF_INET6;
	memset(&ifra.ifra_prefixmask.sin6_addr, 0xff, 16);
	ifra.ifra_lifetime.ia6t_vltime = ND6_INFINITE_LIFETIME;
	ifra.ifra_lifetime.ia6t_pltime = ND6_INFINITE_LIFETIME;
	int e = 0;
	if (ioctl(s, SIOCAIFADDR_IN6, &ifra) < 0) e = errno;
	close(s);
	return e;
}

static int fev_if_drop_linklocal(const char *ifname) {
	struct ifaddrs *list, *i;
	if (getifaddrs(&list) < 0) return -errno;
	int s = socket(AF_INET6, SOCK_DGRAM, 0);
	if (s < 0) { int e = errno; freeifaddrs(list); return -e; }
	int n = 0, e = 0;
	for (i = list; i; i = i->ifa_next) {
		if (!i->ifa_addr || i->ifa_addr->sa_family != AF_INET6 || strcmp(i->ifa_name, ifname) != 0) continue;
		struct sockaddr_in6 *sin6 = (struct sockaddr_in6 *)i->ifa_addr;
		if (!IN6_IS_ADDR_LINKLOCAL(&sin6->sin6_addr)) continue;
		struct in6_ifreq ifr;
		memset(&ifr, 0, sizeof ifr);
		strlcpy(ifr.ifr_name, ifname, sizeof ifr.ifr_name);
		memcpy(&ifr.ifr_addr, sin6, sizeof *sin6);
		if (ioctl(s, SIOCDIFADDR_IN6, &ifr) < 0) { e = errno; break; }
		n++;
	}
	close(s);
	freeifaddrs(list);
	return e ? -e : n;
}

#define FEV_ROUNDUP(a) ((a) > 0 ? (1 + (((a) - 1) | (sizeof(uint32_t) - 1))) : sizeof(uint32_t))

static int fev_route(int add, int family, const unsigned char *dst, int bits, const char *ifname, int seq, int reject) {
	struct ifaddrs *list, *i;
	struct sockaddr_dl sdl;
	int found = 0;
	if (getifaddrs(&list) < 0) return errno;
	for (i = list; i; i = i->ifa_next) {
		if (i->ifa_addr && i->ifa_addr->sa_family == AF_LINK && strcmp(i->ifa_name, ifname) == 0) {
			memset(&sdl, 0, sizeof sdl);
			memcpy(&sdl, i->ifa_addr, i->ifa_addr->sa_len < sizeof sdl ? i->ifa_addr->sa_len : sizeof sdl);
			sdl.sdl_len = sizeof sdl;
			found = 1;
			break;
		}
	}
	freeifaddrs(list);
	if (!found) return ENXIO;
	struct { struct rt_msghdr h; char space[512]; } m;
	memset(&m, 0, sizeof m);
	char *p = m.space;
	m.h.rtm_version = RTM_VERSION;
	m.h.rtm_type = add ? RTM_ADD : RTM_DELETE;
	m.h.rtm_flags = RTF_UP | RTF_STATIC | (reject ? RTF_REJECT : 0);
	m.h.rtm_addrs = RTA_DST | RTA_GATEWAY | RTA_NETMASK;
	m.h.rtm_seq = seq;
	if (family == AF_INET) {
		if (bits == 32) m.h.rtm_flags |= RTF_HOST;
		struct sockaddr_in d, k;
		memset(&d, 0, sizeof d); memset(&k, 0, sizeof k);
		d.sin_len = sizeof d; d.sin_family = AF_INET; memcpy(&d.sin_addr, dst, 4);
		k.sin_len = sizeof k; k.sin_family = AF_INET;
		k.sin_addr.s_addr = bits == 0 ? 0 : htonl(0xffffffffu << (32 - bits));
		memcpy(p, &d, sizeof d); p += FEV_ROUNDUP(sizeof d);
		memcpy(p, &sdl, sizeof sdl); p += FEV_ROUNDUP(sizeof sdl);
		memcpy(p, &k, sizeof k); p += FEV_ROUNDUP(sizeof k);
	} else {
		if (bits == 128) m.h.rtm_flags |= RTF_HOST;
		struct sockaddr_in6 d, k;
		memset(&d, 0, sizeof d); memset(&k, 0, sizeof k);
		d.sin6_len = sizeof d; d.sin6_family = AF_INET6; memcpy(&d.sin6_addr, dst, 16);
		k.sin6_len = sizeof k; k.sin6_family = AF_INET6;
		for (int b = 0; b < bits; b++) k.sin6_addr.s6_addr[b / 8] |= 0x80 >> (b % 8);
		memcpy(p, &d, sizeof d); p += FEV_ROUNDUP(sizeof d);
		memcpy(p, &sdl, sizeof sdl); p += FEV_ROUNDUP(sizeof sdl);
		memcpy(p, &k, sizeof k); p += FEV_ROUNDUP(sizeof k);
	}
	m.h.rtm_msglen = (u_short)(p - (char *)&m);
	int s = socket(PF_ROUTE, SOCK_RAW, 0);
	if (s < 0) return errno;
	int e = 0;
	if (write(s, &m, m.h.rtm_msglen) < 0) e = errno;
	close(s);
	return e;
}
*/
import "C"

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"fev/host/nat64"
)

type Darwin struct{}

var _ System = Darwin{}

const (
	sysprotoControl = 2
	utunOptIfname   = 2
	utunControl     = "com.apple.net.utun_control"
)

func (Darwin) NAT64Prefixes() ([]netip.Prefix, error) { return nat64.Prefixes() }

func (Darwin) Interfaces() ([]Interface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]Interface, 0, len(ifs))
	for _, i := range ifs {
		ifc := Interface{Name: i.Name, Index: i.Index}
		addrs, err := i.Addrs()
		if err != nil {
			return nil, fmt.Errorf("%s's addresses: %w", i.Name, err)
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			ones, _ := n.Mask.Size()
			ifc.Addrs = append(ifc.Addrs, netip.PrefixFrom(ip, ones))
		}
		out = append(out, ifc)
	}
	return out, nil
}

func (Darwin) Routes() ([]Route, error) {
	b, err := syscall.RouteRIB(syscall.NET_RT_DUMP, 0)
	if err != nil {
		return nil, fmt.Errorf("reading the routing table: %w", err)
	}
	return ParseRIB(b)
}

func (d Darwin) OpenTunnel(mtu int, mac, peer, mac6 netip.Addr) (int, string, error) {
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, sysprotoControl)
	if err != nil {
		return -1, "", fmt.Errorf("socket(PF_SYSTEM): %w", err)
	}
	unix.CloseOnExec(fd)
	fail := func(err error) (int, string, error) {
		unix.Close(fd)
		return -1, "", err
	}
	info := &unix.CtlInfo{}
	copy(info.Name[:], utunControl)
	if err := unix.IoctlCtlInfo(fd, info); err != nil {
		return fail(fmt.Errorf("CTLIOCGINFO: %w", err))
	}
	if err := unix.Connect(fd, &unix.SockaddrCtl{ID: info.Id, Unit: 0}); err != nil {
		return fail(fmt.Errorf("connecting the utun control: %w", err))
	}
	name, err := unix.GetsockoptString(fd, sysprotoControl, utunOptIfname)
	if err != nil {
		return fail(fmt.Errorf("UTUN_OPT_IFNAME: %w", err))
	}
	name = strings.TrimRight(name, "\x00")
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	a4, p4 := mac.As4(), peer.As4()
	if e := C.fev_if_config(cname, (*C.uchar)(&a4[0]), (*C.uchar)(&p4[0]), C.int(mtu)); e != 0 {
		return fail(fmt.Errorf("configuring %s: %w", name, syscall.Errno(e)))
	}
	if mac6.IsValid() {
		a6 := mac6.As16()
		if e := C.fev_if_inet6(cname, (*C.uchar)(&a6[0])); e != 0 {
			return fail(fmt.Errorf("giving %s %s: %w", name, mac6, syscall.Errno(e)))
		}
	} else if n := C.fev_if_drop_linklocal(cname); n < 0 {
		return fail(fmt.Errorf("removing %s's link-local address: %w", name, syscall.Errno(-n)))
	}
	return fd, name, nil
}

var routeSeq atomic.Int32

func (Darwin) route(add bool, ifname string, p netip.Prefix, reject bool) error {
	cname := C.CString(ifname)
	defer C.free(unsafe.Pointer(cname))
	b := p.Addr().As16()
	family, dst := C.int(unix.AF_INET6), b[:]
	if p.Addr().Is4() {
		a := p.Addr().As4()
		family, dst = C.int(unix.AF_INET), a[:]
	}
	op := C.int(0)
	if add {
		op = 1
	}
	rj := C.int(0)
	if reject {
		rj = 1
	}
	if e := C.fev_route(op, family, (*C.uchar)(&dst[0]), C.int(p.Bits()), cname, C.int(routeSeq.Add(1)), rj); e != 0 {
		return syscall.Errno(e)
	}
	return nil
}

func (d Darwin) AddRoute(ifname string, p netip.Prefix) error { return d.route(true, ifname, p, false) }
func (d Darwin) DeleteRoute(ifname string, p netip.Prefix) error {
	return d.route(false, ifname, p, false)
}

func (d Darwin) AddRejectRoute(p netip.Prefix) error    { return d.route(true, "lo0", p, true) }
func (d Darwin) DeleteRejectRoute(p netip.Prefix) error { return d.route(false, "lo0", p, true) }

func (Darwin) Unisolate(p netip.Prefix) (bool, error) {
	out, err := exec.Command("/sbin/pfctl", "-a", "com.apple.internet-sharing/network_isolation",
		"-t", "network_isolation_table_v4", "-T", "delete", p.String()).CombinedOutput()
	if strings.Contains(string(out), "1/1 addresses deleted") {
		return true, nil
	}
	if strings.Contains(string(out), "0/1 addresses deleted") || strings.Contains(string(out), "Table does not exist") {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pfctl: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return false, nil
}

func (Darwin) BindPort(proto string, ap netip.AddrPort) (int, error) {
	family := unix.AF_INET6
	var sa unix.Sockaddr = &unix.SockaddrInet6{Port: int(ap.Port()), Addr: ap.Addr().As16()}
	if ap.Addr().Is4() {
		family = unix.AF_INET
		sa = &unix.SockaddrInet4{Port: int(ap.Port()), Addr: ap.Addr().As4()}
	}
	typ := unix.SOCK_STREAM
	if proto == "udp" {
		typ = unix.SOCK_DGRAM
	}
	fd, err := unix.Socket(family, typ, 0)
	if err != nil {
		return -1, err
	}
	unix.CloseOnExec(fd)
	fail := func(err error) (int, error) {
		unix.Close(fd)
		return -1, err
	}
	if family == unix.AF_INET6 {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 1); err != nil {
			return fail(err)
		}
	}
	if err := unix.Bind(fd, sa); err != nil {
		return fail(err)
	}
	if typ == unix.SOCK_STREAM {
		if err := unix.Listen(fd, unix.SOMAXCONN); err != nil {
			return fail(err)
		}
	}
	return fd, nil
}

func (Darwin) CloseFD(fd int) { unix.Close(fd) }

func (Darwin) Probe(ctx context.Context, names []string, kinds []ProbeKind, d time.Duration) (map[string][]Answer, error) {
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < d {
		d = time.Until(dl)
	}
	return probe(names, kinds, d)
}
