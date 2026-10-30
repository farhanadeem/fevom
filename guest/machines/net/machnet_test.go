package machnet

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"fev/proto/relay"
)

var mac = net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}

func clientMessage(typ byte, ciaddr netip.Addr, opts ...[]byte) []byte {
	b := make([]byte, headerLen)
	b[0], b[1], b[2] = 1, 1, 6
	copy(b[4:8], []byte{1, 2, 3, 4})
	binary.BigEndian.PutUint16(b[10:12], 0x8000)
	if ciaddr.IsValid() {
		a := ciaddr.As4()
		copy(b[12:16], a[:])
	}
	copy(b[28:34], mac)
	copy(b[236:240], dhcpMagic[:])
	b = append(b, optType, 1, typ)
	for _, o := range opts {
		b = append(b, o...)
	}
	return append(b, optEnd)
}

func option(code byte, v netip.Addr) []byte {
	a := v.As4()
	return append([]byte{code, 4}, a[:]...)
}

func replyOptions(t *testing.T, b []byte) map[byte][]byte {
	t.Helper()
	if len(b) < minReply || b[0] != 2 || [4]byte(b[236:240]) != dhcpMagic {
		t.Fatalf("not a reply: %x", b)
	}
	opts := map[byte][]byte{}
	for o := b[headerLen:]; len(o) > 0 && o[0] != optEnd; {
		if o[0] == optPad {
			o = o[1:]
			continue
		}
		opts[o[0]] = o[2 : 2+int(o[1])]
		o = o[2+int(o[1]):]
	}
	return opts
}

var testLAN = lan{server: Gateway(Prefix), prefix: Prefix, lease: infiniteLease}

