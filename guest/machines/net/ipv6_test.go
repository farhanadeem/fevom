package machnet

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"testing"

	"fev/guest/internal/fw"
	"fev/proto/netplan"
)

var prefix6 = netip.MustParsePrefix("fd5b:6e2a:91c0:1::/64")

func TestTheMachinesIPv6IsThePlans(t *testing.T) {
	p6, err := netplan.Parse6("fd5b:6e2a:91c0::/48")
	if err != nil {
		t.Fatal(err)
	}
	if got := p6.Subnet(netplan.Subnet6Machines); got != prefix6 {
		t.Errorf("the machines' /64 is %s", got)
	}
	if Gateway6(prefix6) != p6.MachinesGateway6() {
		t.Errorf("the bridge's address %s, the plan's %s", Gateway6(prefix6), p6.MachinesGateway6())
	}
}

func TestTheRouterAdvertisement(t *testing.T) {
	b := routerAdvertisement(bridgeMAC, prefix6)
	if len(b) != 16+8+32+8 {
		t.Fatalf("%d bytes", len(b))
	}
	if b[0] != 134 || b[1] != 0 || b[4] != 64 || b[5] != 0 {
		t.Errorf("header % x", b[:8])
	}
	if lt := binary.BigEndian.Uint16(b[6:8]); lt != 65535 {
		t.Errorf("router lifetime %d", lt)
	}
	if binary.BigEndian.Uint32(b[8:12]) != 0 || binary.BigEndian.Uint32(b[12:16]) != 0 {
		t.Error("reachable time or retransmission timer set")
	}
	if o := b[16:24]; o[0] != 1 || o[1] != 1 || string(o[2:8]) != string(bridgeMAC) {
		t.Errorf("source link-layer address % x", o)
	}
	pi := b[24:56]
	if pi[0] != 3 || pi[1] != 4 || pi[2] != 64 || pi[3] != 0xc0 {
		t.Errorf("prefix information % x", pi[:4])
	}
	if binary.BigEndian.Uint32(pi[4:8]) != 0xffffffff || binary.BigEndian.Uint32(pi[8:12]) != 0xffffffff {
		t.Error("the prefix's lifetimes are not infinite")
	}
	if netip.AddrFrom16([16]byte(pi[16:32])) != prefix6.Addr() {
		t.Errorf("prefix %x", pi[16:32])
	}
	if ri := b[56:]; ri[0] != 24 || ri[1] != 1 || ri[2] != 0 || ri[3] != 0 || binary.BigEndian.Uint32(ri[4:8]) != 0xffffffff {
		t.Errorf("route information % x", ri)
	}
	if !isRouterSolicitation([]byte{133, 0, 0, 0, 0, 0, 0, 0}) || isRouterSolicitation([]byte{133, 1, 0, 0, 0, 0, 0, 0}) ||
		isRouterSolicitation([]byte{133, 0, 0, 0}) || isRouterSolicitation([]byte{135, 0, 0, 0, 0, 0, 0, 0}) {
		t.Error("solicitations told apart wrongly")
	}
}

func addrMsg(typ uint16, scope byte, a netip.Addr, flags uint32) []byte {
	return addrMsgOn(typ, scope, a, flags, 2)
}

func addrMsgOn(typ uint16, scope byte, a netip.Addr, flags uint32, link uint32) []byte {
	le := binary.LittleEndian
	attr := func(t uint16, v []byte) []byte {
		b := make([]byte, 4, 4+len(v)+3)
		le.PutUint16(b[0:2], uint16(4+len(v)))
		le.PutUint16(b[2:4], t)
		b = append(b, v...)
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
		return b
	}
	a16 := a.As16()
	var f [4]byte
	le.PutUint32(f[:], flags)
	body := []byte{afInet6, 64, byte(flags), scope, 0, 0, 0, 0}
	le.PutUint32(body[4:8], link)
	body = append(body, attr(ifaAddress, a16[:])...)
	body = append(body, attr(ifaFlags, f[:])...)
	h := make([]byte, nlmsgHdrLen)
	le.PutUint32(h[0:4], uint32(nlmsgHdrLen+len(body)))
	le.PutUint16(h[4:6], typ)
	return append(h, body...)
}

