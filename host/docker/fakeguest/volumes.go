package fakeguest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

type VolumeData struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels,omitempty"`
	Files  map[string]string `json:"Files,omitempty"`
}

func (e *Engine) Volume(name string) (VolumeData, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.hasVolumeLocked(name) {
		return VolumeData{}, false
	}
	v := VolumeData{Name: name, Labels: map[string]string{}, Files: map[string]string{}}
	if d := e.volumeData[name]; d != nil {
		for k, x := range d.Labels {
			v.Labels[k] = x
		}
		for k, x := range d.Files {
			v.Files[k] = x
		}
	}
	return v, true
}

func (e *Engine) MakeVolume(v VolumeData) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.hasVolumeLocked(v.Name) {
		return fmt.Errorf("the Docker engine has a volume named %s", v.Name)
	}
	e.putVolumeLocked(v)
	e.emit("volume", "create", v.Name, map[string]string{"driver": "local"})
	return nil
}

func (e *Engine) hasVolumeLocked(name string) bool {
	if e.extraVolumes[name] {
		return true
	}
	for _, c := range e.containers {
		for _, x := range c.volumes {
			if x == name {
				return true
			}
		}
	}
	return false
}

func (e *Engine) putVolumeLocked(v VolumeData) {
	e.addVolume(v.Name)
	if e.volumeData == nil {
		e.volumeData = map[string]*VolumeData{}
	}
	e.volumeData[v.Name] = &v
}

func (e *Engine) VolumeNames() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for n := range e.volumeData {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (e *Engine) volumeRoute(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
	case path == "/_fake/volume-data" && r.Method == http.MethodPost:
		var v VolumeData
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil || v.Name == "" {
			fail(w, http.StatusBadRequest, "a volume's data needs a name: %v", err)
			return true
		}
		e.mu.Lock()
		e.putVolumeLocked(v)
		e.mu.Unlock()
		reply(w, map[string]string{})
	case len(path) > len("/_fake/volume-data/") && path[:len("/_fake/volume-data/")] == "/_fake/volume-data/" && r.Method == http.MethodGet:
		v, ok := e.Volume(path[len("/_fake/volume-data/"):])
		if !ok {
			fail(w, http.StatusNotFound, "get %s: no such volume", path[len("/_fake/volume-data/"):])
			return true
		}
		reply(w, v)
	default:
		return false
	}
	return true
}