func TestDHCPLeasesEachMachineItsOwnAddressAndNothingElse(t *testing.T) {
	addr := netip.MustParseAddr("198.19.0.42")
	gw := Gateway(Prefix)
	if gw.String() != "198.19.0.1" {
		t.Fatalf("gateway %s", gw)
	}

	req, err := parseRequest(clientMessage(dhcpDiscover, netip.Addr{}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(req.chaddr, mac) {
		t.Fatalf("chaddr %s", req.chaddr)
	}
	offer, acked := testLAN.answer(req, addr)
	if acked {
		t.Fatal("an offer is not an acknowledgement")
	}
	o := replyOptions(t, offer)
	if o[optType][0] != dhcpOffer || netip.AddrFrom4([4]byte(offer[16:20])) != addr || !bytes.Equal(offer[4:8], []byte{1, 2, 3, 4}) || !bytes.Equal(offer[28:34], mac) {
		t.Fatalf("offer: %x", offer)
	}
	for code, want := range map[byte][]byte{
		optServerID: gw.AsSlice(), optRouter: gw.AsSlice(), optDNS: gw.AsSlice(),
		optSubnetMask: {255, 255, 255, 0}, optLeaseTime: {0xff, 0xff, 0xff, 0xff},
	} {
		if !bytes.Equal(o[code], want) {
			t.Errorf("option %d: %x, want %x", code, o[code], want)
		}
	}
	for _, code := range []byte{15, 119} {
		if v, ok := o[code]; ok {
			t.Errorf("option %d given: %q", code, v)
		}
	}
	if d := destination(req); d.String() != "255.255.255.255:68" {
		t.Errorf("an offer to a client with no address goes to %s", d)
	}

	req, _ = parseRequest(clientMessage(dhcpRequest, netip.Addr{}, option(optRequested, addr), option(optServerID, gw)))
	ack, acked := testLAN.answer(req, addr)
	if !acked || replyOptions(t, ack)[optType][0] != dhcpAck {
		t.Fatalf("request: acked %v, %x", acked, ack)
	}
	req, _ = parseRequest(clientMessage(dhcpRequest, addr))
	ack, acked = testLAN.answer(req, addr)
	if !acked || netip.AddrFrom4([4]byte(ack[12:16])) != addr {
		t.Fatalf("renewal: acked %v, %x", acked, ack)
	}
	if d := destination(req); d != netip.AddrPortFrom(addr, 68) {
		t.Errorf("a renewal's answer goes to %s", d)
	}
	req, _ = parseRequest(clientMessage(dhcpRequest, netip.Addr{}, option(optRequested, netip.MustParseAddr("198.19.0.9"))))
	nak, acked := testLAN.answer(req, addr)
	if acked || replyOptions(t, nak)[optType][0] != dhcpNak || nak[16] != 0 {
		t.Fatalf("another address: acked %v, %x", acked, nak)
	}
	req, _ = parseRequest(clientMessage(dhcpRequest, netip.Addr{}, option(optRequested, addr), option(optServerID, netip.MustParseAddr("10.0.0.1"))))
	if b, _ := testLAN.answer(req, addr); b != nil {
		t.Fatalf("answered a request for another server: %x", b)
	}
	req, _ = parseRequest(clientMessage(dhcpDiscover, netip.Addr{}))
	if b, _ := testLAN.answer(req, netip.Addr{}); b != nil {
		t.Fatal("offered an address to a stranger")
	}
	req, _ = parseRequest(clientMessage(dhcpRelease, addr))
	if b, _ := testLAN.answer(req, addr); b != nil {
		t.Fatal("answered a release")
	}
	for _, b := range [][]byte{nil, make([]byte, 300), clientMessage(dhcpDiscover, netip.Addr{})[:250]} {
		if r, err := parseRequest(b); err == nil && r.typ != dhcpDiscover {
			t.Errorf("parsed %x", b)
		}
	}
	bad := clientMessage(dhcpDiscover, netip.Addr{}, []byte{optRequested, 40, 1})
	if _, err := parseRequest(bad); err == nil {
		t.Error("an option running past the end was taken")
	}
}

func TestAddressesAreStableAndNeverShared(t *testing.T) {
	taken := map[netip.Addr]string{}
	a, err := allocate(Prefix, taken, "u24", "uuid-1", "", netip.Addr{})
	if err != nil || !Prefix.Contains(a) || a == Gateway(Prefix) || a == Prefix.Addr() || a == lastAddr(Prefix) {
		t.Fatalf("%s %v", a, err)
	}
	again, _ := allocate(Prefix, taken, "u24", "uuid-1", "", netip.Addr{})
	if again != a {
		t.Fatalf("the same machine was given %s then %s", a, again)
	}
	taken[a] = "other"
	b, err := allocate(Prefix, taken, "u24", "uuid-1", "", netip.Addr{})
	if err != nil || b == a {
		t.Fatalf("%s %v", b, err)
	}
	delete(taken, a)
	if c, _ := allocate(Prefix, taken, "u24", "uuid-1", "", b); c != b {
		t.Fatalf("the last address %s was not kept: %s", b, c)
	}
	if c, err := allocate(Prefix, taken, "u24", "uuid-1", "198.19.0.77", b); err != nil || c.String() != "198.19.0.77" {
		t.Fatalf("%s %v", c, err)
	}
	taken[netip.MustParseAddr("198.19.0.77")] = "other"
	for want, why := range map[string]string{"198.19.0.77": "other's", "198.19.0.1": "not one of", "198.19.0.255": "not one of", "10.0.0.2": "not one of", "x": "not an IPv4"} {
		if _, err := allocate(Prefix, taken, "u24", "uuid-1", want, netip.Addr{}); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v, want %q", want, err, why)
		}
	}
	full := map[netip.Addr]string{}
	for i := 2; i < 255; i++ {
		full[netip.AddrFrom4([4]byte{198, 19, 0, byte(i)})] = "x"
	}
	if _, err := allocate(Prefix, full, "u24", "uuid-1", "", netip.Addr{}); err != errFull {
		t.Fatalf("full: %v", err)
	}
	m1, m2 := MACFor("uuid-1"), MACFor("uuid-2")
	if m1.String() != MACFor("uuid-1").String() || m1.String() == m2.String() || m1[0]&1 != 0 || m1[0]&2 == 0 {
		t.Fatalf("MACs %s %s", m1, m2)
	}
}

func query(t *testing.T, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 77, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET})
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTheResolverAnswersTheGuestsNamesAndPassesOnTheRest(t *testing.T) {
	r := resolver{gateway: Gateway(Prefix), lookup: func(n string) (netip.Addr, bool) {
		if n == "u24" {
			return netip.MustParseAddr("198.19.0.10"), true
		}
		return netip.Addr{}, false
	}}
	read := func(b []byte) (dnsmessage.Header, []dnsmessage.Resource) {
		t.Helper()
		var m dnsmessage.Message
		if err := m.Unpack(b); err != nil {
			t.Fatal(err)
		}
		return m.Header, m.Answers
	}
	for name, want := range map[string]string{
		"u24.fev.local.": "198.19.0.10", "U24.Fev.Local.": "198.19.0.10", "docker.fev.internal.": "198.19.0.1",
	} {
		a := r.answer(query(t, name, dnsmessage.TypeA))
		h, rr := read(a)
		if h.ID != 77 || !h.Response || h.RCode != dnsmessage.RCodeSuccess || len(rr) != 1 || netip.AddrFrom4(rr[0].Body.(*dnsmessage.AResource).A).String() != want {
			t.Errorf("%s: %+v %v", name, h, rr)
		}
		h, rr = read(r.answer(query(t, name, dnsmessage.TypeAAAA)))
		if h.RCode != dnsmessage.RCodeSuccess || len(rr) != 0 {
			t.Errorf("%s AAAA: %+v %v", name, h, rr)
		}
	}
	for _, name := range []string{"nope.fev.local.", "a.u24.fev.local.", "fev.local."} {
		if h, _ := read(r.answer(query(t, name, dnsmessage.TypeA))); h.RCode != dnsmessage.RCodeNameError {
			t.Errorf("%s: %v", name, h.RCode)
		}
	}
	for _, name := range []string{"host.fev.internal.", "u24.", "example.com.", "x.fev.internal."} {
		if a := r.answer(query(t, name, dnsmessage.TypeA)); a != nil {
			t.Errorf("%s answered here", name)
		}
	}
	if r.answer([]byte{1, 2, 3}) != nil {
		t.Error("answered garbage")
	}
}

