package fakesys

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"syscall"
	"time"

	"fev/host/privhelper"
)

type System struct {
	mu        sync.Mutex
	Ifaces    []privhelper.Interface
	Table     []privhelper.Route
	Resolvers []privhelper.Resolver
	HostName  string
	Answers   map[string][]privhelper.Answer
	ProbeErr  error
	Probed    []Probe
	Peers     map[string]int
	Bound     []netip.AddrPort
	Fail      map[string]error
	NAT64     []netip.Prefix
	Isolated  []netip.Prefix
	AfterOpen func(*System)
	nextUnit  int
	nextIdx   int
}

type Probe struct {
	Names []string
	Kinds []privhelper.ProbeKind
}

var _ privhelper.System = (*System)(nil)

func New() *System {
	return &System{
		Ifaces: []privhelper.Interface{
			{Name: "lo0", Index: 1, Addrs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/8"), netip.MustParsePrefix("::1/128")}},
			{Name: "en0", Index: 4, Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.1.5/24")}},
		},
		Table: []privhelper.Route{
			{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Index: 4, Flags: privhelper.RTFUp | privhelper.RTFGateway | privhelper.RTFStatic},
			{Prefix: netip.MustParsePrefix("127.0.0.0/8"), Index: 1, Flags: privhelper.RTFUp | privhelper.RTFStatic},
			{Prefix: netip.MustParsePrefix("192.168.1.0/24"), Index: 4, Flags: privhelper.RTFUp},
		},
		HostName: "fev-test-mac",
		Answers:  map[string][]privhelper.Answer{},
		Peers:    map[string]int{},
		Fail:     map[string]error{},
		nextIdx:  20,
	}
}

func (s *System) Interfaces() ([]privhelper.Interface, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]privhelper.Interface, len(s.Ifaces))
	for i, ifc := range s.Ifaces {
		ifc.Addrs = slices.Clone(ifc.Addrs)
		out[i] = ifc
	}
	return out, nil
}

func (s *System) NAT64Prefixes() ([]netip.Prefix, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Fail["nat64"]; err != nil {
		return nil, err
	}
	return slices.Clone(s.NAT64), nil
}

func (s *System) Routes() ([]privhelper.Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.Table), nil
}

func (s *System) OpenTunnel(mtu int, mac, peer, mac6 netip.Addr) (int, string, error) {
	fd, name, err := s.openTunnel(mac, peer, mac6)
	if err == nil && s.AfterOpen != nil {
		s.mu.Lock()
		s.AfterOpen(s)
		s.mu.Unlock()
	}
	return fd, name, err
}

func (s *System) openTunnel(mac, peer, mac6 netip.Addr) (int, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Fail["tunnel"]; err != nil {
		return -1, "", err
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return -1, "", err
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	name := fmt.Sprintf("utun%d", s.nextUnit)
	s.nextUnit++
	ifc := privhelper.Interface{Name: name, Index: s.nextIdx, Addrs: []netip.Prefix{netip.PrefixFrom(mac, 32)}}
	s.nextIdx++
	if mac6.IsValid() {
		ifc.Addrs = append(ifc.Addrs, netip.PrefixFrom(mac6, 128))
	}
	s.Ifaces = append(s.Ifaces, ifc)
	s.Table = append(s.Table, privhelper.Route{Prefix: netip.PrefixFrom(peer, 32), Index: ifc.Index, Flags: privhelper.RTFUp | privhelper.RTFHost})
	s.Peers[name] = fds[1]
	return fds[0], name, nil
}

func (s *System) Detach(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	s.Ifaces = slices.DeleteFunc(s.Ifaces, func(i privhelper.Interface) bool {
		if i.Name == name {
			idx = i.Index
			return true
		}
		return false
	})
	s.Table = slices.DeleteFunc(s.Table, func(r privhelper.Route) bool { return r.Index == idx })
	if fd, ok := s.Peers[name]; ok {
		syscall.Close(fd)
		delete(s.Peers, name)
	}
}

func (s *System) index(name string) (int, error) {
	for _, i := range s.Ifaces {
		if i.Name == name {
			return i.Index, nil
		}
	}
	return 0, syscall.ENXIO
}

func (s *System) AddRoute(ifname string, p netip.Prefix) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Fail["route.add"]; err != nil {
		return err
	}
	idx, err := s.index(ifname)
	if err != nil {
		return err
	}
	for _, r := range s.Table {
		if r.Prefix == p && r.Flags&privhelper.RTFIfScope == 0 {
			return syscall.EEXIST
		}
	}
	flags := privhelper.RTFUp | privhelper.RTFStatic
	if p.Bits() == p.Addr().BitLen() {
		flags |= privhelper.RTFHost
	}
	s.Table = append(s.Table, privhelper.Route{Prefix: p, Index: idx, Flags: flags, Gateway: fmt.Sprintf("link#%d", idx)})
	return nil
}

