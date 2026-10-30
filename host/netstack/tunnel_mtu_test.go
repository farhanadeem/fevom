package netstack

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

func tunnelOn(t *testing.T, mtu int, accepts uint32) (*Tunnel, *vmmEnd, *net.UnixConn, *Stack) {
	t.Helper()
	hostEnd, guestEnd := socketPair(t)
	if accepts != 0 {
		announce(t, guestEnd, accepts)
	}
	ns, err := New(hostEnd, Options{Host: newFakeHost()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ns.Close() })
	fd, mac := standInUtun(t)
	tun, err := newTunnel(fd, TunnelConfig{Mac: tunnelMac, Peer: tunnelPeer, MTU: mtu}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tun.Close() })
	tun.SetPrefixes(plan.Routed())
	tun.Attach(ns)
	waitLinkReady(t, ns)
	if accepts != 0 {
		deadline := time.Now().Add(5 * time.Second)
		for !ns.link.offload.Load() {
			if time.Now().After(deadline) {
				t.Fatal("the stack never heard what the guest accepts")
			}
			time.Sleep(time.Millisecond)
		}
	}
	return tun, guestEnd, mac, ns
}

func macTCP(dst netip.AddrPort, seq uint32, flags header.TCPFlags, opts, data []byte) []byte {
	thl := header.TCPMinimumSize + len(opts)
	pkt := make([]byte, header.IPv4MinimumSize+thl+len(data))
	ip := header.IPv4(pkt)
	ip.Encode(&header.IPv4Fields{TotalLength: uint16(len(pkt)), TTL: 64, ID: 7, Flags: header.IPv4FlagDontFragment,
		Protocol: uint8(header.TCPProtocolNumber), SrcAddr: toAddr(tunnelMac), DstAddr: toAddr(dst.Addr())})
	ip.SetChecksum(^ip.CalculateChecksum())
	tc := header.TCP(ip.Payload())
	tc.Encode(&header.TCPFields{SrcPort: 51000, DstPort: dst.Port(), SeqNum: seq, AckNum: 1, DataOffset: uint8(thl), Flags: flags, WindowSize: 65535})
	copy(tc[header.TCPMinimumSize:], opts)
	copy(tc[thl:], data)
	sum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(tc)))
	tc.SetChecksum(^checksum.Checksum(tc, sum))
	return pkt
}

var timestamps = []byte{1, 1, 8, 10, 0, 0, 0, 1, 0, 0, 0, 2}

func TestTheReaderWakesTheVMMOnceForWhatItFindsQueued(t *testing.T) {
	tun, guestEnd, mac, _ := tunnelOn(t, 1500, 0)
	wakes := make(chan int, 1)
	frames := 0
	const n = 20
	for i := 0; i < n; i++ {
		macWrite(t, mac, macTCP(netip.AddrPortFrom(containerAddr, 8080), uint32(i*1000), header.TCPFlagAck, timestamps, make([]byte, 1000)))
	}
	if !guestEnd.fromStack.sleepIfEmpty() {
		t.Fatal("the ring to the guest is not empty")
	}
	go func() {
		w := 0
		deadline := time.Now().Add(3 * time.Second)
		for frames < n && guestEnd.wait(deadline) {
			w++
			for {
				m, ok, err := guestEnd.fromStack.peek()
				if err != nil || !ok {
					break
				}
				guestEnd.fromStack.advance(m)
				frames++
			}
			guestEnd.fromStack.sleepIfEmpty()
		}
		wakes <- w
	}()
	go tun.read()
	w := <-wakes
	if frames != n || w != 1 {
		t.Fatalf("%d packets queued in the utun: %d frames reached the guest after %d wake-ups of the VMM, want %d after 1 (%v)", n, frames, w, n, tun.Stats())
	}
}

