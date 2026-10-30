package privhelper

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"fev/proto/netplan"
)

const ProtocolVersion = 1

const SecurityLevel = 1

const (
	Label               = "dev.fevom.privhelper"
	SocketPath          = "/var/run/dev.fevom.privhelper.sock"
	InstallPath         = "/Library/PrivilegedHelperTools/dev.fevom.privhelper"
	PlistPath           = "/Library/LaunchDaemons/dev.fevom.privhelper.plist"
	BootPlistPath       = "/Library/LaunchDaemons/dev.fevom.privhelper.boot.plist"
	RecordPath          = "/Library/Application Support/dev.fevom/privhelper/install.json"
	ResolverPath        = "/etc/resolver/fev.local"
	ResolverHeader      = "# fev: written by `fev setup --admin`; removed by `fev setup --admin --undo`"
	DockerSockPath      = "/var/run/docker.sock"
	DefaultResolverPort = 15353
)

const (
	ResolverFormFile   = "file"
	ResolverFormStore  = "store"
	ResolverFormTunnel = "utun"
)

const StoreServicePrefix = "dev.fevom.resolver."

func StoreKey(name string) string {
	return "State:/Network/Service/" + StoreServicePrefix + name + "/DNS"
}

func StoreSearches(name string) bool {
	return strings.Count(strings.TrimSuffix(name, "."), ".") == 1
}

type InstallRecord struct {
	ProtocolVersion   int         `json:"protocol_version"`
	SecurityLevel     int         `json:"security_level"`
	Build             string      `json:"build"`
	UID               int         `json:"uid"`
	Home              string      `json:"home"`
	Base              string      `json:"base"`
	Base6             string      `json:"base6,omitempty"`
	Extra             []string    `json:"extra,omitempty"`
	Held              []HeldRoute `json:"held,omitempty"`
	ResolverPort      int         `json:"resolver_port"`
	ResolverForm      string      `json:"resolver_form,omitempty"`
	NoResolver        bool        `json:"no_resolver,omitempty"`
	DockerSockLink    bool        `json:"docker_sock_link,omitempty"`
	DockerSockConsent bool        `json:"docker_sock_consent,omitempty"`
	Paths             []string    `json:"paths"`
	InstalledAt       string      `json:"installed_at"`
}

type HeldRoute struct {
	Prefix    string `json:"prefix"`
	Interface string `json:"interface"`
	Flags     int    `json:"flags"`
}

func HeldFor(bases []netip.Prefix) []HeldRoute {
	var out []HeldRoute
	for _, b := range bases {
		flags := 0x1 | 0x800 | 0x8
		if b.Bits() == b.Addr().BitLen() {
			flags |= 0x4
		}
		out = append(out, HeldRoute{Prefix: b.Masked().String(), Interface: "lo0", Flags: flags})
	}
	return out
}

func HeldOf(r InstallRecord) []HeldRoute {
	if len(r.Held) > 0 {
		return r.Held
	}
	var bases []netip.Prefix
	for _, b := range []string{r.Base, r.Base6} {
		if p, err := netip.ParsePrefix(b); err == nil {
			bases = append(bases, p.Masked())
		}
	}
	return HeldFor(bases)
}

const HeldFlagsMask = 0x1 | 0x2 | 0x4 | 0x8 | 0x800 | 0x1000 | 0x1000000

const (
	MethodHello            = "hello"
	MethodTunnelOpen       = "tunnel.open"
	MethodRoutesSet        = "routes.set"
	MethodPortBind         = "port.bind"
	MethodDockerSockLink   = "dockersock.link"
	MethodDockerSockUnlink = "dockersock.unlink"
	MethodLocalNamesSet    = "localnames.set"
	MethodResolverSet      = "resolver.set"
	MethodResolverClear    = "resolver.clear"
)

const (
	CodeFailed         = -32002
	CodeNotImplemented = -32007
	CodeRefused        = -32010
	CodeTooSoon        = -32011
)

type Refusal struct {
	Arg    string `json:"arg"`
	Value  string `json:"value,omitempty"`
	Reason string `json:"reason"`
}

func (r Refusal) Error() string {
	if r.Value != "" {
		return fmt.Sprintf("%s %q: %s", r.Arg, r.Value, r.Reason)
	}
	return fmt.Sprintf("%s: %s", r.Arg, r.Reason)
}

