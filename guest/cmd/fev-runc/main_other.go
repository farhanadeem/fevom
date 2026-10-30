//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "fev-runc runs in the guest's engine (Linux)")
	os.Exit(1)
}
