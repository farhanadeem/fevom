package debugremote

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

type fakeEngine struct {
	srv *httptest.Server

	mu         sync.Mutex
	arch       string
	containers map[string]*fakeCtr
	images     map[string]bool
	imageLabel map[string]string
	volumes    map[string]bool

	loadCount   int
	created     []CreateConfig
	commits     []string
	removedImgs []string
	removedVols []string
	nextID      int
}

type fakeCtr struct {
	id, name, image string
	running, paused bool
	pid             int
	env             []string
	workdir, user   string
}

func newFakeEngine(arch string) *fakeEngine {
	f := &fakeEngine{arch: arch, containers: map[string]*fakeCtr{}, images: map[string]bool{},
		imageLabel: map[string]string{}, volumes: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeEngine) close() { f.srv.Close() }

func (f *fakeEngine) transport() *Transport {
	addr := f.srv.Listener.Addr().String()
	return &Transport{DialFunc: func(ctx context.Context) (net.Conn, error) {
		return net.Dial("tcp", addr)
	}}
}

func (f *fakeEngine) engine() *Engine { return NewEngine(f.transport()) }

func (f *fakeEngine) addRunning(name, image string, pid int) *fakeCtr {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := strings.Repeat("a", 60) + strings.Repeat("0", 4-len(itoa(f.nextID))) + itoa(f.nextID)
	c := &fakeCtr{id: id[:64], name: name, image: image, running: true, pid: pid, env: []string{"PATH=/usr/bin:/bin"}}
	f.containers[name] = c
	f.containers[c.id] = c
	return c
}
func (f *fakeEngine) addStopped(name, image string) *fakeCtr {
	c := f.addRunning(name, image, 0)
	c.running = false
	return c
}
func (f *fakeEngine) addImage(ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[ref] = true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func (f *fakeEngine) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if i := strings.Index(path, "/v1."); i == 0 {
		if j := strings.IndexByte(path[1:], '/'); j >= 0 {
			path = path[j+1:]
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	jsonReply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	switch {
	case path == "/_ping":
		w.Write([]byte("OK"))
	case path == "/info":
		jsonReply(map[string]any{"Architecture": f.arch, "OSType": "linux", "ServerVersion": "29.9.0"})
	case path == "/images/load":
		f.loadCount++
		tags := manifestTags(r.Body)
		for _, t := range tags {
			f.images[t] = true
			f.imageLabel[t] = LabelHelper
		}
		jsonReply(map[string]string{"stream": "Loaded"})
	case path == "/images/json":
		out := []map[string]any{}
		for ref, lbl := range f.imageLabel {
			if lbl == LabelHelper {
				out = append(out, map[string]any{"Id": "sha256:" + ref, "RepoTags": []string{ref},
					"Labels": map[string]string{Label: lbl}})
			}
		}
		jsonReply(out)
	case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json") && r.Method == http.MethodGet:
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		if f.images[ref] {
			jsonReply(map[string]any{"Id": "sha256:" + ref, "RepoTags": []string{ref},
				"Config": map[string]any{"Env": []string{"PATH=/usr/bin:/bin"}, "WorkingDir": "/app"}})
		} else {
			http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
		}
	case strings.HasPrefix(path, "/images/") && r.Method == http.MethodDelete:
		ref := strings.TrimPrefix(path, "/images/")
		delete(f.images, ref)
		delete(f.imageLabel, ref)
		f.removedImgs = append(f.removedImgs, ref)
		jsonReply([]map[string]string{{"Deleted": ref}})
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json") && r.Method == http.MethodGet:
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
		c := f.containers[ref]
		if c == nil {
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
			return
		}
		status := "running"
		if !c.running {
			status = "exited"
		}
		if c.paused {
			status = "paused"
		}
		jsonReply(map[string]any{"Id": c.id, "Name": "/" + c.name,
			"State":  map[string]any{"Status": status, "Running": c.running, "Paused": c.paused, "Pid": c.pid},
			"Config": map[string]any{"Image": c.image, "Env": c.env, "WorkingDir": c.workdir, "User": c.user}})
	case path == "/containers/create":
		var cfg CreateConfig
		json.NewDecoder(r.Body).Decode(&cfg)
		f.created = append(f.created, cfg)
		f.nextID++
		id := "c" + itoa(f.nextID)
		f.containers[id] = &fakeCtr{id: id, image: cfg.Image}
		jsonReply(map[string]any{"Id": id})
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/start"):
		jsonReply(map[string]string{})
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/wait"):
		jsonReply(map[string]any{"StatusCode": 0})
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/resize"):
		jsonReply(map[string]string{})
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/attach"):
		f.hijackEcho(w, r)
	case strings.HasPrefix(path, "/containers/") && r.Method == http.MethodDelete:
		jsonReply(map[string]string{})
	case path == "/commit":
		ref := r.URL.Query().Get("repo") + ":" + r.URL.Query().Get("tag")
		f.images[ref] = true
		f.commits = append(f.commits, ref)
		jsonReply(map[string]any{"Id": "sha256:committed"})
	case path == "/volumes" && r.Method == http.MethodGet:
		var vs []map[string]any
		for name := range f.volumes {
			lab := map[string]string{}
			if strings.HasPrefix(name, StoreVolume) {
				lab[Label] = LabelStore
			}
			vs = append(vs, map[string]any{"Name": name, "Labels": lab})
		}
		json.NewEncoder(w).Encode(map[string]any{"Volumes": vs})
	case strings.HasPrefix(path, "/volumes/") && r.Method == http.MethodGet:
		name := strings.TrimPrefix(path, "/volumes/")
		if f.volumes[name] {
			jsonReply(map[string]any{"Name": name})
		} else {
			http.Error(w, `{"message":"no such volume"}`, http.StatusNotFound)
		}
	case path == "/volumes/create":
		var body struct{ Name string }
		json.NewDecoder(r.Body).Decode(&body)
		f.volumes[body.Name] = true
		jsonReply(map[string]any{"Name": body.Name})
	case strings.HasPrefix(path, "/volumes/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(path, "/volumes/")
		delete(f.volumes, name)
		f.removedVols = append(f.removedVols, name)
		jsonReply(map[string]string{})
	default:
		http.Error(w, `{"message":"page not found: `+path+`"}`, http.StatusNotFound)
	}
}

func (f *fakeEngine) hijackEcho(w http.ResponseWriter, r *http.Request) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	io.WriteString(conn, "debug shell ready\n")
}

func manifestTags(r io.Reader) []string {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil
		}
		if h.Name == "manifest.json" {
			var m []struct {
				RepoTags []string
			}
			json.NewDecoder(tr).Decode(&m)
			var tags []string
			for _, e := range m {
				tags = append(tags, e.RepoTags...)
			}
			return tags
		}
	}
}

func stdAgentFiles() *AgentFiles {
	return &AgentFiles{
		Files: map[string][]byte{
			"fev-debug-agent": []byte("stand-in agent\n"),
			"fev-debug-stage": []byte("stand-in stage\n"),
			"fev-debug":       []byte("stand-in fev-debug\n"),
			"defaultset.lock": []byte("# fev-debug default set x\n/nix/store/aaaa-zsh\n"),
		},
		Executable: []string{"fev-debug-agent", "fev-debug-stage", "fev-debug"},
		Stamp:      "abc",
	}
}

var _ = bytes.NewReader
