package fakeguest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

type fakeNetwork struct {
	ID, Name, Driver, Scope string
	Subnet, Gateway         string
	Internal, Attachable    bool
	Options, Labels         map[string]string
	Created                 time.Time
}

var predefined = []fakeNetwork{
	{ID: "bb65ef7ca734" + strings.Repeat("0", 52), Name: "bridge", Driver: "bridge", Scope: "local", Subnet: "172.17.0.0/16", Gateway: "172.17.0.1",
		Options: map[string]string{
			"com.docker.network.bridge.default_bridge": "true", "com.docker.network.bridge.enable_icc": "true",
			"com.docker.network.bridge.enable_ip_masquerade": "true", "com.docker.network.bridge.host_binding_ipv4": "0.0.0.0",
			"com.docker.network.bridge.name": "docker0", "com.docker.network.driver.mtu": "1500",
		}},
	{ID: "4e1d2c0f5a11" + strings.Repeat("0", 52), Name: "host", Driver: "host", Scope: "local"},
	{ID: "9a0b3e7d6c22" + strings.Repeat("0", 52), Name: "none", Driver: "null", Scope: "local"},
}

func (e *Engine) networksLocked() map[string]*fakeNetwork {
	if e.networks == nil {
		e.networks = map[string]*fakeNetwork{}
		for _, n := range predefined {
			n.Created = time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
			e.networks[n.ID] = &n
		}
	}
	return e.networks
}

func (e *Engine) findNetworkLocked(ref string) *fakeNetwork {
	for _, n := range e.networksLocked() {
		if n.Name == ref || n.ID == ref || (len(ref) >= 3 && strings.HasPrefix(n.ID, ref)) {
			return n
		}
	}
	return nil
}

func (n *fakeNetwork) api() map[string]any {
	cfg := []map[string]string{}
	if n.Subnet != "" {
		cfg = append(cfg, map[string]string{"Subnet": n.Subnet, "Gateway": n.Gateway})
	}
	opts, labels := n.Options, n.Labels
	if opts == nil {
		opts = map[string]string{}
	}
	if labels == nil {
		labels = map[string]string{}
	}
	return map[string]any{
		"Name": n.Name, "Id": n.ID, "Created": n.Created.Format(time.RFC3339Nano), "Scope": n.Scope, "Driver": n.Driver,
		"EnableIPv4": true, "EnableIPv6": false, "IPAM": map[string]any{"Driver": "default", "Options": map[string]string{}, "Config": cfg},
		"Internal": n.Internal, "Attachable": n.Attachable, "Ingress": false, "ConfigOnly": false,
		"Containers": map[string]any{}, "Options": opts, "Labels": labels,
	}
}

func (e *Engine) networkRoute(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
	case path == "/networks" && r.Method == http.MethodGet:
		e.mu.Lock()
		out := []map[string]any{}
		for _, n := range e.networksLocked() {
			out = append(out, n.api())
		}
		e.mu.Unlock()
		sort.Slice(out, func(i, j int) bool { return out[i]["Name"].(string) < out[j]["Name"].(string) })
		reply(w, out)
	case path == "/networks/create" && r.Method == http.MethodPost:
		var req struct {
			Name     string
			Driver   string
			Internal bool
			IPAM     struct {
				Config []struct{ Subnet, Gateway string }
			}
			Labels map[string]string
		}
		json.NewDecoder(r.Body).Decode(&req)
		e.mu.Lock()
		defer e.mu.Unlock()
		if req.Name == "" {
			fail(w, http.StatusBadRequest, "network name must be given")
			return true
		}
		if e.findNetworkLocked(req.Name) != nil {
			fail(w, http.StatusConflict, "network with name %s already exists", req.Name)
			return true
		}
		n := &fakeNetwork{ID: newID() + newID(), Name: req.Name, Driver: req.Driver, Scope: "local", Internal: req.Internal, Labels: req.Labels, Created: time.Now()}
		if n.Driver == "" {
			n.Driver = "bridge"
		}
		if len(req.IPAM.Config) > 0 {
			n.Subnet, n.Gateway = req.IPAM.Config[0].Subnet, req.IPAM.Config[0].Gateway
		} else {
			k := len(e.networksLocked())
			n.Subnet, n.Gateway = fmt.Sprintf("192.168.%d.0/24", 90+k), fmt.Sprintf("192.168.%d.1", 90+k)
		}
		e.networksLocked()[n.ID] = n
		e.emit("network", "create", n.ID, map[string]string{"name": n.Name, "type": n.Driver})
		reply(w, map[string]any{"Id": n.ID, "Warning": ""})
	case strings.HasPrefix(path, "/networks/") && r.Method == http.MethodGet:
		e.mu.Lock()
		n := e.findNetworkLocked(strings.TrimPrefix(path, "/networks/"))
		var body map[string]any
		if n != nil {
			body = n.api()
		}
		e.mu.Unlock()
		if n == nil {
			fail(w, http.StatusNotFound, "network %s not found", strings.TrimPrefix(path, "/networks/"))
			return true
		}
		reply(w, body)
	case strings.HasPrefix(path, "/networks/") && r.Method == http.MethodDelete:
		ref := strings.TrimPrefix(path, "/networks/")
		e.mu.Lock()
		defer e.mu.Unlock()
		n := e.findNetworkLocked(ref)
		if n == nil {
			fail(w, http.StatusNotFound, "network %s not found", ref)
			return true
		}
		for _, p := range predefined {
			if p.ID == n.ID {
				fail(w, http.StatusForbidden, "%s is a pre-defined network and cannot be removed", n.Name)
				return true
			}
		}
		delete(e.networksLocked(), n.ID)
		e.emit("network", "destroy", n.ID, map[string]string{"name": n.Name, "type": n.Driver})
		w.WriteHeader(http.StatusNoContent)
	default:
		return false
	}
	return true
}
