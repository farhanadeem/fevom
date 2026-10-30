package agent

import (
	"errors"
	"fmt"
	"strings"
)

const (
	CopyIn  = "in"
	CopyOut = "out"
)

func CheckExecCopy(p ExecStartParams) error {
	if p.Copy == "" {
		return nil
	}
	if p.Context != ExecMachine {
		return fmt.Errorf("a copy is for context %s", ExecMachine)
	}
	if p.Login || p.Shell || p.TTY || p.StdinTTY || p.Subsystem != "" || len(p.Terminfo) > 0 {
		return errors.New("a copy has no login, shell, terminal, subsystem or terminfo")
	}
	for _, a := range p.Argv {
		if a == "" {
			return errors.New("a copy's paths are not empty")
		}
	}
	switch p.Copy {
	case CopyIn:
		if len(p.Argv) < 2 {
			return errors.New("a copy in names its destination and the names of what comes")
		}
		seen := map[string]bool{}
		for _, n := range p.Argv[1:] {
			if n == "." || n == ".." || strings.Contains(n, "/") {
				return fmt.Errorf("%q is not a name of one component", n)
			}
			if seen[n] {
				return fmt.Errorf("two of the copy's items are named %q", n)
			}
			seen[n] = true
		}
	case CopyOut:
		if len(p.Argv) == 0 {
			return errors.New("a copy out names what it copies")
		}
	default:
		return fmt.Errorf("no copy %q (have: %s, %s)", p.Copy, CopyIn, CopyOut)
	}
	return nil
}
