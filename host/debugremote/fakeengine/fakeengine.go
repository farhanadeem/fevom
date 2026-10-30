package fakeengine

import (
	"archive/tar"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"fev/host/debugremote"
)

type Server struct {
	srv *http.Server
	ln  net.Listener

	mu         sync.Mutex
	arch       string
	containers []*Container
	byRef      map[string]*Container
	images     map[string]*Image
	volumes    map[string]bool
	nextID     int
}

type Container struct {
	ID      string
	Name    string
	Image   string
	Running bool
	Paused  bool
	Pid     int
	Status  string
}

type Image struct {
	ID       string
	RepoTags []string
	Size     int64
	Created  int64
	Label    string
}

func New(arch string, ln net.Listener) *Server {
	f := &Server{arch: arch, ln: ln, byRef: map[string]*Container{}, images: map[string]*Image{}, volumes: map[string]bool{}}
	f.srv = &http.Server{Handler: http.HandlerFunc(f.handle)}
	go f.srv.Serve(ln)
	return f
}

func (f *Server) Close() error { return f.srv.Close() }

func (f *Server) AddContainer(name, image string, running bool) *Container {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := strings.Repeat("a", 60) + pad(f.nextID)
	status := "Up 2 hours"
	if !running {
		status = "Exited (0) 1 hour ago"
	}
	c := &Container{ID: id[:64], Name: name, Image: image, Running: running, Pid: 1000 + f.nextID, Status: status}
	f.containers = append(f.containers, c)
	f.byRef[name] = c
	f.byRef[c.ID] = c
	return c
}

func (f *Server) AddImage(ref string) *Image {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	im := &Image{ID: "sha256:" + strings.Repeat("b", 60) + pad(f.nextID), RepoTags: []string{ref}, Size: 7340032, Created: 1759000000}
	f.images[ref] = im
	return im
}

func pad(n int) string {
	s := ""
	for _, d := range []byte{byte('0' + (n/1000)%10), byte('0' + (n/100)%10), byte('0' + (n/10)%10), byte('0' + n%10)} {
		s += string(d)
	}
	return s
}