func TestTheResolverAnswersDottedNamesInAnyCase(t *testing.T) {
	byKey := map[string]netip.Addr{}
	for name, addr := range map[string]string{"Mixed.Case": "198.19.0.11", "1abc": "198.19.0.12", "UPPER": "198.19.0.13", "a.b.c": "198.19.0.14"} {
		byKey[relay.MachineNameKey(name)] = netip.MustParseAddr(addr)
	}
	r := resolver{gateway: Gateway(Prefix), lookup: func(n string) (netip.Addr, bool) {
		a, ok := byKey[relay.MachineNameKey(n)]
		return a, ok
	}}
	for name, want := range map[string]string{
		"mixed.case.fev.local.": "198.19.0.11", "MIXED.CASE.fev.local.": "198.19.0.11", "Mixed.Case.Fev.Local.": "198.19.0.11",
		"1abc.fev.local.": "198.19.0.12", "1ABC.fev.local.": "198.19.0.12",
		"upper.fev.local.": "198.19.0.13", "UPPER.fev.local.": "198.19.0.13",
		"a.b.c.fev.local.": "198.19.0.14", "A.B.C.fev.local.": "198.19.0.14",
	} {
		var m dnsmessage.Message
		if err := m.Unpack(r.answer(query(t, name, dnsmessage.TypeA))); err != nil {
			t.Fatal(err)
		}
		if m.Header.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 1 || netip.AddrFrom4(m.Answers[0].Body.(*dnsmessage.AResource).A).String() != want {
			t.Errorf("%s: %+v %v", name, m.Header, m.Answers)
			continue
		}
		if got := m.Answers[0].Header.Name.String(); got != name {
			t.Errorf("%s: answered as %s", name, got)
		}
	}
	for _, name := range []string{"case.fev.local.", "b.c.fev.local.", "x.1abc.fev.local."} {
		var m dnsmessage.Message
		if err := m.Unpack(r.answer(query(t, name, dnsmessage.TypeA))); err != nil {
			t.Fatal(err)
		}
		if m.Header.RCode != dnsmessage.RCodeNameError {
			t.Errorf("%s: %v", name, m.Header.RCode)
		}
	}
}

