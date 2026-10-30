package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

var runcPaths = []string{"/usr/bin/runc", "/usr/local/bin/runc", "/usr/sbin/runc"}

func main() {
	c := parse(os.Args[1:])
	if c.verb == "create" || c.verb == "run" {
		byName := filepath.Base(os.Args[0]) == "runc"
		if err := selectEmulation(c, byName); err != nil {
			runtimeError(c, fmt.Errorf("emulation: %w", err))
			os.Exit(1)
		}
		if root, err := os.ReadFile(c.root); err == nil && !byName {
			if _, err := inject(c.bundle, c.id, root); err != nil {
				fmt.Fprintf(os.Stderr, "fev-runc: fev's root not added to %.12s: %v\n", c.id, err)
			}
		}
	}
	runc := runcPaths[0]
	for _, p := range runcPaths {
		if _, err := os.Stat(p); err == nil {
			runc = p
			break
		}
	}
	err := syscall.Exec(runc, append([]string{"runc"}, c.args...), os.Environ())
	runtimeError(c, fmt.Errorf("exec runc: %w", err))
	os.Exit(1)
}