func TestASynToTheMacIsToldTheUtunsMSS(t *testing.T) {
	for _, c := range []struct {
		name    string
		mtu     int
		partial bool
		mss     uint16
		want    uint16
	}{
		{"MTU 1500: unchanged", 1500, false, 1460, 1460},
		{"MTU 16384, its checksum whole", 16384, false, 1460, 16344},
		{"MTU 65535, its checksum left to the device", 65535, true, 1460, 65495},
		{"MTU 65535, a container's lower MSS kept", 65535, false, 1360, 1360},
		{"MTU 16384, a container's lower MSS kept, its checksum left to the device", 16384, true, 536, 536},
	} {
		t.Run(c.name, func(t *testing.T) {
			hostEnd, guestEnd := socketPair(t)
			ns, err := New(hostEnd, Options{Host: newFakeHost()})
			if err != nil {
				t.Fatal(err)
			}
			defer ns.Close()
			fd, mac := standInUtun(t)
			tun, err := NewTunnel(fd, TunnelConfig{Mac: tunnelMac, Peer: tunnelPeer, MTU: c.mtu}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tun.Close()
			tun.SetPrefixes(plan.Routed())
			tun.Attach(ns)
			opts := []byte{2, 4, byte(c.mss >> 8), byte(c.mss), 4, 2, 8, 10, 0, 0, 0, 9, 0, 0, 0, 1, 1, 3, 3, 7}
			src := netip.AddrPortFrom(containerAddr, 8080)
			frame := tcpFrameOpts(src, netip.AddrPortFrom(tunnelMac, 51000), header.TCPFlagSyn|header.TCPFlagAck, opts)
			var vhdr [netHdrLen]byte
			if c.partial {
				ip := header.IPv4(frame[header.EthernetMinimumSize:])
				tc := header.TCP(ip.Payload())
				tc.SetChecksum(header.PseudoHeaderChecksum(header.TCPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(tc))))
				vhdr[0] = hdrNeedsCsum
				binary.LittleEndian.PutUint16(vhdr[6:], uint16(header.EthernetMinimumSize+header.IPv4MinimumSize))
				binary.LittleEndian.PutUint16(vhdr[8:], 16)
			}
			guestEnd.send(append(vhdr[:], frame...))
			pkt, ok := macRead(t, mac, 5*time.Second)
			if !ok || !validTCP(pkt) {
				t.Fatalf("the SYN-ACK: arrived %v, checksums valid %v (%v)", ok, ok && validTCP(pkt), tun.Stats())
			}
			got := header.TCP(header.IPv4(pkt).Payload()).ParsedOptions()
			if mss := mssOf(header.TCP(header.IPv4(pkt).Payload())); mss != c.want {
				t.Fatalf("MSS %d reached the Mac, want %d (options %+v)", mss, c.want, got)
			}
		})
	}
}

func mssOf(tc header.TCP) uint16 {
	opts := tc[header.TCPMinimumSize:tc.DataOffset()]
	for i := 0; i+1 < len(opts); {
		switch opts[i] {
		case 0:
			return 0
		case 1:
			i++
			continue
		}
		if opts[i] == 2 && opts[i+1] == 4 && i+4 <= len(opts) {
			return binary.BigEndian.Uint16(opts[i+2:])
		}
		if opts[i+1] < 2 {
			return 0
		}
		i += int(opts[i+1])
	}
	return 0
}

func tcpFrameOpts(src, dst netip.AddrPort, flags header.TCPFlags, opts []byte) []byte {
	thl := header.TCPMinimumSize + len(opts)
	frame := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+thl)
	header.Ethernet(frame).Encode(&header.EthernetFields{SrcAddr: guestMAC, DstAddr: GatewayMAC, Type: ipv4.ProtocolNumber})
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{TotalLength: uint16(header.IPv4MinimumSize + thl), TTL: 64, ID: 0x100,
		Protocol: uint8(header.TCPProtocolNumber), SrcAddr: toAddr(src.Addr()), DstAddr: toAddr(dst.Addr())})
	ip.SetChecksum(^ip.CalculateChecksum())
	tc := header.TCP(ip.Payload())
	tc.Encode(&header.TCPFields{SrcPort: src.Port(), DstPort: dst.Port(), SeqNum: 77, AckNum: 1, DataOffset: uint8(thl), Flags: flags, WindowSize: 65535})
	copy(tc[header.TCPMinimumSize:], opts)
	sum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(tc)))
	tc.SetChecksum(^checksum.Checksum(tc, sum))
	return frame
}