func TestTheRulesAreMadeOnceAndLeaveDockersAlone(t *testing.T) {
	fresh := restoreScript(parseSave(""), "fev0", Prefix, "eth0")
	for _, want := range []string{
		"*filter\n:FEV-MACHINES - [0:0]\n:DOCKER-USER - [0:0]\n:DOCKER-INTERNAL - [0:0]\n",
		"-A FEV-MACHINES -i fev0 -j DOCKER-INTERNAL\n-A FEV-MACHINES -i fev0 -j ACCEPT\n",
		"-A FEV-MACHINES -o fev0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n",
		"-A DOCKER-USER -j FEV-MACHINES\n",
		"*nat\n:FEV-MACHINES - [0:0]\n-A FEV-MACHINES -s 198.19.0.0/24 -o eth0 -j MASQUERADE\n-A POSTROUTING -j FEV-MACHINES\nCOMMIT\n",
	} {
		if !strings.Contains(fresh, want) {
			t.Errorf("fresh script lacks %q:\n%s", want, fresh)
		}
	}
	saved := `# Generated by iptables-save v1.8.11 (nf_tables)
*filter
:INPUT ACCEPT [0:0]
:FORWARD ACCEPT [14:936]
:FEV-MACHINES - [0:0]
:DOCKER-INTERNAL - [0:0]
:DOCKER-USER - [0:0]
-A FORWARD -j DOCKER-USER
-A DOCKER-INTERNAL ! -s 172.18.0.0/16 -o br-0dc7f806bd8e -j DROP
-A DOCKER-USER -j FEV-MACHINES
-A DOCKER-USER -s 10.0.0.0/8 -j DROP
COMMIT
*nat
:POSTROUTING ACCEPT [8:488]
:FEV-MACHINES - [0:0]
-A POSTROUTING -s 172.17.0.0/16 ! -o docker0 -j MASQUERADE
-A POSTROUTING -j FEV-MACHINES
COMMIT
`
	again := restoreScript(parseSave(saved), "fev0", Prefix, "eth0")
	for _, never := range []string{":DOCKER-USER", ":DOCKER-INTERNAL", "-A DOCKER-INTERNAL", "DOCKER-USER -j", "-A POSTROUTING", "docker0", "FORWARD"} {
		if strings.Contains(again, never) {
			t.Errorf("again: %q in\n%s", never, again)
		}
	}
	if !strings.Contains(again, "-A FEV-MACHINES -i fev0 -j DOCKER-INTERNAL\n-A FEV-MACHINES -i fev0 -j ACCEPT\n") {
		t.Errorf("again: no refill:\n%s", again)
	}
	if s := restoreScript(parseSave(""), "fev0", Prefix, ""); strings.Contains(s, "MASQUERADE") || !strings.Contains(s, "*nat\n:FEV-MACHINES") {
		t.Errorf("no uplink:\n%s", s)
	}
}

func TestTheResolversNegativeAnswersCarryTheZonesSOA(t *testing.T) {
	r := resolver{gateway: Gateway(Prefix), lookup: func(n string) (netip.Addr, bool) {
		if n == "u24" {
			return netip.MustParseAddr("198.19.0.10"), true
		}
		return netip.Addr{}, false
	}}
	soa := func(name string, typ dnsmessage.Type) (dnsmessage.Header, *dnsmessage.SOAResource, string) {
		t.Helper()
		var m dnsmessage.Message
		if err := m.Unpack(r.answer(query(t, name, typ))); err != nil {
			t.Fatal(err)
		}
		if len(m.Answers) != 0 || len(m.Authorities) != 1 {
			t.Fatalf("%s: answers %v, authorities %v", name, m.Answers, m.Authorities)
		}
		a := m.Authorities[0]
		s, ok := a.Body.(*dnsmessage.SOAResource)
		if !ok || a.Header.TTL != 1 || s.MinTTL != 1 {
			t.Fatalf("%s: %+v", name, a)
		}
		return m.Header, s, a.Header.Name.String()
	}
	if h, _, apex := soa("nope.fev.local.", dnsmessage.TypeA); h.RCode != dnsmessage.RCodeNameError || !h.Authoritative || apex != "fev.local." {
		t.Errorf("no such name: %+v %s", h, apex)
	}
	if h, _, apex := soa("u24.fev.local.", dnsmessage.TypeAAAA); h.RCode != dnsmessage.RCodeSuccess || apex != "fev.local." {
		t.Errorf("no data: %+v %s", h, apex)
	}
	if h, _, apex := soa("docker.fev.internal.", dnsmessage.TypeAAAA); h.RCode != dnsmessage.RCodeSuccess || apex != "fev.internal." {
		t.Errorf("no data in fev.internal: %+v %s", h, apex)
	}
	var m dnsmessage.Message
	if err := m.Unpack(r.answer(query(t, "u24.fev.local.", dnsmessage.TypeA))); err != nil || len(m.Answers) != 1 || len(m.Authorities) != 0 {
		t.Errorf("positive: %v %+v", err, m)
	}
	r.forwardLocal = true
	if a, fallback := r.decide(query(t, "u24.fev.local.", dnsmessage.TypeA)); a != nil || fallback == nil {
		t.Errorf("a machine's name: answered here %v, fallback %v", a != nil, fallback != nil)
	}
	for _, name := range []string{"web.fev.local.", "a.u24.fev.local.", "fev.local.", "api.shop.fev.local."} {
		if a, fallback := r.decide(query(t, name, dnsmessage.TypeA)); a != nil || fallback != nil {
			t.Errorf("%s answered here with the zone forwarded", name)
		}
	}
}

