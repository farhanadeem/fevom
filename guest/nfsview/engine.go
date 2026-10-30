package nfsview

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

type Follower struct {
	Objects  *Objects
	Dial     func(ctx context.Context) (net.Conn, error)
	EngineNS func() (path string, done func(), ok bool)
	Bundle   func(id string) (pid int, dests []string, err error)
	DataRoot string
	Logf     func(string, ...any)

	mu    sync.Mutex
	names map[string]string
	diffs map[string][]string
	http  *http.Client
}

const engineDataRoot = "/var/lib/docker"

func (f *Follower) logf(format string, args ...any) {
	if f.Logf != nil {
		f.Logf(format, args...)
	}
}

func (f *Follower) client() *http.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.http == nil {
		f.http = &http.Client{Transport: &http.Transport{
			DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return f.Dial(ctx) },
			DisableKeepAlives: true,
		}}
		f.names = map[string]string{}
		f.diffs = map[string][]string{}
	}
	return f.http
}

func (f *Follower) get(ctx context.Context, p string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, apiWait)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://engine"+p, nil)
	resp, err := f.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("nfsview: %s: %s: %s", p, resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

const apiWait = 10 * time.Second

func (f *Follower) Name(ctx context.Context, id string) string {
	f.client()
	f.mu.Lock()
	n, ok := f.names[id]
	f.mu.Unlock()
	if ok {
		return n
	}
	var cs []dockerContainer
	q := url.Values{"all": {"1"}, "filters": {`{"id":["` + id + `"]}`}}
	if err := f.get(ctx, "/containers/json?"+q.Encode(), &cs); err != nil {
		f.logf("the name of %.12s: %v", id, err)
		return ""
	}
	for _, c := range cs {
		if c.ID == id {
			n = c.name()
			f.mu.Lock()
			f.names[id] = n
			f.mu.Unlock()
			return n
		}
	}
	return ""
}

type dockerContainer struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	State string   `json:"State"`
}

func (c dockerContainer) name() string {
	for _, n := range c.Names {
		if t := strings.TrimPrefix(n, "/"); !strings.Contains(t, "/") {
			return t
		}
	}
	return c.ID[:min(12, len(c.ID))]
}

type dockerVolume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	Options    map[string]string `json:"Options"`
}

type dockerImage struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
}

type dockerEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