func (f *Server) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/v1.") {
		if j := strings.IndexByte(path[1:], '/'); j >= 0 {
			path = path[j+1:]
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	switch {
	case path == "/_ping":
		w.Write([]byte("OK"))
	case path == "/info":
		reply(map[string]any{"Architecture": f.arch, "OSType": "linux", "ServerVersion": "29.9.0"})
	case path == "/containers/json" && r.Method == http.MethodGet:
		all := r.URL.Query().Get("all") == "1"
		out := []map[string]any{}
		for _, c := range f.containers {
			if !all && !c.Running {
				continue
			}
			state := "running"
			if !c.Running {
				state = "exited"
			}
			out = append(out, map[string]any{"Id": c.ID, "Names": []string{"/" + c.Name},
				"Image": c.Image, "State": state, "Status": c.Status})
		}
		reply(out)
	case path == "/images/json" && r.Method == http.MethodGet:
		label := r.URL.Query().Get("filters") != "" && strings.Contains(r.URL.Query().Get("filters"), debugremote.LabelHelper)
		out := []map[string]any{}
		for _, im := range f.images {
			if label {
				if im.Label != debugremote.LabelHelper {
					continue
				}
				out = append(out, map[string]any{"Id": im.ID, "RepoTags": im.RepoTags,
					"Labels": map[string]string{debugremote.Label: im.Label}})
				continue
			}
			out = append(out, map[string]any{"Id": im.ID, "RepoTags": im.RepoTags, "Size": im.Size, "Created": im.Created})
		}
		reply(out)
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json") && r.Method == http.MethodGet:
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
		c := f.byRef[ref]
		if c == nil {
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
			return
		}
		status := "running"
		if !c.Running {
			status = "exited"
		}
		if c.Paused {
			status = "paused"
		}
		reply(map[string]any{"Id": c.ID, "Name": "/" + c.Name,
			"State":  map[string]any{"Status": status, "Running": c.Running, "Paused": c.Paused, "Pid": c.Pid},
			"Config": map[string]any{"Image": c.Image, "Env": []string{"PATH=/usr/bin:/bin"}, "WorkingDir": "", "User": ""}})
	case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json") && r.Method == http.MethodGet:
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		if im, ok := f.images[ref]; ok {
			reply(map[string]any{"Id": im.ID, "RepoTags": im.RepoTags,
				"Config": map[string]any{"Env": []string{"PATH=/usr/bin:/bin"}, "WorkingDir": "/app"}})
		} else {
			http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
		}
	case path == "/images/load":
		for _, t := range manifestTags(r.Body) {
			f.images[t] = &Image{ID: "sha256:" + t, RepoTags: []string{t}, Label: debugremote.LabelHelper}
		}
		reply(map[string]string{"stream": "Loaded"})
	case strings.HasPrefix(path, "/images/") && r.Method == http.MethodDelete:
		ref := strings.TrimPrefix(path, "/images/")
		delete(f.images, ref)
		reply([]map[string]string{{"Deleted": ref}})
	case path == "/containers/create":
		var cfg map[string]any
		json.NewDecoder(r.Body).Decode(&cfg)
		f.nextID++
		id := "run" + pad(f.nextID)
		f.byRef[id] = &Container{ID: id}
		reply(map[string]any{"Id": id})
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/start"):
		reply(map[string]string{})
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/wait"):
		reply(map[string]any{"StatusCode": 0})
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/resize"):
		reply(map[string]string{})
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/attach"):
		f.mu.Unlock()
		f.echo(w)
		f.mu.Lock()
	case strings.HasPrefix(path, "/containers/") && r.Method == http.MethodDelete:
		reply(map[string]string{})
	case path == "/commit":
		ref := r.URL.Query().Get("repo") + ":" + r.URL.Query().Get("tag")
		f.images[ref] = &Image{ID: "sha256:committed", RepoTags: []string{ref}, Label: debugremote.LabelTemp}
		reply(map[string]any{"Id": "sha256:committed"})
	case path == "/volumes" && r.Method == http.MethodGet:
		var vs []map[string]any
		for name := range f.volumes {
			lab := map[string]string{}
			if strings.HasPrefix(name, debugremote.StoreVolume) {
				lab[debugremote.Label] = debugremote.LabelStore
			}
			vs = append(vs, map[string]any{"Name": name, "Labels": lab})
		}
		json.NewEncoder(w).Encode(map[string]any{"Volumes": vs})
	case strings.HasPrefix(path, "/volumes/") && r.Method == http.MethodGet:
		name := strings.TrimPrefix(path, "/volumes/")
		if f.volumes[name] {
			reply(map[string]any{"Name": name})
		} else {
			http.Error(w, `{"message":"no such volume"}`, http.StatusNotFound)
		}
	case path == "/volumes/create":
		var body struct{ Name string }
		json.NewDecoder(r.Body).Decode(&body)
		f.volumes[body.Name] = true
		reply(map[string]any{"Name": body.Name})
	case strings.HasPrefix(path, "/volumes/") && r.Method == http.MethodDelete:
		delete(f.volumes, strings.TrimPrefix(path, "/volumes/"))
		reply(map[string]string{})
	default:
		http.Error(w, `{"message":"page not found: `+path+`"}`, http.StatusNotFound)
	}
}

func (f *Server) echo(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	io.WriteString(conn, "debug shell ready\r\n")
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			conn.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func manifestTags(r io.Reader) []string {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil
		}
		if h.Name == "manifest.json" {
			var m []struct{ RepoTags []string }
			json.NewDecoder(tr).Decode(&m)
			var tags []string
			for _, e := range m {
				tags = append(tags, e.RepoTags...)
			}
			return tags
		}
	}
}
