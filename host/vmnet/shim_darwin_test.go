//go:build darwin && cgo

package vmnet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

var (
	testBase  = netip.MustParsePrefix("10.183.0.0/16")
	testMacIP = netip.MustParseAddr("10.183.3.1")
	testPeer  = netip.MustParseAddr("10.183.3.2")
	routed    = []netip.Prefix{netip.MustParsePrefix("10.183.0.0/24"), netip.MustParsePrefix("10.183.2.0/24"),
		netip.MustParsePrefix("10.183.4.0/22"), netip.MustParsePrefix("10.183.64.0/18")}
	theMacsMAC = net.HardwareAddr{0x3e, 0x22, 0xfb, 0x11, 0x22, 0x33}
	broadcast  = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
)

type rig struct {
	t   *testing.T
	i   *Interface
	mac *os.File
	tun *os.File
}

func newRig(t *testing.T, mtu int) *rig {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, opt := range []int{syscall.SO_RCVBUF, syscall.SO_SNDBUF} {
		syscall.SetsockoptInt(fds[0], syscall.SOL_SOCKET, opt, 1<<20)
	}
	syscall.SetNonblock(fds[0], true)
	i, err := start(Config{Base: testBase, Mac: testMacIP, Peer: testPeer, Prefixes: routed, MTU: mtu}, fds[1])
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, i: i, mac: os.NewFile(uintptr(fds[0]), "mac"), tun: i.Tunnel()}
	t.Cleanup(func() {
		i.Close()
		r.mac.Close()
		if r.tun != nil {
			r.tun.Close()
		}
	})
	return r
}

func (r *rig) send(f []byte) {
	r.t.Helper()
	if _, err := r.mac.Write(f); err != nil {
		r.t.Fatal(err)
	}
}

func read(t *testing.T, f *os.File, d time.Duration) []byte {
	t.Helper()
	f.SetReadDeadline(time.Now().Add(d))
	b := make([]byte, 70000)
	n, err := f.Read(b)
	if err != nil {
		return nil
	}
	return b[:n]
}

func arpRequest(sha net.HardwareAddr, spa, tpa netip.Addr) []byte {
	f := make([]byte, header.EthernetMinimumSize+header.ARPSize)
	header.Ethernet(f).Encode(&header.EthernetFields{SrcAddr: tcpip.LinkAddress(sha), DstAddr: tcpip.LinkAddress(broadcast), Type: header.ARPProtocolNumber})
	a := header.ARP(f[header.EthernetMinimumSize:])
	a.SetIPv4OverEthernet()
	a.SetOp(header.ARPRequest)
	copy(a.HardwareAddressSender(), sha)
	s, d := spa.As4(), tpa.As4()
	copy(a.ProtocolAddressSender(), s[:])
	copy(a.ProtocolAddressTarget(), d[:])
	return f
}

func ipv4Frame(dstMAC, srcMAC net.HardwareAddr, src, dst netip.Addr, proto tcpip.TransportProtocolNumber, payload []byte) []byte {
	f := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+len(payload))
	header.Ethernet(f).Encode(&header.EthernetFields{SrcAddr: tcpip.LinkAddress(srcMAC), DstAddr: tcpip.LinkAddress(dstMAC), Type: header.IPv4ProtocolNumber})
	ip := header.IPv4(f[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{TotalLength: uint16(header.IPv4MinimumSize + len(payload)), TTL: 64, Protocol: uint8(proto),
		SrcAddr: tcpip.AddrFrom4(src.As4()), DstAddr: tcpip.AddrFrom4(dst.As4())})
	ip.SetChecksum(^ip.CalculateChecksum())
	copy(f[header.EthernetMinimumSize+header.IPv4MinimumSize:], payload)
	return f
}

func udp(src, dst netip.Addr, sport, dport uint16, payload []byte) []byte {
	b := make([]byte, header.UDPMinimumSize+len(payload))
	u := header.UDP(b)
	u.Encode(&header.UDPFields{SrcPort: sport, DstPort: dport, Length: uint16(len(b))})
	copy(b[header.UDPMinimumSize:], payload)
	xsum := header.PseudoHeaderChecksum(header.UDPProtocolNumber, tcpip.AddrFrom4(src.As4()), tcpip.AddrFrom4(dst.As4()), uint16(len(b)))
	u.SetChecksum(^checksum.Checksum(b, xsum))
	return b
}

