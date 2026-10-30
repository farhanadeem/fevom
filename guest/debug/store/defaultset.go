package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"fev/proto/debugstore"
)

func ParseDefaultSet(path string) (id string, paths []string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "#") {
			if i := strings.Index(t, "set "); i >= 0 {
				id = strings.TrimSpace(t[i+len("set "):])
			}
			continue
		}
		paths = append(paths, t)
	}
	return id, paths, nil
}

func (s *Store) DefaultSetState(lockPath string) (id string, whole bool) {
	id, paths, err := ParseDefaultSet(lockPath)
	if err != nil {
		return "", false
	}
	whole = true
	for _, p := range paths {
		if s.registered(hashOf(base(p))) == nil {
			whole = false
			break
		}
		if _, err := os.Lstat(s.storePath(base(p))); err != nil {
			whole = false
			break
		}
	}
	return id, whole && len(paths) > 0
}

func (s *Store) DefaultPaths(lockPath string) map[string]bool {
	_, paths, err := ParseDefaultSet(lockPath)
	if err != nil {
		return nil
	}
	m := map[string]bool{}
	for _, p := range paths {
		m[p] = true
	}
	return m
}

func (s *Store) InstallDefaultSet(ctx context.Context, lockPath string, progress func(debugstore.Progress)) (*debugstore.Installed, error) {
	id, paths, err := ParseDefaultSet(lockPath)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lockRoot()
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.clearTmp()
	roots, err := s.resolveNames(ctx, paths)
	if err != nil {
		return nil, err
	}
	roots, err = s.fetchClosure(ctx, roots, progress)
	if err != nil {
		return nil, err
	}
	old := s.defaultRoots()
	newSet := map[string]bool{}
	var list []string
	for _, r := range roots {
		newSet[StoreDir+"/"+r] = true
	}
	for _, sp := range s.manifest() {
		if !old[sp] && !newSet[sp] {
			list = append(list, sp)
		}
	}
	for _, r := range roots {
		list = append(list, StoreDir+"/"+r)
	}
	if err := s.writeManifest(list); err != nil {
		return nil, err
	}
	var set []string
	for _, r := range roots {
		set = append(set, StoreDir+"/"+r)
	}
	if err := writeFileAtomic(filepath.Join(s.profile(), defaultSetFile), []byte(strings.Join(set, "\n")+"\n")); err != nil {
		return nil, err
	}
	writeFileAtomic(filepath.Join(s.profile(), defaultIDFile), []byte(id+"\n"))
	if err := s.relink(list); err != nil {
		return nil, err
	}
	if _, err := s.gcLocked(); err != nil {
		return nil, err
	}
	return s.installed(roots), nil
}