func TestAContainerKeepsANameAMachineShares(t *testing.T) {
	r := resolver{gateway: Gateway(Prefix), forwardLocal: true, lookup: func(n string) (netip.Addr, bool) {
		if n == "dev" {
			return netip.MustParseAddr("198.19.0.12"), true
		}
		return netip.Addr{}, false
	}}
	q := query(t, "dev.fev.local.", dnsmessage.TypeA)
	a, fallback := r.decide(q)
	if a != nil || fallback == nil {
		t.Fatalf("the shared name was answered here (%v) or has no fallback (%v)", a != nil, fallback == nil)
	}
	netstack := func(rcode dnsmessage.RCode, addr netip.Addr) []byte {
		var p dnsmessage.Parser
		h, _ := p.Start(q)
		qq, _ := p.Question()
		return reply(h, qq, rcode, addr)
	}
	container := netstack(dnsmessage.RCodeSuccess, netip.MustParseAddr("198.19.4.9"))
	nodata := netstack(dnsmessage.RCodeSuccess, netip.Addr{})
	for what, c := range map[string]struct {
		got  []byte
		want bool
	}{
		"the container's address": {container, false},
		"no data":                 {nodata, false},
		"no such name":            {netstack(dnsmessage.RCodeNameError, netip.Addr{}), true},
		"no answer":               {nil, true},
		"a short answer":          {[]byte{0, 1}, true},
	} {
		if useFallback(c.got) != c.want {
			t.Errorf("%s: fallback %v, want %v", what, !c.want, c.want)
		}
	}
	var m dnsmessage.Message
	if err := m.Unpack(fallback); err != nil || len(m.Answers) != 1 || netip.AddrFrom4(m.Answers[0].Body.(*dnsmessage.AResource).A) != netip.MustParseAddr("198.19.0.12") {
		t.Errorf("the fallback is not the machine's address: %v %+v", err, m.Answers)
	}
}

func TestTheResolverAnswersAMachinesIPv6Address(t *testing.T) {
	v6 := map[string]netip.Addr{"u24": netip.MustParseAddr("fd5b:6e2a:91c0:1:5054:ff:fe12:3456")}
	r := resolver{gateway: Gateway(Prefix), lookup: func(n string) (netip.Addr, bool) {
		switch n {
		case "u24":
			return netip.MustParseAddr("198.19.0.10"), true
		case "alp":
			return netip.MustParseAddr("198.19.0.11"), true
		}
		return netip.Addr{}, false
	}, lookup6: func(n string) netip.Addr { return v6[n] }}
	var m dnsmessage.Message
	if err := m.Unpack(r.answer(query(t, "U24.fev.local.", dnsmessage.TypeAAAA))); err != nil {
		t.Fatal(err)
	}
	if m.Header.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 1 {
		t.Fatalf("AAAA: %+v %v", m.Header, m.Answers)
	}
	if got := netip.AddrFrom16(m.Answers[0].Body.(*dnsmessage.AAAAResource).AAAA); got != v6["u24"] || m.Answers[0].Header.Name.String() != "U24.fev.local." {
		t.Errorf("AAAA %s as %s", got, m.Answers[0].Header.Name)
	}
	if err := m.Unpack(r.answer(query(t, "alp.fev.local.", dnsmessage.TypeAAAA))); err != nil || m.Header.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
		t.Errorf("AAAA of a machine with no IPv6 address: %v %+v %v", err, m.Header, m.Answers)
	}
	if err := m.Unpack(r.answer(query(t, "u24.fev.local.", dnsmessage.TypeA))); err != nil || len(m.Answers) != 1 || m.Answers[0].Header.Type != dnsmessage.TypeA {
		t.Errorf("A: %v %+v", err, m.Answers)
	}
}