func TestTheShimAnswersARPForFevomsAddressesAlone(t *testing.T) {
	r := newRig(t, 0)
	for _, c := range []struct {
		target string
		answer bool
	}{
		{"10.183.2.7", true}, {"10.183.4.1", true}, {"10.183.100.9", true}, {"10.183.0.5", true},
		{"10.183.3.2", true},
		{"10.183.3.1", false},
		{"10.183.1.2", false},
		{"10.183.200.1", false}, {"192.168.1.1", false},
	} {
		r.send(arpRequest(theMacsMAC, testMacIP, netip.MustParseAddr(c.target)))
		got := read(t, r.mac, 300*time.Millisecond)
		if !c.answer {
			if got != nil {
				t.Errorf("%s: answered: % x", c.target, got)
			}
			continue
		}
		if len(got) != 42 {
			t.Fatalf("%s: no reply (%d bytes)", c.target, len(got))
		}
		eth := header.Ethernet(got)
		a := header.ARP(got[header.EthernetMinimumSize:])
		tgt := netip.MustParseAddr(c.target).As4()
		mac4 := testMacIP.As4()
		if eth.DestinationAddress() != tcpip.LinkAddress(theMacsMAC) || eth.SourceAddress() != tcpip.LinkAddress(r.i.MAC()) ||
			a.Op() != header.ARPReply || !bytes.Equal(a.HardwareAddressSender(), r.i.MAC()) ||
			!bytes.Equal(a.ProtocolAddressSender(), tgt[:]) || !bytes.Equal(a.HardwareAddressTarget(), theMacsMAC) ||
			!bytes.Equal(a.ProtocolAddressTarget(), mac4[:]) {
			t.Fatalf("%s: reply % x", c.target, got)
		}
	}
	r.i.SetPrefixes([]netip.Prefix{netip.MustParsePrefix("10.183.200.0/24")})
	r.send(arpRequest(theMacsMAC, testMacIP, netip.MustParseAddr("10.183.200.1")))
	if read(t, r.mac, time.Second) == nil {
		t.Fatal("a new prefix's address not answered")
	}
	r.send(arpRequest(theMacsMAC, testMacIP, netip.MustParseAddr("10.183.2.7")))
	if got := read(t, r.mac, 300*time.Millisecond); got != nil {
		t.Fatal("a prefix taken away still answered")
	}
	if s := r.i.Stats(); s.ARPAnswered != 6 || s.ARPIgnored != 5 {
		t.Fatalf("stats %v", s)
	}
}

func TestTheMacsPacketsReachTheSpliceAsAUtunsWould(t *testing.T) {
	r := newRig(t, 0)
	payload := udp(testMacIP, netip.MustParseAddr("10.183.2.7"), 50000, 8080, []byte("hello"))
	f := ipv4Frame(r.i.MAC(), theMacsMAC, testMacIP, netip.MustParseAddr("10.183.2.7"), header.UDPProtocolNumber, payload)
	r.send(append(f, 0, 0, 0, 0))
	got := read(t, r.tun, time.Second)
	if len(got) != 4+len(f)-header.EthernetMinimumSize || binary.BigEndian.Uint32(got) != 2 || !bytes.Equal(got[4:], f[header.EthernetMinimumSize:]) {
		t.Fatalf("got % x", got)
	}
	r.send(ipv4Frame(net.HardwareAddr{2, 0, 0, 0, 0, 9}, theMacsMAC, testMacIP, netip.MustParseAddr("10.183.2.7"), header.UDPProtocolNumber, payload))
	v6 := make([]byte, 80)
	header.Ethernet(v6).Encode(&header.EthernetFields{SrcAddr: tcpip.LinkAddress(theMacsMAC), DstAddr: tcpip.LinkAddress(r.i.MAC()), Type: header.IPv6ProtocolNumber})
	r.send(v6)
	if got := read(t, r.tun, 300*time.Millisecond); got != nil {
		t.Fatalf("passed: % x", got)
	}
	s := r.i.Stats()
	if s.ToTunnel != 1 || s.DroppedOther != 1 || s.DroppedIPv6 != 1 {
		t.Fatalf("stats %v", s)
	}
}