func TestAMachinesIPv6AddressFromItsNamespacesMessages(t *testing.T) {
	stable := netip.MustParseAddr("fd5b:6e2a:91c0:1:216:3eff:fe1a:2b3c")
	temp := netip.MustParseAddr("fd5b:6e2a:91c0:1:1111:2222:3333:4444")
	low := netip.MustParseAddr("fd5b:6e2a:91c0:1::5")
	var dgram []byte
	dgram = append(dgram, addrMsg(rtmNewAddr, 0, stable, ifaTentative)...)
	dgram = append(dgram, addrMsg(rtmNewAddr, 253, netip.MustParseAddr("fe80::216:3eff:fe1a:2b3c"), 0)...)
	have := addrs6{}
	apply := func(b []byte) {
		for _, e := range addrEvents(b) {
			if !prefix6.Contains(e.addr) {
				continue
			}
			if e.gone {
				delete(have, e.addr)
			} else {
				have[e.addr] = e.flags
			}
		}
	}
	apply(dgram)
	if len(have) != 1 || have.best().IsValid() {
		t.Fatalf("a tentative address is the name's: %v %s", have, have.best())
	}
	apply(addrMsg(rtmNewAddr, 0, stable, 0x80))
	apply(addrMsg(rtmNewAddr, 0, temp, ifaTemporary))
	if have.best() != stable {
		t.Errorf("best %s, want the stable %s", have.best(), stable)
	}
	apply(addrMsg(rtmNewAddr, 0, low, 0))
	if have.best() != low {
		t.Errorf("best %s, want the lowest stable %s", have.best(), low)
	}
	apply(addrMsg(rtmNewAddr, 0, low, ifaDeprecated))
	apply(addrMsg(rtmDelAddr, 0, stable, 0))
	if have.best() != temp {
		t.Errorf("best %s, want the temporary one left %s", have.best(), temp)
	}
	apply(addrMsg(rtmDelAddr, 0, temp, ifaTemporary))
	if have.best().IsValid() {
		t.Errorf("best %s with only a deprecated address left", have.best())
	}
	if e := addrEvents([]byte{1, 2, 3}); len(e) != 0 {
		t.Errorf("events from garbage: %v", e)
	}
	done := make([]byte, nlmsgHdrLen+4)
	binary.LittleEndian.PutUint32(done[0:4], uint32(len(done)))
	binary.LittleEndian.PutUint16(done[4:6], nlmsgDone)
	if e := addrEvents(append(done, addrMsg(rtmNewAddr, 0, low, 0)...)); len(e) != 1 || e[0].addr != low {
		t.Errorf("events after a done message: %v", e)
	}
}

func TestTheIPv6RulesMasqueradeTheMachinesSlash64(t *testing.T) {
	s := restoreScript(fw.Tables{}, BridgeName, prefix6, "eth0")
	for _, want := range []string{
		"-A FEV-MACHINES -i fev0 -j DOCKER-INTERNAL\n",
		"-A FEV-MACHINES -i fev0 -j ACCEPT\n",
		"-A FEV-MACHINES -s fd5b:6e2a:91c0:1::/64 -o eth0 -j MASQUERADE\n",
		"-A POSTROUTING -j FEV-MACHINES\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q in:\n%s", want, s)
		}
	}
}

func TestASolicitationsLinkLayerAddress(t *testing.T) {
	rs := []byte{133, 0, 0, 0, 0, 0, 0, 0}
	if solicitationMAC(rs) != nil {
		t.Error("an address from a solicitation with no option")
	}
	nonce := []byte{14, 1, 1, 2, 3, 4, 5, 6}
	slla := []byte{1, 1, 0x02, 0x11, 0x22, 0x33, 0x44, 0x55}
	got := solicitationMAC(append(append(append([]byte{}, rs...), nonce...), slla...))
	if got.String() != "02:11:22:33:44:55" {
		t.Errorf("got %v", got)
	}
	if solicitationMAC(append(append([]byte{}, rs...), 1, 0, 0, 0, 0, 0, 0, 0)) != nil {
		t.Error("a zero-length option was read")
	}
	if solicitationMAC(append(append([]byte{}, rs...), 1, 2, 0, 0)) != nil {
		t.Error("an option past the end was read")
	}
}

func TestAMachinesLinkLocalAddressIsItsAddressLinks(t *testing.T) {
	a := newMachineAddrs(prefix6)
	global := netip.MustParseAddr("fd5b:6e2a:91c0:1:216:3eff:fe1a:2b3c")
	eth0 := netip.MustParseAddr("fe80::216:3eff:fe1a:2b3c")
	docker0 := netip.MustParseAddr("fe80::1")
	var d []byte
	d = append(d, addrMsgOn(rtmNewAddr, scopeLink, docker0, 0x80, 5)...)
	d = append(d, addrMsgOn(rtmNewAddr, scopeLink, eth0, ifaTentative, 2)...)
	a.apply(addrEvents(d))
	if b, ll := a.best(); b.IsValid() || ll.IsValid() {
		t.Errorf("before a global address: %s %s", b, ll)
	}
	a.apply(addrEvents(addrMsgOn(rtmNewAddr, 0, global, 0, 2)))
	if b, ll := a.best(); b != global || ll.IsValid() {
		t.Errorf("with eth0's link-local still tentative: %s %s", b, ll)
	}
	a.apply(addrEvents(addrMsgOn(rtmNewAddr, scopeLink, eth0, 0x80, 2)))
	if b, ll := a.best(); b != global || ll != eth0 {
		t.Errorf("got %s %s, want %s %s", b, ll, global, eth0)
	}
	a.apply(addrEvents(addrMsgOn(rtmDelAddr, 0, global, 0, 2)))
	if b, ll := a.best(); b.IsValid() || ll.IsValid() {
		t.Errorf("after the global address went: %s %s", b, ll)
	}
}
