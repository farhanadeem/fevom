//go:build linux

package main

import (
	"os"
	"path/filepath"

	"fev/proto/bridge"
)

const noBridge = 126

func macExecMain(args []string) int {
	if len(args) < 2 {
		sayAs = "fev-macexec"
		say("the interpreter of Mac programs in the VM's binfmt_misc table; run a Mac program by its path instead")
		return 2
	}
	path, argv := args[0], args[1:]
	sayAs = path
	if argv[0] == "" {
		argv[0] = path
	}
	if _, err := os.Lstat(socketPath); err != nil {
		say("a Mac program, which runs only in a fev machine: not in a container or an isolated machine (there is no command bridge, %s)", socketPath)
		return noBridge
	}
	abs := path
	if !filepath.IsAbs(abs) {
		dir, err := os.Getwd()
		if err != nil {
			say("the working directory: %v", err)
			return noBridge
		}
		abs = filepath.Join(dir, abs)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil || !filepath.IsAbs(real) {
		real = filepath.Clean(abs)
	}
	return runHeader(&bridge.Header{Verb: bridge.VerbExec, Path: real, Argv: argv})
}