type Empty struct{}

const (
	StateFev     = "fevs"
	StateAbsent  = "absent"
	StateForeign = "foreign"
)

type HelloResult struct {
	ProtocolVersion  int      `json:"protocol_version"`
	SecurityLevel    int      `json:"security_level"`
	Build            string   `json:"build"`
	UID              int      `json:"uid"`
	Home             string   `json:"home"`
	Base             string   `json:"base"`
	Base6            string   `json:"base6,omitempty"`
	ResolverPort     int      `json:"resolver_port"`
	Resolver         string   `json:"resolver"`
	DockerSock       string   `json:"docker_sock"`
	DockerSockTarget string   `json:"docker_sock_target,omitempty"`
	DockerSockLink   bool     `json:"docker_sock_link,omitempty"`
	LocalNames       []string `json:"local_names"`
	RoutesNAT64      bool     `json:"routes_nat64,omitempty"`
}

const (
	MinMTU = 1280
	MaxMTU = 65535
)

type TunnelParams struct {
	MTU  int  `json:"mtu"`
	IPv6 bool `json:"ipv6,omitempty"`
}

type Tunnel struct {
	Interface string `json:"interface"`
	Mac       string `json:"mac"`
	Peer      string `json:"peer"`
	Mac6      string `json:"mac6,omitempty"`
	MTU       int    `json:"mtu"`
}

const MaxRoutes = 64

type RoutesParams struct {
	Interface string   `json:"interface"`
	Prefixes  []string `json:"prefixes"`
}

type Routes struct {
	Interface string   `json:"interface"`
	Prefixes  []string `json:"prefixes"`
	Added     []string `json:"added,omitempty"`
	Removed   []string `json:"removed,omitempty"`
}

type PortParams struct {
	Proto  string `json:"proto"`
	Family int    `json:"family"`
	Port   int    `json:"port"`
}

type Bound struct {
	Address string `json:"address"`
}

type DockerSock struct {
	State  string `json:"state"`
	Target string `json:"target,omitempty"`
}

const (
	MaxLocalNames      = 32
	LocalNamesInterval = 1000
	ProbeMS            = 1000
)

type LocalNamesParams struct {
	Names          []string `json:"names"`
	NetworkChanged bool     `json:"network_changed,omitempty"`
}

const (
	NamePresent = "present"
	NameRefused = "refused"
	NameRemoved = "removed"
)

type LocalName struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	AnsweredBy string `json:"answered_by,omitempty"`
}

type LocalNames struct {
	Names []LocalName `json:"names"`
}

type Resolver struct {
	State string `json:"state"`
}

