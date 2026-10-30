//go:build linux

package main

import (
	"fmt"
	"os"

	"fev/guest/sftpserver"
)

func init() {
	commands["sftp"] = func(args []string) int {
		if len(args) != 0 {
			fmt.Fprintln(os.Stderr, "usage: fev-guest sftp (no arguments: the protocol is on standard input and output)")
			return 2
		}
		return sftpserver.Main("fev-guest sftp")
	}
}