func TestTheSplicesPacketsReachTheMacOnceItsAddressIsKnown(t *testing.T) {
	r := newRig(t, 0)
	pkt := ipv4Frame(nil, nil, netip.MustParseAddr("10.183.2.7"), testMacIP, header.UDPProtocolNumber,
		udp(netip.MustParseAddr("10.183.2.7"), testMacIP, 8080, 50000, []byte("back")))[header.EthernetMinimumSize:]
	utun := append([]byte{0, 0, 0, 2}, pkt...)
	if _, err := r.tun.Write(utun); err != nil {
		t.Fatal(err)
	}
	got := read(t, r.mac, time.Second)
	if len(got) != 42 || header.ARP(got[14:]).Op() != header.ARPRequest || header.Ethernet(got).DestinationAddress() != tcpip.LinkAddress(broadcast) {
		t.Fatalf("no ARP request: % x", got)
	}
	a := header.ARP(got[14:])
	mac4, peer4 := testMacIP.As4(), testPeer.As4()
	if !bytes.Equal(a.ProtocolAddressTarget(), mac4[:]) || !bytes.Equal(a.ProtocolAddressSender(), peer4[:]) {
		t.Fatalf("ARP request % x", got)
	}
	r.send(arpRequest(theMacsMAC, testMacIP, netip.MustParseAddr("10.183.2.7")))
	read(t, r.mac, time.Second)
	if _, err := r.tun.Write(utun); err != nil {
		t.Fatal(err)
	}
	got = read(t, r.mac, time.Second)
	eth := header.Ethernet(got)
	if got == nil || eth.DestinationAddress() != tcpip.LinkAddress(theMacsMAC) || eth.SourceAddress() != tcpip.LinkAddress(r.i.MAC()) ||
		eth.Type() != header.IPv4ProtocolNumber || !bytes.Equal(got[14:], pkt) {
		t.Fatalf("got % x", got)
	}
	r.tun.Write(append([]byte{0, 0, 0, 30}, pkt...))
	if got := read(t, r.mac, 300*time.Millisecond); got != nil {
		t.Fatalf("sent: % x", got)
	}
	if s := r.i.Stats(); s.DroppedNoMac != 1 || s.FromTunnel != 3 || s.Malformed != 1 {
		t.Fatalf("stats %v", s)
	}
}

func TestALargePacketCrossesWholeAtTheInterfacesMTU(t *testing.T) {
	r := newRig(t, 16384)
	r.send(arpRequest(theMacsMAC, testMacIP, netip.MustParseAddr("10.183.2.7")))
	read(t, r.mac, time.Second)
	big := make([]byte, 16384-header.IPv4MinimumSize)
	for k := range big {
		big[k] = byte(k)
	}
	f := ipv4Frame(r.i.MAC(), theMacsMAC, testMacIP, netip.MustParseAddr("10.183.2.7"), 253, big)
	r.send(f)
	if got := read(t, r.tun, time.Second); len(got) != 4+16384 {
		t.Fatalf("Mac to splice: %d bytes", len(got))
	}
	r.tun.Write(append([]byte{0, 0, 0, 2}, f[14:]...))
	if got := read(t, r.mac, time.Second); len(got) != 14+16384 {
		t.Fatalf("splice to Mac: %d bytes", len(got))
	}
}