func TestALargeSegmentFromTheMacGoesToTheGuestAsOne(t *testing.T) {
	data := make([]byte, 16000)
	for i := range data {
		data[i] = byte(i * 13)
	}
	dst := netip.AddrPortFrom(containerAddr, 8080)
	seg := macTCP(dst, 9000, header.TCPFlagAck|header.TCPFlagPsh, timestamps, data)

	t.Run("a guest that takes large segments: one, cut at its MTU by its side", func(t *testing.T) {
		tun, guestEnd, mac, _ := tunnelOn(t, 16384, acceptsPartialCsum|acceptsTSO4)
		go tun.read()
		macWrite(t, mac, seg)
		msg, ok := guestEnd.recv(time.Now().Add(5 * time.Second))
		if !ok {
			t.Fatalf("nothing reached the guest (%v)", tun.Stats())
		}
		hdr, frame := msg[:netHdrLen], msg[netHdrLen:]
		pkt := frame[header.EthernetMinimumSize:]
		if len(pkt) != len(seg) || !bytes.Equal(header.TCP(header.IPv4(pkt).Payload()).Payload(), data) {
			t.Fatalf("the guest got %d bytes, not the segment of %d whole", len(pkt), len(seg))
		}
		ihl, thl := header.IPv4MinimumSize, header.TCPMinimumSize+len(timestamps)
		if hdr[0] != hdrNeedsCsum || hdr[1] != hdrGSOTCPv4 || binary.LittleEndian.Uint16(hdr[4:]) != uint16(ipMTU-ihl-thl) ||
			binary.LittleEndian.Uint16(hdr[2:]) != uint16(header.EthernetMinimumSize+ihl+thl) ||
			binary.LittleEndian.Uint16(hdr[6:]) != uint16(header.EthernetMinimumSize+ihl) || binary.LittleEndian.Uint16(hdr[8:]) != 16 {
			t.Fatalf("virtio header % x: want a TCPv4 segment to cut at %d, its checksum from byte %d", hdr, ipMTU-ihl-thl, header.EthernetMinimumSize+ihl)
		}
		whole := append([]byte(nil), pkt...)
		completeChecksum(whole, ihl, 16)
		if !validTCP(whole) {
			t.Fatal("the segment's checksum, finished, is not valid")
		}
		segmentTCP(append([]byte(nil), pkt...), ipMTU-ihl-thl, 0, func(p []byte) {
			if len(p) > ipMTU || !validTCP(p) {
				t.Fatalf("cut at the guest's MTU: %d bytes, valid %v", len(p), validTCP(p))
			}
		})
		if s := tun.Stats(); s.ToGuest != 1 || s.Malformed != 0 {
			t.Fatalf("stats %v", s)
		}
	})

	t.Run("a guest that takes no large segment: cut here, checksums whole", func(t *testing.T) {
		tun, guestEnd, mac, _ := tunnelOn(t, 16384, 0)
		go tun.read()
		macWrite(t, mac, seg)
		var got []byte
		for len(got) < len(data) {
			msg, ok := guestEnd.recv(time.Now().Add(5 * time.Second))
			if !ok {
				t.Fatalf("%d of %d bytes reached the guest (%v)", len(got), len(data), tun.Stats())
			}
			if !bytes.Equal(msg[:netHdrLen], make([]byte, netHdrLen)) {
				t.Fatalf("virtio header % x for a guest that takes no offload", msg[:netHdrLen])
			}
			pkt := msg[netHdrLen+header.EthernetMinimumSize:]
			if len(pkt) > ipMTU || !validTCP(pkt) {
				t.Fatalf("a segment of %d bytes, checksums valid %v", len(pkt), validTCP(pkt))
			}
			got = append(got, header.TCP(header.IPv4(pkt).Payload()).Payload()...)
		}
		if !bytes.Equal(got, data) {
			t.Fatal("the segments do not carry the data")
		}
	})

	t.Run("not TCP: whole, for the guest's IP; dropped for a guest of 1500-byte buffers", func(t *testing.T) {
		big := echoRequest(tunnelMac, containerAddr, 9, 1)
		big = append(big, make([]byte, 8000)...)
		header.IPv4(big).SetTotalLength(uint16(len(big)))
		ip := header.IPv4(big)
		ip.SetChecksum(0)
		ip.SetChecksum(^ip.CalculateChecksum())
		tun, guestEnd, mac, _ := tunnelOn(t, 16384, acceptsPartialCsum|acceptsTSO4)
		go tun.read()
		macWrite(t, mac, big)
		msg, ok := guestEnd.recv(time.Now().Add(5 * time.Second))
		if !ok || len(msg) != netHdrLen+header.EthernetMinimumSize+len(big) {
			t.Fatalf("the guest got %d bytes (%v)", len(msg), tun.Stats())
		}
		tun2, _, mac2, _ := tunnelOn(t, 16384, 0)
		go tun2.read()
		macWrite(t, mac2, big)
		deadline := time.Now().Add(5 * time.Second)
		for s := tun2.Stats(); s.Malformed+s.ToGuest == 0 && time.Now().Before(deadline); s = tun2.Stats() {
			time.Sleep(time.Millisecond)
		}
		if s := tun2.Stats(); s.Malformed != 1 || s.ToGuest != 0 {
			t.Fatalf("stats %v", s)
		}
	})
}

