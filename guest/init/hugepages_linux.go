package main

import (
	"fmt"
	"os"
	"syscall"

	"fev/guest/internal/thp"
)

func hugePagesOff() {
	if off, err := thp.IsOff(); err == nil && off {
		return
	}
	if err := thp.Off(); err != nil {
		fmt.Fprintf(os.Stderr, "fev-init: transparent huge pages stay on for fev's own processes: %v\n", err)
		return
	}
	if len(os.Args) == 0 {
		return
	}
	err := syscall.Exec(os.Args[0], os.Args, os.Environ())
	fmt.Fprintf(os.Stderr, "fev-init: executing itself again with transparent huge pages off: %v; its first heap pages stay huge\n", err)
}
