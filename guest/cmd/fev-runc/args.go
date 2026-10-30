package main

import (
	"strings"
)

const RootFile = "/run/fev/https/ca.pem"

var valued = map[string]bool{"--root": true, "--log": true, "--log-format": true, "--criu": true, "--rootless": true}

type call struct {
	args           []string
	verb           string
	bundle, id     string
	log, logFormat string
	root           string
}

func parse(argv []string) call {
	c := call{root: RootFile}
	for _, a := range argv {
		if v, ok := strings.CutPrefix(a, "--fev-root="); ok {
			c.root = v
			continue
		}
		c.args = append(c.args, a)
	}
	i := 0
	for i < len(c.args) && strings.HasPrefix(c.args[i], "-") {
		a := c.args[i]
		value := ""
		if key, v, ok := strings.Cut(a, "="); ok {
			a, value = key, v
		} else if valued[a] && i+1 < len(c.args) {
			value = c.args[i+1]
		}
		switch a {
		case "--log":
			c.log = value
		case "--log-format":
			c.logFormat = value
		}
		if valued[c.args[i]] && !strings.Contains(c.args[i], "=") {
			i++
		}
		i++
	}
	if i >= len(c.args) {
		return c
	}
	c.verb = c.args[i]
	if c.verb != "create" && c.verb != "run" {
		return c
	}
	c.bundle = "."
	rest := c.args[i+1:]
	for j := 0; j < len(rest); j++ {
		a := rest[j]
		switch {
		case (a == "--bundle" || a == "-b") && j+1 < len(rest):
			c.bundle = rest[j+1]
			j++
		case strings.HasPrefix(a, "--bundle="):
			c.bundle = strings.TrimPrefix(a, "--bundle=")
		case strings.HasPrefix(a, "-"):
			if !strings.Contains(a, "=") && (a == "--console-socket" || a == "--pid-file" || a == "--preserve-fds") && j+1 < len(rest) {
				j++
			}
		default:
			c.id = a
		}
	}
	return c
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return !strings.HasPrefix(id, ".")
}