func TestAGuestSegmentTheUtunTakesIsWrittenWhole(t *testing.T) {
	data := make([]byte, 65000)
	for i := range data {
		data[i] = byte(i)
	}
	src := netip.AddrPortFrom(netip.MustParseAddr("198.19.64.5"), 80)
	dst := netip.AddrPortFrom(tunnelMac, 51000)
	for _, c := range []struct {
		mtu, writes int
	}{
		{65535, 1},
		{16384, (len(data) + 16343) / 16344},
	} {
		t.Run("MTU "+strconv.Itoa(c.mtu), func(t *testing.T) {
			tun, guestEnd, mac, _ := tunnelOn(t, c.mtu, 0)
			go tun.read()
			frame := tcpFrame(src, dst, 5000, header.TCPFlagAck|header.TCPFlagPsh, data)
			ip := header.IPv4(frame[header.EthernetMinimumSize:])
			tc := header.TCP(ip.Payload())
			tc.SetChecksum(header.PseudoHeaderChecksum(header.TCPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(tc))))
			vhdr := [netHdrLen]byte{hdrNeedsCsum, hdrGSOTCPv4}
			binary.LittleEndian.PutUint16(vhdr[4:], 1448)
			binary.LittleEndian.PutUint16(vhdr[6:], uint16(header.EthernetMinimumSize+header.IPv4MinimumSize))
			binary.LittleEndian.PutUint16(vhdr[8:], 16)
			guestEnd.send(append(vhdr[:], frame...))
			var got []byte
			for i := 0; i < c.writes; i++ {
				pkt, ok := macRead(t, mac, 5*time.Second)
				if !ok {
					t.Fatalf("write %d of %d missing (%v)", i, c.writes, tun.Stats())
				}
				if len(pkt) > c.mtu || !validTCP(pkt) {
					t.Fatalf("write %d: %d bytes, checksums valid %v", i, len(pkt), validTCP(pkt))
				}
				got = append(got, header.TCP(header.IPv4(pkt).Payload()).Payload()...)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("%d bytes in %d writes, not the %d sent", len(got), c.writes, len(data))
			}
			if _, more := macRead(t, mac, 200*time.Millisecond); more || tun.Stats().ToMac != uint64(c.writes) {
				t.Fatalf("more than %d writes (%v)", c.writes, tun.Stats())
			}
		})
	}
}

func TestALargeMTUTunnelCarriesTheMacsTCPInLargeSegments(t *testing.T) {
	const mtu = 16384
	hostEnd, guestEnd := socketPair(t)
	announce(t, guestEnd, acceptsPartialCsum|acceptsTSO4)
	ns, err := New(hostEnd, Options{Host: newFakeHost()})
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	g := newGuestStack(t, guestEnd, true)
	defer g.Close()
	addr := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: toAddr(containerAddr), PrefixLen: 24}}
	if err := g.AddProtocolAddress(1, addr, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	fd, macEnd := standInUtun(t)
	tun, err := NewTunnel(fd, TunnelConfig{Mac: tunnelMac, Peer: tunnelPeer, MTU: mtu}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	tun.SetPrefixes(plan.Routed())
	tun.Attach(ns)
	waitLinkReady(t, ns)
	mac := macStackMTU(t, macEnd, mtu)

	l, err := gonet.ListenTCP(g, tcpip.FullAddress{NIC: 1, Addr: toAddr(containerAddr), Port: 8080}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			got <- nil
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		got <- b
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := gonet.DialContextTCP(ctx, mac, tcpip.FullAddress{NIC: 1, Addr: toAddr(containerAddr), Port: 8080}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("dial from the Mac: %v (%v)", err, tun.Stats())
	}
	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte(i*7 + i>>11)
	}
	c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	c.Close()
	b := <-got
	if !bytes.Equal(b, payload) {
		t.Fatalf("the guest read %d bytes of %d (%v)", len(b), len(payload), tun.Stats())
	}
	s := tun.Stats()
	if s.FromMac > 600 || s.Malformed+s.GuestBusy != 0 {
		t.Fatalf("%d packets from the Mac for %d bytes: the Mac's TCP did not send segments of the utun's MTU (%v)", s.FromMac, len(payload), s)
	}
}

func macStackMTU(t *testing.T, c *net.UnixConn, mtu uint32) *stack.Stack {
	t.Helper()
	s := macStackOf(t, c, mtu)
	snd := tcpip.TCPSendBufferSizeRangeOption{Min: 4 << 10, Default: 4 << 20, Max: 8 << 20}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &snd)
	return s
}

func TestATunnelNarrowerThanTheGuestsMTUIsRefused(t *testing.T) {
	for _, c := range []struct {
		mtu int
		ok  bool
	}{{1280, false}, {1499, false}, {1500, true}, {16384, true}, {65535, true}, {65536, false}} {
		fd, _ := standInUtun(t)
		tun, err := NewTunnel(fd, TunnelConfig{Mac: tunnelMac, Peer: tunnelPeer, MTU: c.mtu}, nil)
		if err == nil {
			tun.Close()
		}
		if (err == nil) != c.ok {
			t.Errorf("a tunnel of MTU %d: err %v, want taken %v", c.mtu, err, c.ok)
		}
	}
}