func (f *Follower) Run(ctx context.Context) error {
	q := url.Values{"filters": {`{"type":["container","image","volume"]}`}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://engine/events?"+q.Encode(), nil)
	resp, err := f.client().Do(req)
	if err != nil {
		return fmt.Errorf("nfsview: the engine's events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("nfsview: the engine's events: %s", resp.Status)
	}
	f.Reconcile(ctx)
	s := bufio.NewScanner(resp.Body)
	s.Buffer(make([]byte, 64<<10), 1<<20)
	for s.Scan() {
		var ev dockerEvent
		if json.Unmarshal(s.Bytes(), &ev) != nil {
			continue
		}
		f.event(ctx, ev)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := s.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (f *Follower) event(ctx context.Context, ev dockerEvent) {
	id := ev.Actor.ID
	switch ev.Type {
	case "container":
		name := strings.TrimPrefix(ev.Actor.Attributes["name"], "/")
		switch ev.Action {
		case "create":
			f.setName(id, name)
		case "rename":
			f.setName(id, name)
			f.Objects.Rename(id, name)
		case "start":
			f.setName(id, name)
			if _, shown := f.Objects.Containers()[id]; !shown {
				f.attachFound(ctx, id, name)
			}
		case "destroy":
			f.Objects.Release(id)
			f.Objects.Forget(id)
			f.mu.Lock()
			delete(f.names, id)
			f.mu.Unlock()
		}
	case "volume":
		switch ev.Action {
		case "create", "destroy":
			f.syncVolumes(ctx)
		}
	case "image":
		switch ev.Action {
		case "pull", "tag", "untag", "delete", "import", "load", "create":
			f.syncImages(ctx)
		}
	}
}

func (f *Follower) setName(id, name string) {
	if name == "" {
		return
	}
	f.client()
	f.mu.Lock()
	f.names[id] = name
	f.mu.Unlock()
}

func (f *Follower) Reconcile(ctx context.Context) {
	var cs []dockerContainer
	if err := f.get(ctx, "/containers/json?all=1", &cs); err != nil {
		f.logf("reconcile: the containers: %v", err)
	} else {
		state := map[string]string{}
		f.client()
		f.mu.Lock()
		f.names = map[string]string{}
		for _, c := range cs {
			f.names[c.ID] = c.name()
			state[c.ID] = c.State
		}
		f.mu.Unlock()
		for id := range f.Objects.Containers() {
			switch state[id] {
			case "running", "paused", "restarting", "created", "removing":
			default:
				f.Objects.Release(id)
			}
		}
		for _, c := range cs {
			if c.State != "running" && c.State != "paused" {
				continue
			}
			if _, shown := f.Objects.Containers()[c.ID]; !shown {
				f.attachFound(ctx, c.ID, c.name())
			}
		}
	}
	f.syncVolumes(ctx)
	f.syncImages(ctx)
}

func (f *Follower) attachFound(ctx context.Context, id, name string) {
	ns, done, ok := f.EngineNS()
	if !ok {
		return
	}
	defer done()
	attached, err := f.Objects.AttachFound(ns, id, name)
	if err != nil {
		f.logf("container %s: %v", name, err)
	}
	if !attached || f.Bundle == nil {
		return
	}
	pid, dests, err := f.Bundle(id)
	if err != nil {
		f.logf("container %s's binds: %v", name, err)
		return
	}
	if err := f.Objects.Binds(ns, id, pid, dests); err != nil {
		f.logf("container %s's binds: %v", name, err)
	}
}

func (f *Follower) syncVolumes(ctx context.Context) {
	var vs struct {
		Volumes []dockerVolume `json:"Volumes"`
	}
	if err := f.get(ctx, "/volumes", &vs); err != nil {
		f.logf("the volumes: %v", err)
		return
	}
	var specs []VolumeSpec
	for _, v := range vs.Volumes {
		data, ok := strings.CutPrefix(path.Clean(v.Mountpoint), engineDataRoot+"/")
		if v.Driver != "local" || !ok {
			continue
		}
		specs = append(specs, VolumeSpec{Name: v.Name, Data: path.Join(f.DataRoot, data)})
	}
	f.Objects.SyncVolumes(specs)
}

func (f *Follower) syncImages(ctx context.Context) {
	if f.Objects.images == nil {
		return
	}
	var is []dockerImage
	if err := f.get(ctx, "/images/json", &is); err != nil {
		f.logf("the images: %v", err)
		return
	}
	var specs []ImageSpec
	seen := map[string]bool{}
	for _, im := range is {
		seen[im.ID] = true
		f.mu.Lock()
		diffs, ok := f.diffs[im.ID]
		f.mu.Unlock()
		if !ok {
			var in struct {
				RootFS struct {
					Layers []string `json:"Layers"`
				} `json:"RootFS"`
			}
			if err := f.get(ctx, "/images/"+url.PathEscape(im.ID)+"/json", &in); err != nil {
				f.logf("image %.19s: %v", im.ID, err)
				continue
			}
			diffs = in.RootFS.Layers
			f.mu.Lock()
			f.diffs[im.ID] = diffs
			f.mu.Unlock()
		}
		if len(diffs) == 0 {
			continue
		}
		for _, n := range ImageNames(im.ID, im.RepoTags) {
			specs = append(specs, ImageSpec{ID: im.ID, Name: n, DiffIDs: diffs})
		}
	}
	f.mu.Lock()
	for id := range f.diffs {
		if !seen[id] {
			delete(f.diffs, id)
		}
	}
	f.mu.Unlock()
	f.Objects.SyncImages(specs)
}

func ImageNames(id string, tags []string) []string {
	var out []string
	for _, t := range tags {
		if t != "" && t != "<none>:<none>" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		short := strings.TrimPrefix(id, "sha256:")
		out = append(out, short[:min(12, len(short))])
	}
	sort.Strings(out)
	return out
}

type Mount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options"`
}

var macPaths = []string{"/Users", "/Volumes", "/private", "/Applications", "/Library", "/mnt/mac"}

func under(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

func BindDests(spec []byte) ([]string, error) {
	var s struct {
		Mounts []Mount `json:"mounts"`
	}
	if err := json.Unmarshal(spec, &s); err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range s.Mounts {
		d := path.Clean("/" + m.Destination)
		if d == "/" || under(d, "/dev") || under(d, "/proc") || under(d, "/sys") || seen[d] {
			continue
		}
		bind := m.Type == "bind"
		for _, o := range m.Options {
			if o == "bind" || o == "rbind" {
				bind = true
			}
		}
		switch {
		case bind:
			src := path.Clean(m.Source)
			if under(src, engineDataRoot+"/containers") {
				continue
			}
			mac := false
			for _, p := range macPaths {
				mac = mac || under(src, p)
			}
			if mac {
				continue
			}
		case m.Type == "tmpfs":
		default:
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out, nil
}
