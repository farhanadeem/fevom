package daemon

import (
	"encoding/binary"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"
)

func ipv4UDP(src, dst netip.Addr) []byte {
	pkt := make([]byte, 4+20+8+4)
	binary.BigEndian.PutUint32(pkt, syscall.AF_INET)
	ip := pkt[4:]
	ip[0], ip[8], ip[9] = 0x45, 64, syscall.IPPROTO_UDP
	binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
	s, d := src.As4(), dst.As4()
	copy(ip[12:], s[:])
	copy(ip[16:], d[:])
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(ip[i:]))
	}
	for sum > 0xffff {
		sum = sum&0xffff + sum>>16
	}
	binary.BigEndian.PutUint16(ip[10:], ^uint16(sum))
	binary.BigEndian.PutUint16(ip[20:], 40000)
	binary.BigEndian.PutUint16(ip[22:], 53)
	binary.BigEndian.PutUint16(ip[24:], 12)
	return pkt
}

func TestTheTunnelsDescriptorGoesToTheSplice(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	syscall.SetNonblock(fds[1], true)
	mac := os.NewFile(uintptr(fds[1]), "mac")
	defer mac.Close()
	g := &GuestNet{}
	g.SetPrefixes([]netip.Prefix{netip.MustParsePrefix("198.19.2.0/24")})
	tunnelMac, peer := netip.MustParseAddr("198.19.3.1"), netip.MustParseAddr("198.19.3.2")
	g.Attach(os.NewFile(uintptr(fds[0]), "utun"), TunnelEnds{Interface: "utun99", Mac: tunnelMac, Peer: peer, MTU: 1500})
	if _, ok := g.TunnelStats(); !ok {
		t.Fatal("no splice after Attach")
	}
	mac.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := mac.Write(ipv4UDP(tunnelMac, netip.MustParseAddr("198.19.2.5"))); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	n, err := mac.Read(buf)
	if err != nil {
		t.Fatalf("no answer from the splice: %v", err)
	}
	ip := buf[4:n]
	if n < 4+20+8 || ip[9] != syscall.IPPROTO_ICMP || ip[20] != 3 || netip.AddrFrom4([4]byte(ip[12:16])) != peer {
		t.Errorf("the answer is not ICMP unreachable from the peer: % x", buf[:n])
	}
	mac.Write(ipv4UDP(tunnelMac, netip.MustParseAddr("198.19.9.5")))
	mac.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, err := mac.Read(buf); err == nil {
		t.Errorf("a packet to an unrouted address was answered: % x", buf[:n])
	}
	st, _ := g.TunnelStats()
	if st.FromMac != 2 || st.Unreachable != 1 {
		t.Errorf("the splice's count: %+v", st)
	}
	g.Detach()
	mac.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := mac.Write(ipv4UDP(tunnelMac, netip.MustParseAddr("198.19.2.5"))); err == nil {
		t.Error("the descriptor is still open after Detach")
	}
}