func mdnsQuery(t *testing.T, qs ...dns.Question) []byte {
	m := new(dns.Msg)
	m.Question = qs
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTheMacsMDNSQuestionsForFevomsNamesAreAnswered(t *testing.T) {
	r := newRig(t, 0)
	r.i.SetZones([]string{"fev.local", "Shop.Local."})
	r.i.ServeMDNS(&Answerer{Lookup: testLookup})
	group := net.HardwareAddr{0x01, 0x00, 0x5e, 0x00, 0x00, 0xfb}
	ask := func(qs ...dns.Question) {
		m := mdnsQuery(t, qs...)
		r.send(ipv4Frame(group, theMacsMAC, testMacIP, Group, header.UDPProtocolNumber, udp(testMacIP, Group, 5353, 5353, m)))
	}
	ask(q("WEB.fev.local", dns.TypeA))
	got := read(t, r.mac, 2*time.Second)
	if got == nil {
		t.Fatal("no answer")
	}
	eth := header.Ethernet(got)
	ip := header.IPv4(got[14:])
	if eth.DestinationAddress() != tcpip.LinkAddress(group) || eth.SourceAddress() != tcpip.LinkAddress(r.i.MAC()) || ip.TTL() != 255 ||
		ip.SourceAddress() != tcpip.AddrFrom4(testPeer.As4()) || ip.DestinationAddress() != tcpip.AddrFrom4(Group.As4()) || !ip.IsChecksumValid() {
		t.Fatalf("frame % x", got[:42])
	}
	u := header.UDP(ip.Payload())
	if u.SourcePort() != 5353 || u.DestinationPort() != 5353 ||
		!u.IsChecksumValid(ip.SourceAddress(), ip.DestinationAddress(), checksum.Checksum(u.Payload(), 0)) {
		t.Fatalf("udp % x", ip.Payload()[:8])
	}
	var m dns.Msg
	if err := m.Unpack(u.Payload()); err != nil || len(m.Answer) != 1 || m.Answer[0].(*dns.A).A.String() != "10.183.4.2" {
		t.Fatalf("answer %v %v", err, m.Answer)
	}
	ask(q("api.shop.local", dns.TypeA))
	if got := read(t, r.mac, 500*time.Millisecond); got != nil {
		t.Fatalf("answered: % x", got)
	}
	ask(q("_companion-link._tcp.local", dns.TypePTR))
	ask(q("printer.local", dns.TypeA))
	resp := new(dns.Msg)
	resp.Response = true
	rr, _ := dns.NewRR("web.fev.local. 10 IN A 10.0.0.9")
	resp.Answer = []dns.RR{rr}
	rb, _ := resp.Pack()
	r.send(ipv4Frame(group, theMacsMAC, testMacIP, Group, header.UDPProtocolNumber, udp(testMacIP, Group, 5353, 5353, rb)))
	time.Sleep(300 * time.Millisecond)
	if s := r.i.Stats(); s.MDNSUp != 2 || s.MDNSIgnored != 3 || s.MDNSDown != 1 {
		t.Fatalf("stats %v", s)
	}
	lq := new(dns.Msg)
	lq.Id = 77
	lq.Question = []dns.Question{q("shop.local", dns.TypeA)}
	mb, _ := lq.Pack()
	r.send(ipv4Frame(group, theMacsMAC, testMacIP, Group, header.UDPProtocolNumber, udp(testMacIP, Group, 60000, 5353, mb)))
	got = read(t, r.mac, 2*time.Second)
	if got == nil {
		t.Fatal("no legacy answer")
	}
	ip = header.IPv4(got[14:])
	u = header.UDP(ip.Payload())
	if header.Ethernet(got).DestinationAddress() != tcpip.LinkAddress(theMacsMAC) || ip.DestinationAddress() != tcpip.AddrFrom4(testMacIP.As4()) || u.DestinationPort() != 60000 {
		t.Fatalf("legacy answer % x", got[:42])
	}
	var lm dns.Msg
	if err := lm.Unpack(u.Payload()); err != nil || lm.Id != 77 || len(lm.Answer) != 1 {
		t.Fatalf("legacy %v %v", err, lm)
	}
}

func TestCloseEndsTheShimAndKeepsItsCount(t *testing.T) {
	r := newRig(t, 0)
	r.i.ServeMDNS(&Answerer{Lookup: testLookup})
	r.send(arpRequest(theMacsMAC, testMacIP, netip.MustParseAddr("10.183.2.7")))
	read(t, r.mac, time.Second)
	r.i.Close()
	if s := r.i.Stats(); s.ARPAnswered != 1 || s.FramesIn != 1 {
		t.Fatalf("stats after close %v", s)
	}
	r.mac.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := r.mac.Write([]byte{1}); err == nil {
		t.Fatal("the shim's end still open")
	}
	r.i.Close()
}

func TestTheConfigurationIsChecked(t *testing.T) {
	for _, c := range []Config{
		{Base: netip.MustParsePrefix("fd00::/48"), Mac: testMacIP, Peer: testPeer},
		{Base: testBase, Mac: netip.MustParseAddr("10.9.3.1"), Peer: testPeer},
		{Base: testBase, Mac: testMacIP, Peer: testMacIP},
		{Base: testBase, Mac: testMacIP, Peer: testPeer, MTU: 16385},
		{Base: testBase, Mac: testMacIP, Peer: testPeer, MTU: 1400},
	} {
		if i, err := start(c, -1); err == nil {
			i.Close()
			t.Errorf("%+v started", c)
		}
	}
	for _, b := range []string{"198.19.0.0/16", "198.18.0.0/16", "100.80.0.0/16"} {
		base := netip.MustParsePrefix(b)
		a := base.Addr().As4()
		a[2] = 3
		mac, peer := a, a
		mac[3], peer[3] = 1, 2
		c := Config{Base: base, Mac: netip.AddrFrom4(mac), Peer: netip.AddrFrom4(peer)}
		if i, err := start(c, -1); !errors.Is(err, ErrNotPrivate) {
			if err == nil {
				i.Close()
			}
			t.Errorf("%s: %v, want ErrNotPrivate", b, err)
		}
	}
}
