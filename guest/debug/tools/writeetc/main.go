package main

import (
	"fmt"
	"os"

	"fev/guest/debug"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "writeetc DIR")
		os.Exit(2)
	}
	if err := debug.WriteEtc(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "writeetc:", err)
		os.Exit(1)
	}
}
