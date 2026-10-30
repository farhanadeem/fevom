package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"fev/host/cli"
	"fev/host/privhelper"
	api "fev/proto/privhelper"
)

func TestTheAdminInstallerIsTheHelpersOwn(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, _ := filepath.EvalSymlinks(t.TempDir())
	src := filepath.Join(t.TempDir(), "fev-privhelper")
	os.WriteFile(src, []byte("#!/bin/sh\n"), 0o755)
	var ran []string
	launchctl := func(args ...string) error {
		ran = append(ran, strings.Join(args, " "))
		return nil
	}
	base, base6, err := admin{}.Plan(home, os.Getuid())
	if err != nil || base != "198.19.0.0/16" || base6 != "" {
		t.Fatalf("the plan of a home with no configuration: %q %q %v", base, base6, err)
	}
	r, err := admin{}.Install(cli.AdminInstall{Root: root, OwnerUID: os.Getuid(), Source: src, UID: os.Getuid(), Home: home,
		Base: base, ResolverPort: 15353, Build: "test", Launchctl: launchctl})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/Library/PrivilegedHelperTools/dev.fevom.privhelper", "/Library/LaunchDaemons/dev.fevom.privhelper.plist",
		"/Library/Application Support/dev.fevom/privhelper/install.json"} {
		found := false
		for _, p := range r.Written {
			found = found || p == root+want
		}
		if _, err := os.Stat(root + want); !found || err != nil {
			t.Errorf("%s: listed %v, %v", want, found, err)
		}
	}
	entry := root + privhelper.StoreStandIn + "/fev.local.json"
	if _, err := os.Stat(entry); err != nil || !slices.Contains(r.Written, api.StoreKey("fev.local")) {
		t.Errorf("fev.local's store entry: listed %v, %v", slices.Contains(r.Written, api.StoreKey("fev.local")), err)
	}
	if _, err := os.Stat(root + api.ResolverPath); err == nil {
		t.Errorf("the install wrote %s: the store form writes no resolver file", api.ResolverPath)
	}
	if len(ran) == 0 || !strings.HasPrefix(ran[len(ran)-1], "bootstrap system "+root) {
		t.Errorf("launchctl: %q", ran)
	}
	u, err := admin{}.Undo(root, launchctl)
	if err != nil || len(u.Removed) == 0 {
		t.Fatalf("undo: %+v %v", u, err)
	}
	if _, err := os.Stat(root + "/Library/PrivilegedHelperTools/dev.fevom.privhelper"); err == nil {
		t.Error("the undo left the helper")
	}
	if _, err := os.Stat(entry); err == nil || !slices.Contains(u.Removed, api.StoreKey("fev.local")) {
		t.Errorf("the undo left fev.local's store entry (removed %q)", u.Removed)
	}
}