func (s *System) AddRejectRoute(p netip.Prefix) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Fail["reject.add"]; err != nil {
		return err
	}
	for _, r := range s.Table {
		if r.Prefix == p && r.Flags&privhelper.RTFIfScope == 0 {
			return syscall.EEXIST
		}
	}
	flags := privhelper.RTFUp | privhelper.RTFStatic | privhelper.RTFReject
	if p.Bits() == p.Addr().BitLen() {
		flags |= privhelper.RTFHost
	}
	s.Table = append(s.Table, privhelper.Route{Prefix: p, Index: 1, Flags: flags | 0x10000, Gateway: "link#1"})
	return nil
}

func (s *System) DeleteRejectRoute(p netip.Prefix) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.Table)
	s.Table = slices.DeleteFunc(s.Table, func(r privhelper.Route) bool { return r.Prefix == p && r.Flags&privhelper.RTFIfScope == 0 })
	if len(s.Table) == n {
		return syscall.ESRCH
	}
	return nil
}

func (s *System) Unisolate(p netip.Prefix) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Fail["unisolate"]; err != nil {
		return false, err
	}
	n := len(s.Isolated)
	s.Isolated = slices.DeleteFunc(s.Isolated, func(q netip.Prefix) bool { return q == p })
	return len(s.Isolated) != n, nil
}

func (s *System) DeleteRoute(ifname string, p netip.Prefix) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Fail["route.delete"]; err != nil {
		return err
	}
	idx, err := s.index(ifname)
	if err != nil {
		return err
	}
	n := len(s.Table)
	s.Table = slices.DeleteFunc(s.Table, func(r privhelper.Route) bool { return r.Prefix == p && r.Index == idx })
	if len(s.Table) == n {
		return syscall.ESRCH
	}
	return nil
}

func (s *System) BindPort(proto string, ap netip.AddrPort) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Fail["bind"]; err != nil {
		return -1, err
	}
	family := syscall.AF_INET6
	var sa syscall.Sockaddr = &syscall.SockaddrInet6{Addr: ap.Addr().As16()}
	if ap.Addr().Is4() {
		family = syscall.AF_INET
		sa = &syscall.SockaddrInet4{Addr: ap.Addr().As4()}
	}
	typ := syscall.SOCK_STREAM
	if proto == "udp" {
		typ = syscall.SOCK_DGRAM
	}
	fd, err := syscall.Socket(family, typ, 0)
	if err != nil {
		return -1, err
	}
	syscall.CloseOnExec(fd)
	if err := syscall.Bind(fd, sa); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	if typ == syscall.SOCK_STREAM {
		if err := syscall.Listen(fd, 16); err != nil {
			syscall.Close(fd)
			return -1, err
		}
	}
	s.Bound = append(s.Bound, ap)
	return fd, nil
}

func (s *System) CloseFD(fd int) { syscall.Close(fd) }

func (s *System) With(fn func(*System)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

func (s *System) PeerOf(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fd, ok := s.Peers[name]; ok {
		return fd
	}
	return -1
}

func (s *System) Probes() []Probe {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.Probed)
}

func (s *System) DNSResolvers() ([]privhelper.Resolver, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.Resolvers), nil
}

func (s *System) LocalHostName() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.HostName, nil
}

func (s *System) Probe(ctx context.Context, names []string, kinds []privhelper.ProbeKind, d time.Duration) (map[string][]privhelper.Answer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Probed = append(s.Probed, Probe{Names: slices.Clone(names), Kinds: slices.Clone(kinds)})
	if s.ProbeErr != nil {
		return nil, s.ProbeErr
	}
	out := map[string][]privhelper.Answer{}
	for _, n := range names {
		for _, a := range s.Answers[n] {
			if slices.Contains(kinds, a.Kind) {
				out[n] = append(out[n], a)
			}
		}
	}
	return out, nil
}
