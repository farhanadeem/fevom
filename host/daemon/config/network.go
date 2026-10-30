package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/netplan"
)

const (
	ProxyAuto = "auto"
	ProxyNone = "none"
)

const MaxProxyExclude = 256

var checks = map[string]func(v any) (any, error){
	api.KeyEmulationEngine: func(v any) (any, error) {
		switch v.(string) {
		case agent.EmulationEngineFEX, agent.EmulationEngineRosetta, agent.EmulationEngineAuto:
			return v, nil
		default:
			return nil, errors.New("must be auto, fex or rosetta")
		}
	},
	api.KeyDockerNodeName: func(v any) (any, error) {
		if err := agent.CheckNodeName(v.(string)); err != nil {
			return nil, err
		}
		return v, nil
	},
	api.KeyNetworkSubnet: func(v any) (any, error) {
		p, err := netplan.Parse(v.(string))
		if err != nil {
			return nil, err
		}
		return p.Base().String(), nil
	},
	api.KeyNetworkSubnet6: func(v any) (any, error) {
		s := strings.TrimSpace(v.(string))
		if s == "" {
			return "", nil
		}
		p, err := netplan.Parse6(s)
		if err != nil {
			return nil, err
		}
		return p.Prefix().String(), nil
	},
	api.KeyNetworkProxy:        func(v any) (any, error) { return checkProxy(v.(string)) },
	api.KeyNetworkProxyExclude: checkProxyExclude,
}

func checkProxy(s string) (any, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case ProxyAuto, ProxyNone:
		return strings.ToLower(s), nil
	case "":
		return nil, errors.New(`is "auto", "none" or a proxy's URL, such as http://proxy.example:3128`)
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, errors.New("is not a URL: " + Redact(api.KeyNetworkProxy, s).(string))
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	case "":
		return nil, fmt.Errorf(`needs a scheme: http://%s, or https://, socks5:// or socks5h://`, Redact(api.KeyNetworkProxy, s))
	default:
		return nil, fmt.Errorf("has the scheme %q; a proxy's is http, https, socks5 or socks5h", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("names no host")
	}
	if p := u.Port(); p != "" {
		n, err := netip.ParseAddrPort("0.0.0.0:" + p)
		if err != nil || n.Port() == 0 {
			return nil, fmt.Errorf("has the port %q; a port is 1 to 65535", p)
		}
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("has a path, a query or a fragment; a proxy's URL is scheme://[user:password@]host[:port]")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Path = ""
	return u.String(), nil
}

func checkProxyExclude(v any) (any, error) {
	in := v.([]string)
	if len(in) > MaxProxyExclude {
		return nil, fmt.Errorf("holds %d values; at most %d", len(in), MaxProxyExclude)
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		c, err := excludeValue(s)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, nil
}

func excludeValue(s string) (string, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked().String(), nil
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a.String(), nil
	}
	name := strings.ToLower(strings.TrimSuffix(s, "."))
	lead := ""
	for _, l := range []string{"*.", "."} {
		if strings.HasPrefix(name, l) {
			lead, name = l, name[len(l):]
			break
		}
	}
	if !hostName(name) {
		return "", fmt.Errorf("%q is not a host name, a domain (.corp.example), an address or a CIDR", s)
	}
	return lead + name, nil
}

func hostName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range []byte(label) {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

func listOf(v any) (any, error) {
	var in []string
	switch x := v.(type) {
	case []string:
		in = x
	case []any:
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, errors.New("must be a list of strings")
			}
			in = append(in, s)
		}
	case string:
		in = strings.Split(x, ",")
	default:
		return nil, errors.New("must be a list of strings, or one string of values separated by commas")
	}
	out := []string{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

func Redact(key string, v any) any {
	s, ok := v.(string)
	if key != api.KeyNetworkProxy || !ok || !strings.Contains(s, "@") {
		return v
	}
	u, err := url.Parse(s)
	if err != nil {
		return "xxxxx" + s[strings.LastIndexByte(s, '@'):]
	}
	return u.Redacted()
}
