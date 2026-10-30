package main

import (
	"os"
	"path/filepath"

	"fev/host/cli"
	"fev/host/daemon/boottrace"
)

func main() {
	env := cli.Env{
		OriginNS: boottrace.Now(),
		Name:     filepath.Base(os.Args[0]),
		Args:     os.Args[1:],
		Stdin:    os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Admin:         admin{},
		Keychain:      keychain{},
		GraphicAccess: cli.GraphicAccess,
	}
	if cli.IsEntryName(env.Name) {
		os.Exit(cli.Entry(env))
	}
	os.Exit(cli.Main(env))
}
