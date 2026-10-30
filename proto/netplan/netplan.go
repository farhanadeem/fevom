package netplan

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
)

const (
	ParamPlan = "fev.machnet"
	ParamNet6 = "fev.net6"
)

const (
	WebHTTPPort  = 15080
	WebHTTPSPort = 15443
	ProxyPort    = 3128
)

var DefaultBase = netip.MustParsePrefix("198.19.0.0/16")

var Candidates = []netip.Prefix{
	DefaultBase,
	netip.MustParsePrefix("10.198.0.0/16"),
	netip.MustParsePrefix("10.234.0.0/16"),
	netip.MustParsePrefix("10.251.0.0/16"),
}

var allowed = append(append([]netip.Prefix{}, private...), netip.MustParsePrefix("198.18.0.0/15"))

var private = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

func Private(p netip.Prefix) bool {
	for _, q := range private {
		if p.Addr().Is4() && q.Bits() <= p.Bits() && q.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

func PrivateCandidates() []netip.Prefix {
	var out []netip.Prefix
	for _, c := range Candidates {
		if Private(c) {
			out = append(out, c)
		}
	}
	return out
}

const BaseBits = 16

type Plan struct {
	base netip.Prefix
}

func New(base netip.Prefix) (Plan, error) {
	if !base.IsValid() || !base.Addr().Is4() {
		return Plan{}, fmt.Errorf("%s is not an IPv4 prefix", base)
	}
	if base.Bits() != BaseBits {
		return Plan{}, fmt.Errorf("%s is a /%d; the plan is a /%d", base, base.Bits(), BaseBits)
	}
	if base.Masked() != base {
		return Plan{}, fmt.Errorf("%s has host bits set; write %s", base, base.Masked())
	}
	for _, a := range allowed {
		if a.Contains(base.Addr()) {
			return Plan{base: base}, nil
		}
	}
	return Plan{}, fmt.Errorf("%s is outside private (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16) and benchmark (198.18.0.0/15) space", base)
}

func Parse(s string) (Plan, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(s))
	if err != nil {
		return Plan{}, fmt.Errorf("%q is not a prefix: %w", s, err)
	}
	return New(p)
}

func Default() Plan { return Plan{base: DefaultBase} }

func (p Plan) Base() netip.Prefix { return p.base }

func (p Plan) at(c, d byte) netip.Addr {
	a := p.base.Addr().As4()
	return netip.AddrFrom4([4]byte{a[0], a[1], c, d})
}

func (p Plan) prefix(c byte, bits int) netip.Prefix {
	return netip.PrefixFrom(p.at(c, 0), bits)
}

func (p Plan) Machines() netip.Prefix      { return p.prefix(0, 24) }
func (p Plan) MachinesGateway() netip.Addr { return p.at(0, 1) }

func (p Plan) Uplink() netip.Prefix      { return p.prefix(1, 24) }
func (p Plan) UplinkGateway() netip.Addr { return p.at(1, 1) }
func (p Plan) Guest() netip.Prefix       { return netip.PrefixFrom(p.at(1, 2), 24) }
func (p Plan) Host() netip.Addr          { return p.at(1, 254) }

func (p Plan) HostLoSource() netip.Addr { return p.at(1, 253) }

func (p Plan) ProxyEndpoint() netip.AddrPort {
	return netip.AddrPortFrom(p.UplinkGateway(), ProxyPort)
}

func (p Plan) Docker0() netip.Prefix { return netip.PrefixFrom(p.at(2, 1), 24) }

func (p Plan) Tunnel() netip.Prefix   { return p.prefix(3, 24) }
func (p Plan) TunnelMac() netip.Addr  { return p.at(3, 1) }
func (p Plan) TunnelPeer() netip.Addr { return p.at(3, 2) }

func (p Plan) TunnelMac46() netip.Addr { return p.at(3, 3) }

func (p Plan) Names() netip.Prefix { return p.prefix(4, 22) }
func (p Plan) Index() netip.Addr   { return p.at(4, 1) }

func (p Plan) firstName() netip.Addr         { return p.at(4, 2) }
func (p Plan) lastName() netip.Addr          { return p.at(7, 254) }
func (p Plan) poolPrefix(i int) netip.Prefix { return p.prefix(byte(64+64*i), 18) }

func (p Plan) NameAddress(a netip.Addr) bool {
	return p.Names().Contains(a) && a.Compare(p.firstName()) >= 0 && a.Compare(p.lastName()) <= 0
}

func (p Plan) NameAddresses() int { return 1024 - 3 }

func (p Plan) KubeLoadBalancers() netip.Prefix { return p.prefix(8, 22) }
func (p Plan) KubePods() netip.Prefix          { return p.prefix(16, 20) }
func (p Plan) KubeServices() netip.Prefix      { return p.prefix(32, 20) }
func (p Plan) KubeDNS() netip.Addr             { return p.at(32, 10) }

const KubePodsPerNode = 24

func (p Plan) Kube() []netip.Prefix {
	return []netip.Prefix{p.KubeLoadBalancers(), p.KubePods(), p.KubeServices()}
}

type Pool struct {
	Base netip.Prefix
	Size int
}

func (p Plan) Pools() []Pool {
	return []Pool{{Base: p.poolPrefix(0), Size: 24}, {Base: p.poolPrefix(1), Size: 24}}
}

func (p Plan) Routed() []netip.Prefix {
	return []netip.Prefix{p.Machines(), p.Docker0().Masked(), p.Names(), p.poolPrefix(0), p.poolPrefix(1)}
}

func (p Plan) Prefixes() []netip.Prefix {
	return append([]netip.Prefix{p.Machines(), p.Uplink(), p.Docker0().Masked(), p.Tunnel(), p.Names(), p.poolPrefix(0), p.poolPrefix(1)}, p.Kube()...)
}

func (p Plan) Param() string { return ParamPlan + "=" + p.base.String() }

func Choose(candidates, taken []netip.Prefix) (Plan, bool) {
	for _, c := range candidates {
		p, err := New(c)
		if err != nil {
			continue
		}
		if len(Overlapping(p, taken)) == 0 {
			return p, true
		}
	}
	return Plan{}, false
}

func Overlapping(p Plan, taken []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, t := range taken {
		if !t.Addr().Is4() || t.Bits() == 0 {
			continue
		}
		for _, q := range p.Prefixes() {
			if q.Overlaps(t) {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

type Plan6 struct {
	prefix netip.Prefix
}

var ula = netip.MustParsePrefix("fd00::/8")

const Bits6 = 48

func New6(prefix netip.Prefix) (Plan6, error) {
	if !prefix.IsValid() || !prefix.Addr().Is6() || prefix.Addr().Is4In6() {
		return Plan6{}, fmt.Errorf("%s is not an IPv6 prefix", prefix)
	}
	if prefix.Bits() != Bits6 {
		return Plan6{}, fmt.Errorf("%s is a /%d; the IPv6 plan is a /%d", prefix, prefix.Bits(), Bits6)
	}
	if prefix.Masked() != prefix {
		return Plan6{}, fmt.Errorf("%s has host bits set; write %s", prefix, prefix.Masked())
	}
	if !ula.Contains(prefix.Addr()) {
		return Plan6{}, fmt.Errorf("%s is not a locally assigned ULA (fd00::/8)", prefix)
	}
	return Plan6{prefix: prefix}, nil
}

func Parse6(s string) (Plan6, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(s))
	if err != nil {
		return Plan6{}, fmt.Errorf("%q is not a prefix: %w", s, err)
	}
	return New6(p)
}

func NewULA(r io.Reader) (netip.Prefix, error) {
	if r == nil {
		r = rand.Reader
	}
	var b [16]byte
	b[0] = 0xfd
	if _, err := io.ReadFull(r, b[1:6]); err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(netip.AddrFrom16(b), Bits6), nil
}

func (p Plan6) Prefix() netip.Prefix { return p.prefix }

const (
	Subnet6Uplink   = 0
	Subnet6Machines = 1
	Subnet6Tunnel   = 2
	Subnet6Names    = 3
	Subnet6Pools    = 0x100
)

func (p Plan6) Subnet(n uint16) netip.Prefix {
	b := p.prefix.Addr().As16()
	b[6], b[7] = byte(n>>8), byte(n)
	return netip.PrefixFrom(netip.AddrFrom16(b), 64)
}

func (p Plan6) host(n uint16, iid uint64) netip.Addr {
	b := p.Subnet(n).Addr().As16()
	for i := 0; i < 8; i++ {
		b[15-i] = byte(iid >> (8 * i))
	}
	return netip.AddrFrom16(b)
}

func (p Plan6) UplinkGateway6() netip.Addr { return p.host(Subnet6Uplink, 1) }
func (p Plan6) Guest6() netip.Prefix       { return netip.PrefixFrom(p.host(Subnet6Uplink, 2), 64) }
func (p Plan6) Host6() netip.Addr          { return p.host(Subnet6Uplink, 0xfe) }

func (p Plan6) MachinesGateway6() netip.Addr { return p.host(Subnet6Machines, 1) }

func (p Plan6) TunnelMac6() netip.Addr  { return p.host(Subnet6Tunnel, 1) }
func (p Plan6) TunnelPeer6() netip.Addr { return p.host(Subnet6Tunnel, 2) }

func (p Plan6) Names6() netip.Prefix { return p.Subnet(Subnet6Names) }

func (p Plan6) NameV4(a netip.Addr) (netip.Addr, bool) {
	if !a.Is6() || !p.Names6().Contains(a) {
		return netip.Addr{}, false
	}
	b := a.As16()
	if b[8]|b[9]|b[10]|b[11] != 0 {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte(b[12:16])), true
}

func (p Plan6) Routed6() []netip.Prefix {
	return []netip.Prefix{p.Subnet(Subnet6Machines), p.Names6(), p.Pool6().Base}
}

func (p Plan6) Name6(v4 netip.Addr) netip.Addr {
	a := v4.As4()
	return p.host(Subnet6Names, uint64(a[0])<<24|uint64(a[1])<<16|uint64(a[2])<<8|uint64(a[3]))
}

func (p Plan6) Pool6() Pool {
	return Pool{Base: netip.PrefixFrom(p.Subnet(Subnet6Pools).Addr(), 56), Size: 64}
}

func (p Plan6) Param() string { return ParamNet6 + "=" + p.prefix.String() }

func Arg(cmdline, name string) string {
	value := ""
	for _, field := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(field, name+"="); ok {
			value = v
		}
	}
	return value
}

func FromCmdline(cmdline string) (p Plan, p6 Plan6, ok6 bool, err error) {
	p = Default()
	if v := Arg(cmdline, ParamPlan); v != "" {
		if p, err = Parse(v); err != nil {
			return Plan{}, Plan6{}, false, fmt.Errorf("%s: %w", ParamPlan, err)
		}
	}
	if v := Arg(cmdline, ParamNet6); v != "" {
		if p6, err = Parse6(v); err != nil {
			return Plan{}, Plan6{}, false, fmt.Errorf("%s: %w", ParamNet6, err)
		}
		ok6 = true
	}
	return p, p6, ok6, nil
}

var ErrNoBase = errors.New("every candidate base for fev's addresses overlaps a network of this Mac; set network.subnet")
