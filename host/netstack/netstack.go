package netstack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

const nicID = 1

var GatewayMAC = tcpip.LinkAddress([]byte{0x02, 0x00, 0xbe, 0x27, 0x00, 0x01})

type Host interface {
	DialTCP(ctx context.Context, addr netip.AddrPort) (net.Conn, error)
	DialUDP(ctx context.Context, addr netip.AddrPort) (UDPConn, error)
	ListenICMP() (PingConn, error)
	Resolver
}

type Options struct {
	Config         Config
	Host           Host
	Log            *slog.Logger
	NoHostLoopback *atomic.Bool
	IPv6Usable     func() bool
	Proxy          *ProxyOptions
	Zone           Zone
}

type Stack struct {
	cfg            Config
	host           Host
	log            *slog.Logger
	noHostLoopback *atomic.Bool
	ipv6Usable     func() bool
	zone           Zone
	stack          *stack.Stack
	link           *linkEndpoint

	ctx    context.Context
	cancel context.CancelFunc
	flows  sync.WaitGroup

	icmp      *icmpForwarder
	icmp6     *icmp6Forwarder
	udpErrors *udpErrorRelay
	syns      synQuotes
	mu        sync.Mutex
	dns       *dnsServer
	proxy     *proxyEndpoint
	proxyOpts *ProxyOptions

	macs sync.Map

	timers timerCounts

	tunnelRef atomic.Pointer[Tunnel]

	names answeredNames

	udpFlows udpFlowSet

	stopped chan struct{}
}

func (s *Stack) Timers() TimerCounts { return s.timers.read() }

func (s *Stack) FramesFromGuest() uint64 {
	if nic, ok := s.stack.NICInfo()[nicID]; ok {
		return nic.Stats.Rx.Packets.Value()
	}
	return 0
}

func (s *Stack) learn(next func(stack.TransportEndpointID, *stack.PacketBuffer) bool) func(stack.TransportEndpointID, *stack.PacketBuffer) bool {
	return func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
		if eth := header.Ethernet(pkt.LinkHeader().Slice()); len(eth) >= header.EthernetMinimumSize {
			s.macs.Store(fromAddr(id.RemoteAddress), eth.SourceAddress())
		}
		return next(id, pkt)
	}
}

func (s *Stack) guestMAC(addr netip.Addr) tcpip.LinkAddress {
	if mac, ok := s.macs.Load(addr); ok {
		return mac.(tcpip.LinkAddress)
	}
	return header.EthernetBroadcastAddress
}

func toAddr(a netip.Addr) tcpip.Address {
	if a.Is6() && !a.Is4In6() {
		return tcpip.AddrFrom16(a.As16())
	}
	return tcpip.AddrFrom4(a.Unmap().As4())
}

func fromAddr(a tcpip.Address) netip.Addr {
	switch a.Len() {
	case 4:
		return netip.AddrFrom4(a.As4())
	case 16:
		return netip.AddrFrom16(a.As16())
	}
	return netip.Addr{}
}

func New(conn net.Conn, opts Options) (*Stack, error) {
	return newStack(conn, opts, nil)
}

func newStack(conn net.Conn, opts Options, onSend func(frameLen int)) (*Stack, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if !opts.Config.Subnet.IsValid() {
		opts.Config = DefaultConfig
	}
	if opts.Host == nil {
		return nil, errors.New("netstack: no Host")
	}
	ctx, cancel := context.WithCancel(context.Background())
	ns := &Stack{cfg: opts.Config, host: opts.Host, log: opts.Log, ctx: ctx, cancel: cancel, noHostLoopback: opts.NoHostLoopback, ipv6Usable: opts.IPv6Usable, proxyOpts: opts.Proxy, zone: opts.Zone, stopped: make(chan struct{})}
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol, ipv6.NewProtocolWithOptions(ipv6Options())},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6},
		Clock:              newCountedClock(&ns.timers),
	})
	ns.stack = s

	fail := func(what string, err tcpip.Error) (*Stack, error) {
		cancel()
		s.Destroy()
		return nil, fmt.Errorf("netstack: %s: %v", what, err)
	}
	sack := tcpip.TCPSACKEnabled(true)
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack); err != nil {
		return fail("enable SACK", err)
	}
	moderate := tcpip.TCPModerateReceiveBufferOption(true)
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &moderate); err != nil {
		return fail("enable receive buffer moderation", err)
	}

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		cancel()
		s.Destroy()
		return nil, errors.New("netstack: the link to the VMM must be a unix socket")
	}
	ns.link = newLinkEndpoint(uc, GatewayMAC)
	ns.link.onSend = onSend
	ns.link.ended = ns.shutdown
	ns.icmp = newICMPForwarder(ns)
	ns.icmp6 = newICMP6Forwarder(ns)
	ns.udpErrors = &udpErrorRelay{s: ns, flows: map[uint16]*udpFlow{}}
	ns.link.start(ns.bringUp)
	return ns, nil
}

