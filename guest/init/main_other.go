//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "fev-init runs only as PID 1 of the fev guest; build it with GOOS=linux GOARCH=arm64")
	os.Exit(1)
}
