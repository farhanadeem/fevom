//go:build !darwin || !cgo

package netstack

import "context"

type directOnly struct{}

func (directOnly) Decide(context.Context, string) ([]Upstream, error) {
	return []Upstream{{Kind: Direct}}, nil
}

func SystemProxies() ProxyDecider { return directOnly{} }

func ProxiesFromSettings(string) ProxyDecider { return directOnly{} }

func (directOnly) Configured() bool { return false }