func (ns *Stack) bringUp(uint32) error {
	s := ns.stack
	ns.log.Debug("guest interface", "segmentation offload", ns.link.offload.Load())
	fail := func(what string, err tcpip.Error) error {
		return fmt.Errorf("netstack: %s: %v", what, err)
	}
	if err := s.CreateNIC(nicID, ethernet.New(ns.link)); err != nil {
		return fail("create interface", err)
	}
	gateway := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: toAddr(ns.cfg.Gateway), PrefixLen: ns.cfg.Subnet.Bits()},
	}
	if err := s.AddProtocolAddress(nicID, gateway, stack.AddressProperties{}); err != nil {
		return fail("assign gateway address", err)
	}
	if ns.cfg.Host.IsValid() {
		host := tcpip.ProtocolAddress{
			Protocol:          ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{Address: toAddr(ns.cfg.Host), PrefixLen: ns.cfg.Subnet.Bits()},
		}
		if err := s.AddProtocolAddress(nicID, host, stack.AddressProperties{}); err != nil {
			return fail("assign host address", err)
		}
	}
	s.SetRouteTable(append([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}}, ns.bringUp6()...))
	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		return fail("promiscuous mode", err)
	}
	if err := s.SetSpoofing(nicID, true); err != nil {
		return fail("spoofing", err)
	}

	dns, err := newDNSServer(ns)
	if err != nil {
		return err
	}
	proxy, err := newProxyEndpoint(ns, ns.proxyOpts)
	if err != nil {
		dns.close()
		return err
	}
	ns.mu.Lock()
	ns.dns = dns
	ns.proxy = proxy
	ns.mu.Unlock()
	tcpForwarder := tcp.NewForwarder(s, 0, synQuotesMax, ns.forwardTCP)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
		ns.syns.keep(id, pkt)
		return tcpForwarder.HandlePacket(id, pkt)
	})
	s.SetTransportProtocolHandler(udp.ProtocolNumber, ns.learn(udp.NewForwarder(s, ns.forwardUDP).HandlePacket))
	s.SetTransportProtocolHandler(icmp.ProtocolNumber4, ns.icmp.handle)
	s.SetTransportProtocolHandler(icmp.ProtocolNumber6, ns.icmp6.handle)
	return nil
}

func (s *Stack) Done() <-chan struct{} { return s.stopped }

func (s *Stack) Close() {
	s.link.Close()
	<-s.stopped
}

func (s *Stack) shutdown() {
	defer close(s.stopped)
	if t := s.tunnelRef.Load(); t != nil {
		t.detach(s)
	}
	s.cancel()
	s.mu.Lock()
	dns, proxy := s.dns, s.proxy
	s.mu.Unlock()
	if dns != nil {
		dns.close()
	}
	if proxy != nil {
		proxy.close()
	}
	s.icmp.close()
	s.icmp6.close()
	s.udpErrors.close()
	s.stack.Close()
}

func Serve(l net.Listener, opts Options) error {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		ns, err := New(conn, opts)
		if err != nil {
			opts.Log.Error("cannot start a stack for a new guest", "err", err)
			conn.Close()
			continue
		}
		opts.Log.Info("guest connected")
		go func() {
			<-ns.Done()
			opts.Log.Info("guest disconnected", "err", ns.link.err)
		}()
	}
}
