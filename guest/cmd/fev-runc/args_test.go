package main

import (
	"slices"
	"testing"
)

func TestTheCommandLine(t *testing.T) {
	for _, c := range []struct {
		argv             []string
		verb, bundle, id string
		root             string
		args             []string
	}{
		{[]string{"--root", "/run/containerd/runc/moby", "--log", "/x/log.json", "--log-format", "json", "create", "--bundle", "/run/containerd/b", "--pid-file", "/run/p", "abc"},
			"create", "/run/containerd/b", "abc", RootFile,
			[]string{"--root", "/run/containerd/runc/moby", "--log", "/x/log.json", "--log-format", "json", "create", "--bundle", "/run/containerd/b", "--pid-file", "/run/p", "abc"}},
		{[]string{"--fev-root=/tmp/r.pem", "create", "-b", "/b", "--console-socket", "/s.sock", "id2"}, "create", "/b", "id2", "/tmp/r.pem",
			[]string{"create", "-b", "/b", "--console-socket", "/s.sock", "id2"}},
		{[]string{"create", "--bundle=/b3", "id3"}, "create", "/b3", "id3", RootFile, []string{"create", "--bundle=/b3", "id3"}},
		{[]string{"--root", "/r", "start", "abc"}, "start", "", "", RootFile, []string{"--root", "/r", "start", "abc"}},
		{[]string{"--version"}, "", "", "", RootFile, []string{"--version"}},
	} {
		got := parse(c.argv)
		if got.verb != c.verb || got.bundle != c.bundle || got.id != c.id || got.root != c.root || !slices.Equal(got.args, c.args) {
			t.Errorf("%q: %+v", c.argv, got)
		}
	}
	for id, ok := range map[string]bool{"3f2a9c41d7e0": true, "": false, "../x": false, "a/b": false, ".hidden": false} {
		if validID(id) != ok {
			t.Errorf("validID(%q)", id)
		}
	}
}