type Request struct {
	Version string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Reply struct {
	Version string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

func Refused(r Refusal) *Error {
	data, _ := json.Marshal(r)
	return &Error{Code: CodeRefused, Message: r.Error(), Data: data}
}

func AsRefusal(err error) (Refusal, bool) {
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeRefused {
		return Refusal{}, false
	}
	var r Refusal
	return r, json.Unmarshal(e.Data, &r) == nil
}

type Bases []netip.Prefix

func BasesOf(r InstallRecord) (Bases, error) {
	var out Bases
	for _, s := range append([]string{r.Base, r.Base6}, r.Extra...) {
		if s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Masked() != p {
			return nil, fmt.Errorf("the install record's base %q is not a masked prefix", s)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errors.New("the install record has no base")
	}
	return out, nil
}

func CheckRoutes(bases Bases, prefixes []string) ([]netip.Prefix, error) {
	return CheckRoutesNAT64(bases, nil, prefixes)
}

func CheckRoutesNAT64(bases Bases, nat64 []netip.Prefix, prefixes []string) ([]netip.Prefix, error) {
	if len(prefixes) > MaxRoutes {
		return nil, Refusal{Arg: "prefixes", Reason: fmt.Sprintf("more than %d prefixes", MaxRoutes)}
	}
	seen := map[netip.Prefix]bool{}
	out := make([]netip.Prefix, 0, len(prefixes))
	for i, s := range prefixes {
		arg := fmt.Sprintf("prefixes[%d]", i)
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, Refusal{Arg: arg, Value: s, Reason: "not a prefix"}
		}
		if p.Masked() != p {
			return nil, Refusal{Arg: arg, Value: s, Reason: "host bits set"}
		}
		inside := false
		for _, b := range bases {
			if b.Addr().Is4() == p.Addr().Is4() && b.Contains(p.Addr()) && p.Bits() >= b.Bits() {
				inside = true
				break
			}
		}
		if !inside && p.Addr().Is6() {
			inside = nat64Twin(bases, nat64, p)
		}
		if !inside {
			return nil, Refusal{Arg: arg, Value: s, Reason: "outside the address base recorded at install; run `sudo fev setup --admin` to record it"}
		}
		if seen[p] {
			return nil, Refusal{Arg: arg, Value: s, Reason: "named twice"}
		}
		seen[p] = true
		out = append(out, p)
	}
	return out, nil
}

func CheckPort(p PortParams) error {
	if p.Proto != "tcp" && p.Proto != "udp" {
		return Refusal{Arg: "proto", Value: p.Proto, Reason: "neither tcp nor udp"}
	}
	if p.Family != 4 && p.Family != 6 {
		return Refusal{Arg: "family", Value: fmt.Sprint(p.Family), Reason: "neither 4 nor 6"}
	}
	if p.Port < 1 || p.Port > 1023 {
		return Refusal{Arg: "port", Value: fmt.Sprint(p.Port), Reason: "not a port below 1024 (any other needs no helper)"}
	}
	return nil
}

func CheckMTU(mtu int) error {
	if mtu < MinMTU || mtu > MaxMTU {
		return Refusal{Arg: "mtu", Value: fmt.Sprint(mtu), Reason: fmt.Sprintf("outside %d to %d", MinMTU, MaxMTU)}
	}
	return nil
}

func CanonicalLocalName(name, own string) (string, error) {
	n := strings.ToLower(strings.TrimSuffix(name, "."))
	refuse := func(reason string) (string, error) {
		return "", Refusal{Arg: "names", Value: name, Reason: reason}
	}
	if len(n) > 253 {
		return refuse("longer than 253 bytes")
	}
	if n == "local" || !strings.HasSuffix(n, ".local") {
		return refuse("not a name under .local")
	}
	if n == "fev.local" || strings.HasSuffix(n, ".fev.local") {
		return refuse("fev.local and its names are fev's own resolver's already")
	}
	if o := strings.ToLower(strings.TrimSuffix(own, ".")); o != "" && n == o {
		return refuse("the Mac's own name")
	}
	for _, label := range strings.Split(n, ".") {
		if len(label) < 1 || len(label) > 63 {
			return refuse("a label is empty or longer than 63 bytes")
		}
		for i, c := range label {
			ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' && i > 0 && i < len(label)-1
			if !ok {
				return refuse("a label holds something other than letters, digits and inner hyphens")
			}
		}
	}
	return n, nil
}

func Overlaps(a, b string) bool {
	return a == b || strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a)
}

func nat64Twin(bases Bases, nat64 []netip.Prefix, p netip.Prefix) bool {
	for _, pref := range nat64 {
		if netplan.CheckNAT64(pref) != nil || !pref.Contains(p.Addr()) || p.Bits() < pref.Bits() {
			continue
		}
		v4, ok := netplan.NAT64V4(pref, p.Addr())
		if !ok {
			continue
		}
		for _, b := range bases {
			if !b.Addr().Is4() || !b.Contains(v4) {
				continue
			}
			for bits := b.Bits(); bits <= 32; bits++ {
				if q := netip.PrefixFrom(v4, bits); q.Masked() == q && netplan.NAT64Prefix(pref, q) == p {
					return true
				}
			}
		}
	}
	return false
}

func NAT64Twin(bases Bases, p netip.Prefix) bool {
	if !p.Addr().Is6() {
		return false
	}
	for _, l := range []int{32, 40, 48, 56, 64, 96} {
		if l > p.Bits() {
			break
		}
		pref := netip.PrefixFrom(p.Addr(), l).Masked()
		if netplan.CheckNAT64(pref) == nil && nat64Twin(bases, []netip.Prefix{pref}, p) {
			return true
		}
	}
	return false
}
