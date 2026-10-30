package machines

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheResolverDropInIsWrittenWhereThereIsResolved(t *testing.T) {
	for _, tc := range []struct {
		name, program string
		want          bool
	}{
		{"usr", "usr/lib/systemd/systemd-resolved", true},
		{"lib", "lib/systemd/systemd-resolved", true},
		{"none", "", false},
	} {
		root, run := t.TempDir(), t.TempDir()
		if tc.program != "" {
			p := filepath.Join(root, tc.program)
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, nil, 0o755)
		}
		if err := writeResolvedDropIn(root, run, ""); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		b, err := os.ReadFile(filepath.Join(run, "systemd/resolved.conf.d", DropInName))
		switch {
		case tc.want && (err != nil || string(b) != ResolvedDropIn(ResolverAddress)):
			t.Errorf("%s: %q %v", tc.name, b, err)
		case !tc.want && err == nil:
			t.Errorf("%s: written with no resolved: %q", tc.name, b)
		}
	}
	if want := "Domains=~fev.local\n"; !strings.Contains(ResolvedDropIn(ResolverAddress), want) {
		t.Errorf("drop-in %q", ResolvedDropIn(ResolverAddress))
	}
}

func TestTheResolverDropInNamesThePlansResolver(t *testing.T) {
	root, run := t.TempDir(), t.TempDir()
	p := filepath.Join(root, "usr/lib/systemd/systemd-resolved")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, nil, 0o755)
	if err := writeResolvedDropIn(root, run, "10.198.0.1"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(run, "systemd/resolved.conf.d", DropInName))
	if err != nil || !strings.Contains(string(b), "\nDNS=10.198.0.1\n") {
		t.Errorf("drop-in under 10.198.0.0/16: %q %v", b, err)
	}
}
