//go:build !linux

package main

import "net"

func authzPeer(string) func(net.Conn) error {
	return func(net.Conn) error { return nil }
}
