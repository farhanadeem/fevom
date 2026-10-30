package machines

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "fev/proto/agent"
)

func TestAStartRunsTheToolOnlyWhenTheSetChanged(t *testing.T) {
	root := t.TempDir()
	tool := filepath.Join(root, "usr/sbin/update-ca-certificates")
	os.MkdirAll(filepath.Dir(tool), 0o755)
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho run >> \"$(dirname \"$0\")/../../ran\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runs := func() int {
		b, _ := os.ReadFile(filepath.Join(root, "ran"))
		return strings.Count(string(b), "run")
	}
	a, b := []byte("root a"), []byte("root b")
	file := func(d []byte) string { return filepath.Join(root, localCertificates, trustFileName(d, ".crt")) }
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	if ran, err := installTrust(root, nil); err != nil || ran || exists(filepath.Join(root, TrustedFile)) {
		t.Fatalf("an empty set on a machine that never had one: ran %v, %v", ran, err)
	}
	if ran, err := installTrust(root, [][]byte{a, b}); err != nil || !ran || runs() != 1 || !exists(file(a)) || !exists(file(b)) {
		t.Fatalf("a new set: ran %v, %v, runs %d", ran, err, runs())
	}
	if d, _ := os.ReadFile(filepath.Join(root, TrustedFile)); strings.TrimSpace(string(d)) != api.TrustDigest([][]byte{a, b}) {
		t.Errorf("the digest kept: %q", d)
	}
	os.WriteFile(filepath.Join(root, localCertificates, "corp.crt"), []byte("theirs"), 0o644)
	if ran, err := installTrust(root, [][]byte{b, a}); err != nil || ran || runs() != 1 {
		t.Fatalf("the same set again: ran %v, %v, runs %d", ran, err, runs())
	}
	if ran, err := installTrust(root, [][]byte{b}); err != nil || !ran || runs() != 2 || exists(file(a)) || !exists(file(b)) {
		t.Fatalf("a root removed: ran %v, %v, runs %d", ran, err, runs())
	}
	if ran, err := installTrust(root, nil); err != nil || !ran || runs() != 3 || exists(file(b)) || !exists(filepath.Join(root, localCertificates, "corp.crt")) {
		t.Fatalf("the set emptied: ran %v, %v, runs %d", ran, err, runs())
	}
	if ran, err := installTrust(root, nil); err != nil || ran || runs() != 3 {
		t.Fatalf("empty again: ran %v, %v", ran, err)
	}

	bare := t.TempDir()
	if ran, err := installTrust(bare, [][]byte{a}); err == nil || ran {
		t.Fatalf("no tool: ran %v, %v", ran, err)
	}
	if !exists(filepath.Join(bare, localCertificates, trustFileName(a, ".crt"))) || exists(filepath.Join(bare, TrustedFile)) {
		t.Error("with no tool: the files are not left for a tool installed later, or a digest was kept")
	}
}
