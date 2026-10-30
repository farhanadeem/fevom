package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

func Fake(t *testing.T, dir string, files ...string) string {
	t.Helper()
	c := filepath.Join(dir, "Fevom.app", "Contents")
	for _, f := range append([]string{"Info.plist"}, files...) {
		p := filepath.Join(c, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestAProgramOfTheBundleFindsItsPeersInTheirPlaces(t *testing.T) {
	c := Fake(t, t.TempDir(), "MacOS/Fevom", "MacOS/bin/fev", "MacOS/xbin/docker",
		"Helpers/fevd", "Helpers/fevvm", "Helpers/fev-privhelper")
	cli := filepath.Join(c, "MacOS/bin/fev")
	fevd := filepath.Join(c, "Helpers/fevd")
	for _, tc := range []struct{ exe, name, want string }{
		{cli, "fevd", fevd},
		{cli, "fev-privhelper", filepath.Join(c, "Helpers/fev-privhelper")},
		{fevd, "fevvm", filepath.Join(c, "Helpers/fevvm")},
		{fevd, "fev", cli},
		{filepath.Join(c, "MacOS/Fevom"), "fevd", fevd},
		{cli, "fev-keysigner", ""},
	} {
		if got := Peer(tc.exe, tc.name); got != tc.want {
			t.Errorf("Peer(%s, %s) = %q, want %q", tc.exe, tc.name, got, tc.want)
		}
	}
	if got := Tools(cli); got != filepath.Join(c, "MacOS/xbin") {
		t.Errorf("Tools = %q", got)
	}
	if got := Contents(fevd); got != c {
		t.Errorf("Contents = %q", got)
	}
}

func TestTheBuildTreeIsNoBundle(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "host", "out")
	os.MkdirAll(out, 0o755)
	for _, n := range []string{"fev", "fevd"} {
		os.WriteFile(filepath.Join(out, n), nil, 0o755)
	}
	if got := Peer(filepath.Join(out, "fev"), "fevd"); got != filepath.Join(out, "fevd") {
		t.Errorf("Peer = %q", got)
	}
	if Contents(filepath.Join(out, "fev")) != "" || Tools(filepath.Join(out, "fev")) != "" {
		t.Error("the build tree was taken for a bundle")
	}
	c := filepath.Join(dir, "x", "Contents", "MacOS", "bin")
	os.MkdirAll(c, 0o755)
	if Contents(filepath.Join(c, "fev")) != "" {
		t.Error("a Contents without Info.plist was taken for a bundle")
	}
	b := Fake(t, t.TempDir(), "MacOS/bin/fev")
	if Tools(filepath.Join(b, "MacOS/bin/fev")) != "" {
		t.Error("tools from a bundle that has none")
	}
}
