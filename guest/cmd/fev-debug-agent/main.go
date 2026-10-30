package main

import (
	"os"

	"fev/guest/debug/remote"
)

func main() { os.Exit(remote.Main(os.Args[1:])) }
