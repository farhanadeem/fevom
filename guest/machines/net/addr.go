package machnet

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"fev/proto/netplan"
)

var Prefix = netplan.Default().Machines()

const BridgeName = "fev0"

var bridgeMAC = net.HardwareAddr{0x02, 0x66, 0x65, 0x76, 0x6f, 0x6d}

func Gateway(prefix netip.Prefix) netip.Addr { return prefix.Masked().Addr().Next() }

func MACFor(id string) net.HardwareAddr {
	sum := sha256.Sum256([]byte("fev machine " + id))
	mac := net.HardwareAddr(sum[:6])
	mac[0] = mac[0]&^1 | 2
	return mac
}

var errFull = errors.New("every address of the machines' subnet is taken")

func allocate(prefix netip.Prefix, taken map[netip.Addr]string, name, id, want string, last netip.Addr) (netip.Addr, error) {
	free := func(a netip.Addr) bool {
		owner, held := taken[a]
		return !held || owner == name
	}
	usable := func(a netip.Addr) bool {
		return prefix.Contains(a) && a != prefix.Masked().Addr() && a != Gateway(prefix) && a != lastAddr(prefix)
	}
	if want != "" {
		a, err := netip.ParseAddr(want)
		if err != nil || !a.Is4() {
			return netip.Addr{}, fmt.Errorf("the machine's address %q is not an IPv4 address", want)
		}
		if !usable(a) {
			return netip.Addr{}, fmt.Errorf("the machine's address %s is not one of %s's for machines", a, prefix)
		}
		if !free(a) {
			return netip.Addr{}, fmt.Errorf("the machine's address %s is %s's", a, taken[a])
		}
		return a, nil
	}
	if last.IsValid() && usable(last) && free(last) {
		return last, nil
	}
	first := prefix.Masked().Addr().As4()
	base := binary.BigEndian.Uint32(first[:])
	hosts := uint32(1)<<(32-prefix.Bits()) - 3
	sum := sha256.Sum256([]byte("fev address " + id))
	start := binary.BigEndian.Uint32(sum[:4]) % hosts
	for i := uint32(0); i < hosts; i++ {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], base+2+(start+i)%hosts)
		if a := netip.AddrFrom4(b); free(a) {
			return a, nil
		}
	}
	return netip.Addr{}, errFull
}

func lastAddr(prefix netip.Prefix) netip.Addr {
	first := prefix.Masked().Addr().As4()
	v := binary.BigEndian.Uint32(first[:]) | (uint32(1)<<(32-prefix.Bits()) - 1)
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}
