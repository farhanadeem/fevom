//go:build !linux

package registry

import (
	"errors"
	"net"
)

func Peer(procs string) func(net.Conn) (bool, error) {
	return func(net.Conn) (bool, error) { return false, errors.New("the peer check is Linux's") }
}
