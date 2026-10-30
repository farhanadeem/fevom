package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRealCacheInstall(t *testing.T) {
	if os.Getenv("FEV_DEBUG_NET") != "1" {
		t.Skip("set FEV_DEBUG_NET=1 to fetch from the real cache and index")
	}
	s, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		filepath.WalkDir(s.Root(), func(q string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				os.Chmod(q, 0o755)
			}
			return nil
		})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	l, err := s.Lookup(ctx, "jq")
	if err != nil {
		t.Fatalf("index lookup: %v", err)
	}
	var pick string
	for _, c := range l.Candidates {
		if c.Pick {
			pick = c.StorePath
		}
	}
	if pick == "" {
		t.Fatalf("index gave no pick for jq: %+v", l.Candidates)
	}
	t.Logf("jq picks %s", pick)

	start := time.Now()
	got, err := s.Install(ctx, []string{"jq"}, nil)
	if err != nil {
		t.Fatalf("install jq from the real cache: %v", err)
	}
	took := time.Since(start)
	t.Logf("installed jq in %s: %d packages, commands %v", took.Round(time.Millisecond), len(got.Installed), got.Commands)
	found := false
	for _, p := range got.Installed {
		if _, err := os.Stat(filepath.Join(s.storePath(base(p.StorePath)), "bin", "jq")); err == nil {
			found = true
		}
	}
	if !found {
		if _, err := os.Lstat(filepath.Join(s.profile(), "bin", "jq")); err != nil {
			t.Fatalf("bin/jq not installed")
		}
	}
	s2 := time.Now()
	if _, err := s.Install(ctx, []string{"jq"}, nil); err != nil {
		t.Fatalf("reinstall jq: %v", err)
	}
	t.Logf("reinstall (shared) in %s", time.Since(s2).Round(time.Millisecond))
}

func TestRealCacheColdInstalls(t *testing.T) {
	if os.Getenv("FEV_DEBUG_NET") != "1" {
		t.Skip("set FEV_DEBUG_NET=1 to fetch from the real cache and index")
	}
	tools := []string{"curl", "strace", "tcpdump", "jq", "dig"}
	for _, tool := range tools {
		tool := tool
		t.Run(tool, func(t *testing.T) {
			root := t.TempDir()
			t.Cleanup(func() {
				filepath.WalkDir(root, func(q string, d os.DirEntry, err error) error {
					if err == nil && d.IsDir() {
						os.Chmod(q, 0o755)
					}
					return nil
				})
			})
			s, err := Open(root, "")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			start := time.Now()
			got, err := s.Install(ctx, []string{tool}, nil)
			if err != nil {
				t.Fatalf("install %s: %v", tool, err)
			}
			took := time.Since(start)
			bytes, paths := s.Bytes()
			t.Logf("%s: %s, %d paths, %d MB", tool, took.Round(time.Millisecond), paths, bytes/1e6)
			if took > 10*time.Second {
				t.Errorf("%s took %s, over the 10 s bar", tool, took)
			}
			if len(got.Commands) == 0 {
				t.Errorf("%s installed no commands", tool)
			}
		})
	}
}
