package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"fev/host/daemon/home"
)

func TestOnlyTheUsersOwnDaemonManagesTheirDockerConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONFIG", "")
	t.Setenv("FEV_DOCKER_CONFIG", "")
	cases := []struct {
		name            string
		fakeVM, ownHome bool
		want            string
	}{
		{"the default home, a real VM", false, false, filepath.Join(home, ".docker")},
		{"a home of its own", false, true, ""},
		{"a stand-in VM", true, false, ""},
		{"both", true, true, ""},
	}
	for _, tc := range cases {
		if got := dockerConfig(tc.fakeVM, tc.ownHome); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	t.Setenv("DOCKER_CONFIG", "/tmp/their/real/config")
	if got := dockerConfig(false, false); got != "/tmp/their/real/config" {
		t.Errorf("the user's daemon with DOCKER_CONFIG set: %q", got)
	}
	for _, dev := range [][2]bool{{true, false}, {false, true}, {true, true}} {
		if got := dockerConfig(dev[0], dev[1]); got != "" {
			t.Errorf("a development daemon followed DOCKER_CONFIG to %q", got)
		}
	}
	t.Setenv("FEV_DOCKER_CONFIG", "/tmp/scratch/config")
	for _, any := range [][2]bool{{true, true}, {false, false}} {
		if got := dockerConfig(any[0], any[1]); got != "/tmp/scratch/config" {
			t.Errorf("with a directory named outright: %q", got)
		}
	}
}

func TestOnlyTheUsersOwnDaemonSharesTheMacUnasked(t *testing.T) {
	layout, err := home.Resolve("/tmp/bh.x", "")
	if err != nil {
		t.Fatal(err)
	}
	share, warning := shareOf(layout, "", false, false)
	if share == nil || warning != "" || share.Root != "/" || !reflect.DeepEqual(share.Deny, []string{"/tmp/bh.x/data", "/tmp/bh.x/run"}) {
		t.Fatalf("the user's own daemon: %+v %q", share, warning)
	}
	for _, dev := range [][2]bool{{true, false}, {false, true}, {true, true}} {
		if share, warning := shareOf(layout, "", dev[0], dev[1]); share != nil || warning != "" {
			t.Errorf("a development daemon shares %+v unasked (%q)", share, warning)
		}
		if share, _ := shareOf(layout, "/tmp/scratch/mac", dev[0], dev[1]); share == nil || share.Root != "/tmp/scratch/mac" {
			t.Errorf("a development daemon told what to share: %+v", share)
		}
	}
	comma, _ := home.Resolve("/tmp/bh,x", "")
	if share, warning := shareOf(comma, "", false, false); share != nil || !strings.Contains(warning, "not shared") {
		t.Errorf("a home with a comma in its path: %+v %q", share, warning)
	}
}
