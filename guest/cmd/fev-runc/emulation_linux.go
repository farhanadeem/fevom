package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	policy "fev/guest/internal/emulation"
	api "fev/proto/agent"
)

func selectEmulation(c call, byName bool) error {
	var metadata struct {
		Name   string
		Config struct{ Labels map[string]string }
	}
	if validID(c.id) {
		if b, err := readLimited(filepath.Join(containersDir, c.id, "config.v2.json"), 4<<20); err == nil {
			_ = json.Unmarshal(b, &metadata)
		}
	}
	config, err := policy.Load(policy.ConfigPath)
	if errors.Is(err, os.ErrNotExist) && metadata.Config.Labels[policy.Label] == "" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load emulation policy: %w", err)
	}
	b, err := readLimited(filepath.Join(c.bundle, "config.json"), 4<<20)
	if err != nil {
		return err
	}
	var s spec
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	override := metadata.Config.Labels[policy.Label]
	if annotation := s.Annotations[policy.Label]; annotation != "" {
		if override != "" && override != annotation {
			return fmt.Errorf("conflicting %s label and annotation", policy.Label)
		}
		override = annotation
	}
	if config.Mode == policy.FEX && override == "" {
		return nil
	}
	root := s.Root.Path
	if root == "" {
		return errors.New("emulation: OCI spec has no root")
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(c.bundle, root)
	}
	facts, err := policy.Inspect(root, s.Process.Args, s.Process.Env)
	if err != nil {
		return fmt.Errorf("inspect emulation requirements: %w", err)
	}
	d, err := policy.Select(config, override, facts)
	if err != nil {
		return err
	}
	cg := strings.TrimPrefix(s.Linux.CgroupsPath, "/")
	if !filepath.IsLocal(cg) || strings.Contains(cg, ":") {
		return fmt.Errorf("emulation: unsupported cgroup path %q", s.Linux.CgroupsPath)
	}
	cgroup := filepath.Join("/sys/fs/cgroup", cg)
	if err := os.MkdirAll(cgroup, 0755); err != nil {
		return err
	}
	if err := policy.SetCgroup(cgroup, d.Engine); err != nil {
		return err
	}
	if facts.AMD64 || override != "" && override != policy.Auto {
		name := strings.TrimPrefix(metadata.Name, "/")
		if name == "" {
			name = c.id
		}
		kind := "container"
		if byName {
			kind = "build"
		}
		record := api.EmulationWorkload{ID: c.id, Kind: kind, Name: name, Engine: d.Engine, Reason: d.Reason}
		b, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(c.bundle, policy.RecordName), b, 0600); err != nil {
			return err
		}
	}
	return nil
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	return b, err
}
