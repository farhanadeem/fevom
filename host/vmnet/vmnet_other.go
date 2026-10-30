//go:build !darwin || !cgo

package vmnet

import (
	"net"
	"net/netip"
	"os"
)

const Supported = false

type Interface struct{}

func Start(Config) (*Interface, error)        { return nil, ErrUnsupported }
func (*Interface) Name() string               { return "" }
func (*Interface) MTU() int                   { return 0 }
func (*Interface) PacketMTU() int             { return 0 }
func (*Interface) MAC() net.HardwareAddr      { return nil }
func (*Interface) Tunnel() *os.File           { return nil }
func (*Interface) Stopped() <-chan struct{}   { return nil }
func (*Interface) SetPrefixes([]netip.Prefix) {}
func (*Interface) SetZones([]string)          {}
func (*Interface) Stats() Stats               { return Stats{} }
func (*Interface) ServeMDNS(*Answerer)        {}
func (*Interface) Close() error               { return nil }

func Bridges() string { return "" }
