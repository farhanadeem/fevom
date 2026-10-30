package debugremote

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ContextInfo struct {
	Name        string
	Host        string
	Description string
	Current     bool
}

const DefaultSocket = "unix:///var/run/docker.sock"

type contextMeta struct {
	Name     string `json:"Name"`
	Metadata struct {
		Description string `json:"Description"`
	} `json:"Metadata"`
	Endpoints map[string]struct {
		Host string `json:"Host"`
	} `json:"Endpoints"`
}

func Contexts(config, defaultHost string) ([]ContextInfo, error) {
	if defaultHost == "" {
		defaultHost = DefaultSocket
	}
	current := "default"
	if b, err := os.ReadFile(filepath.Join(config, "config.json")); err == nil {
		var cfg struct {
			CurrentContext string `json:"currentContext"`
		}
		if json.Unmarshal(b, &cfg) == nil && cfg.CurrentContext != "" {
			current = cfg.CurrentContext
		}
	}
	out := []ContextInfo{{
		Name:        "default",
		Host:        defaultHost,
		Description: "the current DOCKER_HOST configuration",
		Current:     current == "default",
	}}
	metaDir := filepath.Join(config, "contexts", "meta")
	entries, err := os.ReadDir(metaDir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	var named []ContextInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(metaDir, e.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var m contextMeta
		if json.Unmarshal(b, &m) != nil || m.Name == "" || m.Name == "default" {
			continue
		}
		host := ""
		if ep, ok := m.Endpoints["docker"]; ok {
			host = ep.Host
		}
		named = append(named, ContextInfo{
			Name: m.Name, Host: host, Description: strings.TrimSpace(m.Metadata.Description),
			Current: m.Name == current,
		})
	}
	sort.Slice(named, func(i, j int) bool { return named[i].Name < named[j].Name })
	return append(out, named...), nil
}

func Lookup(config, defaultHost, name string) (ContextInfo, bool) {
	list, _ := Contexts(config, defaultHost)
	if name == "" {
		name = "default"
	}
	for _, c := range list {
		if c.Name == name {
			return c, true
		}
	}
	return ContextInfo{}, false
}
