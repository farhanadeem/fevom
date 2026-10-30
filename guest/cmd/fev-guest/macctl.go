//go:build linux

package main

import (
	"fmt"
	"io"
	"strings"

	"fev/proto/bridge"
)

var subcommands = []struct {
	name, args, about string
	run               func(prog string, args []string) int
}{
	{"run", "COMMAND [ARG...]", "run a command on the Mac", func(_ string, a []string) int { return runOnMac(a) }},
	{"link", "NAME...", "run the Mac's NAME by its name alone in every machine (root)", linkMain},
	{"unlink", "NAME...", "undo a link (root)", linkMain},
	{"push", "PATH [MACDEST]", "copy a file or directory to the Mac (default: the Mac's home)", pushMain},
	{"pull", "MACPATH [DEST]", "copy a file or directory from the Mac (default: here)", pullMain},
	{"notify", "[-t TITLE] MESSAGE...", "show a notification on the Mac", notifyMain},
}

func usage(w io.Writer, prog string) {
	if prog == "mac" {
		fmt.Fprintf(w, "Usage: mac COMMAND [ARG...]     run a Mac command in this directory\n       mac SUBCOMMAND [ARG...]\n\n")
	} else {
		fmt.Fprintf(w, "Usage: macctl SUBCOMMAND [ARG...]\n\n")
	}
	fmt.Fprintln(w, "Subcommands:")
	for _, s := range subcommands {
		fmt.Fprintf(w, "  %-8s %-22s %s\n", s.name, s.args, s.about)
	}
	if prog == "macctl" {
		fmt.Fprintf(w, "  %-8s %-22s %s\n", "completion", "bash|zsh|fish", "print a shell's completion script")
	}
	fmt.Fprintf(w, "\nLinked commands (/opt/fev/bin): %s, and what mac link adds.\n", strings.Join(bridge.Defaults, ", "))
}

func helpWord(s string) bool { return s == "-h" || s == "--help" || s == "help" }

func macMain(args []string) int {
	if len(args) == 0 || (len(args) == 1 && helpWord(args[0])) {
		usage(stdout, "mac")
		return 0
	}
	for _, s := range subcommands {
		if s.name == args[0] {
			return s.run("mac "+s.name, args[1:])
		}
	}
	return runOnMac(args)
}

func macctlMain(args []string) int {
	if len(args) == 0 || (len(args) == 1 && helpWord(args[0])) {
		usage(stdout, "macctl")
		return 0
	}
	if args[0] == "completion" {
		return completionMain(args[1:])
	}
	for _, s := range subcommands {
		if s.name == args[0] {
			return s.run("macctl "+s.name, args[1:])
		}
	}
	fmt.Fprintf(stderr, "macctl: no subcommand %q; a Mac command is run with: macctl run %s\n", args[0], args[0])
	return 2
}

func linkMain(prog string, args []string) int {
	verb := bridge.VerbLink
	if strings.HasSuffix(prog, "unlink") {
		verb = bridge.VerbUnlink
	}
	if len(args) == 0 || helpWord(args[0]) {
		fmt.Fprintf(stderr, "usage: %s NAME...\n", prog)
		return 2
	}
	if err := ask(&bridge.Header{Verb: verb, Names: args}); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return 1
	}
	return 0
}

func notifyMain(prog string, args []string) int {
	title := ""
	for len(args) > 0 {
		switch {
		case (args[0] == "-t" || args[0] == "--title") && len(args) > 1:
			title, args = args[1], args[2:]
			continue
		case strings.HasPrefix(args[0], "--title="):
			title, args = strings.TrimPrefix(args[0], "--title="), args[1:]
			continue
		case args[0] == "--":
			args = args[1:]
		}
		break
	}
	msg := strings.Join(args, " ")
	if msg == "" && title == "" || len(args) > 0 && helpWord(args[0]) {
		fmt.Fprintf(stderr, "usage: %s [-t TITLE] MESSAGE...\n", prog)
		return 2
	}
	if err := ask(&bridge.Header{Verb: bridge.VerbNotify, Title: title, Message: msg}); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return 1
	}
	return 0
}
