package netcfg

import (
	"fmt"
	"net/netip"
	"strings"

	"fev/proto/netplan"
)

type Config struct {
	Address netip.Prefix
	Gateway netip.Addr
	DNS     netip.Addr
}

func FromPlan(p netplan.Plan) Config {
	return Config{Address: p.Guest(), Gateway: p.UplinkGateway(), DNS: p.UplinkGateway()}
}

var Default = FromPlan(netplan.Default())

const Param = "fev.net"

func Arg(cmdline, name string) string {
	value := ""
	for _, field := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(field, name+"="); ok {
			value = v
		}
	}
	return value
}

func FromCmdline(cmdline string) (Config, error) {
	if value := Arg(cmdline, Param); value != "" {
		return Parse(value)
	}
	p, _, _, err := netplan.FromCmdline(cmdline)
	if err != nil {
		return Config{}, err
	}
	return FromPlan(p), nil
}

func Parse(value string) (Config, error) {
	parts := strings.Split(value, ",")
	if len(parts) < 2 || len(parts) > 3 {
		return Config{}, fmt.Errorf("%s=%q: want ADDRESS/BITS,GATEWAY[,DNS]", Param, value)
	}
	var c Config
	var err error
	if c.Address, err = netip.ParsePrefix(parts[0]); err != nil {
		return Config{}, fmt.Errorf("%s: address %q: %v", Param, parts[0], err)
	}
	if c.Gateway, err = netip.ParseAddr(parts[1]); err != nil {
		return Config{}, fmt.Errorf("%s: gateway %q: %v", Param, parts[1], err)
	}
	c.DNS = c.Gateway
	if len(parts) == 3 {
		if c.DNS, err = netip.ParseAddr(parts[2]); err != nil {
			return Config{}, fmt.Errorf("%s: DNS server %q: %v", Param, parts[2], err)
		}
	}
	if !c.Address.Addr().Is4() || !c.Gateway.Is4() {
		return Config{}, fmt.Errorf("%s: the uplink is IPv4 for now, got %q", Param, value)
	}
	if !c.Address.Contains(c.Gateway) {
		return Config{}, fmt.Errorf("%s: gateway %s is not in %s", Param, c.Gateway, c.Address.Masked())
	}
	if c.Address.Addr() == c.Gateway {
		return Config{}, fmt.Errorf("%s: address and gateway are both %s", Param, c.Gateway)
	}
	return c, nil
}
