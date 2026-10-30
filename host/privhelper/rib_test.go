package privhelper

import (
	"bufio"
	"encoding/binary"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func msg(index uint16, flags, addrs int32, sas ...[]byte) []byte {
	b := make([]byte, rtMsghdrSize)
	b[2] = rtmVersion
	b[3] = 4
	binary.NativeEndian.PutUint16(b[4:], index)
	binary.NativeEndian.PutUint32(b[8:], uint32(flags))
	binary.NativeEndian.PutUint32(b[12:], uint32(addrs))
	for _, sa := range sas {
		b = append(b, sa...)
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
		if len(sa) == 0 {
			b = append(b, 0, 0, 0, 0)
		}
	}
	binary.NativeEndian.PutUint16(b[0:], uint16(len(b)))
	return b
}

func sin(a string) []byte {
	ip := netip.MustParseAddr(a).As4()
	return append([]byte{16, afInet, 0, 0}, append(ip[:], make([]byte, 8)...)...)
}

func mask4(bits int) []byte {
	m := net.CIDRMask(bits, 32)
	n := 4
	for n > 0 && m[n-1] == 0 {
		n--
	}
	if n == 0 {
		return []byte{0}[:0]
	}
	return append([]byte{byte(4 + n), 0xff, 0, 0}, m[:n]...)
}

func sdl(index uint16) []byte {
	b := make([]byte, 20)
	b[0], b[1] = 20, 18
	binary.NativeEndian.PutUint16(b[2:], index)
	return b
}

func TestARoutingTableIsReadAsTheKernelWritesIt(t *testing.T) {
	ll := netip.MustParseAddr("fe80::").As16()
	ll[3] = 7
	llsa := append([]byte{28, afInet6, 0, 0, 0, 0, 0, 0}, ll[:]...)
	llsa = append(llsa, 0, 0, 0, 0)
	b := append(msg(4, RTFUp|RTFStatic, rtaDst|rtaGateway|rtaNetmask, sin("198.19.64.0"), sdl(4), mask4(18)),
		msg(4, RTFUp|RTFGateway|RTFStatic, rtaDst|rtaGateway|rtaNetmask, sin("0.0.0.0"), sin("192.168.1.1"), nil)...)
	b = append(b, msg(1, RTFUp|RTFHost, rtaDst|rtaGateway, sin("127.0.0.1"), sin("127.0.0.1"))...)
	b = append(b, msg(9, RTFUp, rtaDst|rtaNetmask, llsa, append([]byte{12, 0, 0, 0, 0, 0, 0, 0}, 0xff, 0xff, 0xff, 0xff))...)
	b = append(b, msg(3, RTFUp, rtaDst, sdl(3))...)
	routes, err := ParseRIB(b)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range routes {
		got = append(got, r.Prefix.String()+"@"+strconv.Itoa(r.Index))
	}
	if want := "198.19.64.0/18@4 0.0.0.0/0@4 127.0.0.1/32@1 fe80::/32@9"; strings.Join(got, " ") != want {
		t.Errorf("read %v, want %s", got, want)
	}
	var gws []string
	for _, r := range routes {
		gws = append(gws, "<"+r.Gateway+">")
	}
	if want := "<link#4> <192.168.1.1> <127.0.0.1> <>"; strings.Join(gws, " ") != want {
		t.Errorf("gateways %v, want %s", gws, want)
	}
	for _, bad := range [][]byte{b[:len(b)-3], append(b, 1, 2), func() []byte { c := append([]byte{}, b...); c[2] = 4; return c }()} {
		if _, err := ParseRIB(bad); err == nil {
			t.Errorf("a broken table of %d bytes was read", len(bad))
		}
	}
}

func netstatRoutes(t *testing.T) map[string]bool {
	out, err := exec.Command("/usr/sbin/netstat", "-rn", "-f", "inet").Output()
	if err != nil {
		t.Skip("netstat:", err)
	}
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || f[0] == "Destination" || f[0] == "Routing" || f[0] == "Internet:" {
			continue
		}
		dst, gw, ifname := f[0], f[1], f[3]
		var p netip.Prefix
		if dst == "default" {
			p = netip.MustParsePrefix("0.0.0.0/0")
		} else {
			addr, bitsText, cut := strings.Cut(dst, "/")
			addr, _, _ = strings.Cut(addr, "%")
			octets := strings.Split(addr, ".")
			bits := 8 * len(octets)
			if cut {
				bits, _ = strconv.Atoi(bitsText)
			}
			for len(octets) < 4 {
				octets = append(octets, "0")
			}
			a, err := netip.ParseAddr(strings.Join(octets, "."))
			if err != nil {
				continue
			}
			if p, err = a.Prefix(bits); err != nil {
				continue
			}
		}
		seen[p.String()+"@"+ifname] = true
		if _, err := netip.ParseAddr(gw); err == nil || strings.HasPrefix(gw, "link#") {
			seen[p.String()+"@"+ifname+" via "+gw] = true
		}
	}
	return seen
}

func TestTheMacsTableHoldsWhatNetstatShows(t *testing.T) {
	routes, err := Darwin{}.Routes()
	if err != nil {
		t.Fatal(err)
	}
	ifs, err := Darwin{}.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, r := range routes {
		if r.Prefix.Addr().Is4() {
			have[r.Prefix.String()+"@"+nameOf(ifs, r.Index)] = true
			have[r.Prefix.String()+"@"+nameOf(ifs, r.Index)+" via "+r.Gateway] = true
		}
	}
	want := netstatRoutes(t)
	if len(want) == 0 {
		t.Skip("netstat showed nothing")
	}
	missing := 0
	for k := range want {
		if !have[k] {
			missing++
			t.Errorf("netstat shows %s; the table read lacks it", k)
		}
	}
	t.Logf("%d IPv4 routes read, %d distinct in netstat, %d missing", len(have), len(want), missing)
	if !have["127.0.0.0/8@lo0"] {
		t.Error("no 127.0.0.0/8 on lo0")
	}
}
