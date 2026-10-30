//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"fev/proto/bridge"
)

var commands = map[string]func(args []string) int{
	"mac":    macMain,
	"macctl": macctlMain,
}

func main() {
	os.Exit(dispatch(filepath.Base(os.Args[0]), os.Args[1:]))
}

func dispatch(name string, args []string) int {
	switch name {
	case "fev-guest":
		if len(args) == 0 {
			var names []string
			for n := range commands {
				names = append(names, n)
			}
			sort.Strings(names)
			fmt.Fprintf(os.Stderr, "usage: fev-guest COMMAND [ARG...]\ncommands: %v\n", names)
			return 2
		}
		if cmd, ok := commands[args[0]]; ok {
			return cmd(args[1:])
		}
		fmt.Fprintf(os.Stderr, "fev-guest: no command %q\n", args[0])
		return 2
	case "mac":
		return macMain(args)
	case "macctl":
		return macctlMain(args)
	case filepath.Base(bridge.ExecHandler):
		return macExecMain(args)
	}
	return runOnMac(append([]string{name}, args...))
}
