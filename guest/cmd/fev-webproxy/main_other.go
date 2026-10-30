//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "fev-webproxy runs in the guest, started by fev-init (Linux)")
	os.Exit(1)
}
