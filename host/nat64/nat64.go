package nat64

import (
	"net"
	"net/netip"
	"slices"

	"fev/proto/netplan"
)

func Prefixes() ([]netip.Prefix, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		ps, err := Of(i.Name)
		if err != nil {
			continue
		}
		for _, p := range ps {
			if netplan.CheckNAT64(p) == nil && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out, nil
}
