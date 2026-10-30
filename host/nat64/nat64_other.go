//go:build !darwin

package nat64

import "net/netip"

func Of(string) ([]netip.Prefix, error) { return nil, nil }
