package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"fev/proto/debugstore"
)

func notFound(sock string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("not-found CMD [ARGS...]")
	}
	cmd := args[0]
	cmdArgs := args[1:]

	c, err := dial(sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: command not found\n", cmd)
		os.Exit(127)
	}
	ctx := context.Background()
	var l debugstore.Lookup
	lerr := c.Call(ctx, debugstore.MethodLookup, debugstore.LookupParams{Command: cmd}, &l)
	var pick *debugstore.Candidate
	for i := range l.Candidates {
		if l.Candidates[i].Pick {
			pick = &l.Candidates[i]
			break
		}
	}
	if lerr != nil || pick == nil {
		c.Close()
		fmt.Fprintf(os.Stderr, "%s: command not found\n", cmd)
		os.Exit(127)
	}
	others := len(l.Candidates) - 1

	interactive := isatty(os.Stdin.Fd()) && isatty(os.Stderr.Fd())
	if !interactive {
		c.Close()
		fmt.Fprintln(os.Stderr, suggestLine(cmd, pick.Name, licenceNote(pick)))
		os.Exit(127)
	}

	fmt.Fprint(os.Stderr, promptLine(cmd, pick.Name, licenceNote(pick), others))
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	if !isYes(line) {
		c.Close()
		os.Exit(127)
	}
	if err := runInstall(ctx, c, []string{cmd}, os.Stderr); err != nil {
		c.Close()
		return err
	}
	c.Close()
	return execCommand(cmd, cmdArgs)
}

func promptLine(cmd, pkg, lic string, others int) string {
	s := fmt.Sprintf("fev: install package '%s'%s for '%s'? [y/N] ", pkg, lic, cmd)
	if others > 0 {
		s = fmt.Sprintf("fev: install package '%s'%s for '%s'? [y/N] (galley which %s lists %d others) ", pkg, lic, cmd, cmd, others)
	}
	return s
}

func suggestLine(cmd, pkg, lic string) string {
	return fmt.Sprintf("fev: %s is in package '%s'%s; galley install %s adds it", cmd, pkg, lic, cmd)
}

func licenceNote(c *debugstore.Candidate) string {
	if c.License == "" {
		if c.Unfree {
			return " (unfree)"
		}
		return ""
	}
	if c.Unfree {
		return " (" + c.License + ", unfree)"
	}
	return " (" + c.License + ")"
}

func isYes(line string) bool {
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}
