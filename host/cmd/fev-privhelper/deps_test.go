package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestTheHelperLinksOnlyWhatItNeeds(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output()
	if err != nil {
		t.Skip("go list:", err)
	}
	allowed := map[string]bool{
		"fev/host/cmd/fev-privhelper": true,
		"fev/host/privhelper":         true,
		"fev/host/nat64":              true,
		"fev/proto/privhelper":        true,
		"fev/proto/netplan":           true,
		"fev/proto/version":           true,
		"fev/proto/contract":          true,
		"golang.org/x/sys/unix":       true,
	}
	var deps []string
	for _, p := range strings.Fields(string(out)) {
		deps = append(deps, p)
		if !allowed[p] {
			t.Errorf("fev-privhelper links %s", p)
		}
	}
	t.Logf("%s", strings.Join(deps, " "))
}
