package netstack

import (
	"bytes"
	"net/netip"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func TestSegmentationMakesValidPacketsInBothFamilies(t *testing.T) {
	for _, family := range []string{"IPv4", "IPv6"} {
		t.Run(family, func(t *testing.T) {
			src, dst := netip.MustParseAddr("198.19.64.5"), netip.MustParseAddr("198.19.3.1")
			hl := header.IPv4MinimumSize
			if family == "IPv6" {
				src, dst = netip.MustParseAddr("fd5b:6e2a:91c0::2"), netip.MustParseAddr("2606:4700::1111")
				hl = header.IPv6MinimumSize
			}
			data := bytes.Repeat([]byte("0123456789abcdef"), 4000)
			pkt := make([]byte, hl+header.TCPMinimumSize+len(data))
			if family == "IPv6" {
				header.IPv6(pkt).Encode(&header.IPv6Fields{PayloadLength: uint16(len(pkt) - hl), TransportProtocol: header.TCPProtocolNumber, HopLimit: 64, SrcAddr: toAddr(src), DstAddr: toAddr(dst)})
			} else {
				header.IPv4(pkt).Encode(&header.IPv4Fields{TotalLength: uint16(len(pkt)), TTL: 64, ID: 0xfff0, Protocol: uint8(header.TCPProtocolNumber), SrcAddr: toAddr(src), DstAddr: toAddr(dst)})
			}
			header.TCP(pkt[hl:]).Encode(&header.TCPFields{SrcPort: 1, DstPort: 2, SeqNum: 0xfffff000, DataOffset: header.TCPMinimumSize,
				Flags: header.TCPFlagAck | header.TCPFlagPsh | header.TCPFlagFin | header.TCPFlagCwr, WindowSize: 100})
			copy(pkt[hl+header.TCPMinimumSize:], data)
			var got []byte
			var n int
			ok := segmentTCP(pkt, 1400, 4, func(seg []byte) {
				p := seg[4:]
				l4 := p[hl:]
				var s, d = header.IPv4(p).SourceAddress(), header.IPv4(p).DestinationAddress()
				if family == "IPv6" {
					s, d = header.IPv6(p).SourceAddress(), header.IPv6(p).DestinationAddress()
					if int(header.IPv6(p).PayloadLength()) != len(l4) {
						t.Fatalf("segment %d: payload length %d", n, header.IPv6(p).PayloadLength())
					}
				} else if !header.IPv4(p).IsChecksumValid() || int(header.IPv4(p).TotalLength()) != len(p) || header.IPv4(p).ID() != 0xfff0+uint16(n) {
					t.Fatalf("segment %d: IPv4 header wrong", n)
				}
				sum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, s, d, uint16(len(l4)))
				if checksum.Checksum(l4, sum) != 0xffff {
					t.Fatalf("segment %d: TCP checksum", n)
				}
				tc := header.TCP(l4)
				if tc.SequenceNumber() != 0xfffff000+uint32(len(got)) {
					t.Fatalf("segment %d: seq %#x", n, tc.SequenceNumber())
				}
				last := len(got)+len(tc.Payload()) == len(data)
				if (tc.Flags()&header.TCPFlagFin != 0) != last || (tc.Flags()&header.TCPFlagPsh != 0) != last || (tc.Flags()&header.TCPFlagCwr != 0) != (n == 0) {
					t.Fatalf("segment %d flags %v", n, tc.Flags())
				}
				if len(tc.Payload()) > 1400 {
					t.Fatalf("segment %d carries %d", n, len(tc.Payload()))
				}
				got = append(got, tc.Payload()...)
				n++
			})
			if !ok || !bytes.Equal(got, data) || n != (len(data)+1399)/1400 {
				t.Fatalf("ok %v, %d segments, %d bytes", ok, n, len(got))
			}
		})
	}
	if segmentTCP([]byte{0x45, 0, 0, 20}, 1000, 0, func([]byte) {}) {
		t.Fatal("a truncated packet was cut")
	}
}
