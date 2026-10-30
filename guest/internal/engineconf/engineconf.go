package engineconf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type Base struct {
	HostGateway string
	Bip         string
	Pools       []Pool
	Pool6       Pool
	Runtime     string
	Containerd  string
}

const RuntimeName = "fev-runc"

type Pool struct {
	Base string `json:"base"`
	Size int    `json:"size"`
}

const cgroupDriver = "native.cgroupdriver="

func isCgroupDriver(opt string) bool {
	name, _, _ := strings.Cut(opt, "=")
	return strings.EqualFold(strings.TrimSpace(name), strings.TrimSuffix(cgroupDriver, "="))
}

var Reserved = map[string]string{
	"hosts":               "the engine's socket",
	"data-root":           "where the engine keeps its data",
	"exec-root":           "the engine's runtime state",
	"pidfile":             "the engine's pid file",
	"init-path":           "docker-init",
	"userland-proxy-path": "docker-proxy",
	"host-gateway-ip":     "the address host.docker.internal names",
	"host-gateway-ips":    "the address host.docker.internal names",
	"containerd":          "the engine's own containerd",
	"cgroup-parent":       "the engine's cgroup",
}

type Refused struct{ Message string }

func (r *Refused) Error() string { return r.Message }

func refuse(key string) error {
	return &Refused{Message: key + " is set by fev and cannot be changed in docker.json"}
}

func Check(user map[string]json.RawMessage) error {
	keys := make([]string, 0, len(user))
	for k := range user {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := Reserved[k]; ok {
			return refuse(k)
		}
	}
	if raw, ok := user["exec-opts"]; ok {
		var opts []string
		if err := json.Unmarshal(raw, &opts); err != nil {
			return &Refused{Message: "exec-opts must be a list of strings"}
		}
		for _, o := range opts {
			if isCgroupDriver(o) {
				return refuse("the cgroup driver (exec-opts " + cgroupDriver + ")")
			}
		}
	}
	return nil
}

func storageDriver(user map[string]json.RawMessage) string {
	var features map[string]bool
	if json.Unmarshal(user["features"], &features) == nil {
		if on, set := features["containerd-snapshotter"]; set && !on {
			return "overlay2"
		}
	}
	var remap string
	if json.Unmarshal(user["userns-remap"], &remap) == nil && remap != "" {
		return "overlay2"
	}
	return "overlayfs"
}

func Canonical(user map[string]json.RawMessage) string {
	if len(user) == 0 {
		return "{}"
	}
	var generic map[string]any
	data, _ := json.Marshal(user)
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return string(data)
	}
	out, _ := json.Marshal(generic)
	return string(out)
}

func Merge(base Base, user map[string]json.RawMessage) ([]byte, error) {
	if err := Check(user); err != nil {
		return nil, err
	}
	out := map[string]any{
		"hosts":               []string{"unix:///run/docker.sock"},
		"data-root":           "/var/lib/docker",
		"exec-root":           "/run/docker",
		"pidfile":             "/run/docker.pid",
		"init-path":           "/usr/bin/docker-init",
		"userland-proxy-path": "/usr/bin/docker-proxy",
		"host-gateway-ips":    []string{base.HostGateway},
		"storage-driver":      storageDriver(user),
		"default-ulimits": map[string]any{
			"nofile": map[string]any{"Name": "nofile", "Soft": 1024, "Hard": 524288},
		},
		"allow-direct-routing": true,
	}
	if base.Containerd != "" {
		out["containerd"] = base.Containerd
	}
	if base.Bip != "" {
		out["bip"] = base.Bip
	}
	pools := base.Pools
	if base.Pool6.Base != "" {
		pools = append(append([]Pool{}, pools...), base.Pool6)
	}
	if len(pools) > 0 {
		out["default-address-pools"] = pools
	}
	runtimes := map[string]any{}
	if base.Runtime != "" {
		runtimes[RuntimeName] = map[string]any{"path": base.Runtime}
		out["default-runtime"] = RuntimeName
	}
	opts := []string{cgroupDriver + "cgroupfs"}
	for k, v := range user {
		switch k {
		case "exec-opts":
			var more []string
			if err := json.Unmarshal(v, &more); err != nil {
				return nil, &Refused{Message: "exec-opts must be a list of strings"}
			}
			opts = append(opts, more...)
			continue
		case "runtimes":
			var theirs map[string]json.RawMessage
			if err := json.Unmarshal(v, &theirs); err != nil {
				return nil, &Refused{Message: "runtimes must be an object"}
			}
			if _, ok := theirs[RuntimeName]; ok && base.Runtime != "" {
				return nil, refuse("the runtime " + RuntimeName)
			}
			for name, r := range theirs {
				runtimes[name] = r
			}
			continue
		}
		out[k] = v
	}
	if len(runtimes) > 0 {
		out["runtimes"] = runtimes
	}
	out["exec-opts"] = opts
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("engine configuration: %w", err)
	}
	return append(data, '\n'), nil
}
