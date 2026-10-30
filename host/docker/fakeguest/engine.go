package fakeguest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fev/proto/netplan"
)

const APIVersion = "1.48"

type Port struct {
	HostIP        string `json:"host_ip"`
	HostPort      uint16 `json:"host_port"`
	Proto         string `json:"proto"`
	ContainerPort uint16 `json:"container_port"`
}

type Spec struct {
	Name    string            `json:"name"`
	Image   string            `json:"image,omitempty"`
	Ports   []Port            `json:"ports,omitempty"`
	Binds   []string          `json:"binds,omitempty"`
	Volumes []string          `json:"volumes,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	Logs    []string          `json:"logs,omitempty"`
}

type container struct {
	id, name, image string
	created         time.Time
	running         bool
	connected       bool
	ports           []Port
	binds           []string
	volumes         []string
	closers         []io.Closer
	labels          map[string]string
	paused          bool
	logs            []logMsg
	copiers         [3]copier
	logSubs         map[chan logMsg]struct{}
}

type Stats struct {
	Connections int      `json:"connections"`
	Requests    int      `json:"requests"`
	Paths       []string `json:"paths"`
}

type Engine struct {
	server *http.Server

	mu           sync.Mutex
	containers   map[string]*container
	subs         map[chan []byte]struct{}
	guest        map[string]string
	stats        Stats
	extraImages  map[string]fakeImage
	extraVolumes map[string]bool
	execs        map[string]*fakeExec
	volumeData   map[string]*VolumeData
	networks     map[string]*fakeNetwork
	authSeen     []string

	HostStream func(service string, payload []byte, framed bool) ([]byte, error)
}

func NewEngine() *Engine {
	e := &Engine{containers: map[string]*container{}, subs: map[chan []byte]struct{}{}, guest: map[string]string{}}
	e.server = &http.Server{Handler: http.HandlerFunc(e.handle), ConnState: func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			e.mu.Lock()
			e.stats.Connections++
			e.mu.Unlock()
		}
	}}
	return e
}

func (e *Engine) Serve(l net.Listener) error { return e.server.Serve(l) }

func (e *Engine) Close() {
	e.server.Close()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.containers {
		e.stop(c)
	}
	for ch := range e.subs {
		close(ch)
		delete(e.subs, ch)
	}
}

func (e *Engine) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.stats
	s.Paths = append([]string{}, s.Paths...)
	return s
}

func (e *Engine) Answer(network, guest, real string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if real == "" {
		delete(e.guest, network+" "+guest)
		return
	}
	e.guest[network+" "+guest] = real
}

func (e *Engine) resolve(network, guest string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	real, ok := e.guest[network+" "+guest]
	return real, ok
}

func newID() string {
	var b [32]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

var Uplink = netplan.Default().Guest().Addr().String()

func guestAddrs(p Port) []string {
	port := strconv.Itoa(int(p.HostPort))
	switch p.HostIP {
	case "":
		return []string{"127.0.0.1:" + port, "[::1]:" + port, Uplink + ":" + port}
	case "0.0.0.0":
		return []string{"127.0.0.1:" + port, Uplink + ":" + port}
	case "::":
		return []string{"[::1]:" + port}
	}
	return []string{net.JoinHostPort(p.HostIP, port)}
}

func (e *Engine) Run(spec Spec) (string, error) {
	c := &container{id: newID(), name: spec.Name, image: spec.Image, created: time.Now(), running: true, connected: true, ports: spec.Ports,
		binds: spec.Binds, volumes: spec.Volumes, labels: spec.Labels}
	for _, l := range spec.Logs {
		c.logged(c.copiers[1].write(1, []byte(l+"\n"), c.created))
	}
	if c.name == "" {
		c.name = "fake_" + c.id[:8]
	}
	if c.image == "" {
		c.image = "fake:latest"
	}
	for _, p := range spec.Ports {
		if p.Proto != "tcp" && p.Proto != "udp" {
			return "", fmt.Errorf("port protocol %q", p.Proto)
		}
		if p.HostIP != "" {
			if _, err := netip.ParseAddr(p.HostIP); err != nil {
				return "", err
			}
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, other := range e.containers {
		if other.name == c.name {
			return "", fmt.Errorf("the container name %q is already in use", c.name)
		}
	}
	if err := e.publish(c); err != nil {
		e.stop(c)
		return "", err
	}
	e.containers[c.id] = c
	e.emit("container", "create", c.id, map[string]string{"name": c.name, "image": c.image})
	e.emit("container", "start", c.id, map[string]string{"name": c.name, "image": c.image})
	return c.id, nil
}

func (e *Engine) publish(c *container) error {
	for _, p := range c.ports {
		greeting := fmt.Sprintf("%s:%d/%s", c.name, p.ContainerPort, p.Proto)
		var real string
		if p.Proto == "tcp" {
			l, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				return err
			}
			go serveTCP(l, greeting)
			c.closers = append(c.closers, l)
			real = l.Addr().String()
		} else {
			pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				return err
			}
			go serveUDP(pc, greeting)
			c.closers = append(c.closers, pc)
			real = pc.LocalAddr().String()
		}
		for _, addr := range guestAddrs(p) {
			key := p.Proto + " " + addr
			if _, taken := e.guest[key]; taken {
				return fmt.Errorf("Bind for %s failed: port is already allocated", addr)
			}
			e.guest[key] = real
		}
	}
	return nil
}

func (e *Engine) stop(c *container) {
	for _, closer := range c.closers {
		closer.Close()
	}
	c.closers = nil
	for _, p := range c.ports {
		for _, addr := range guestAddrs(p) {
			delete(e.guest, p.Proto+" "+addr)
		}
	}
}

func (e *Engine) find(ref string) *container {
	if c, ok := e.containers[ref]; ok {
		return c
	}
	for _, c := range e.containers {
		if c.name == ref || (len(ref) >= 4 && strings.HasPrefix(c.id, ref)) {
			return c
		}
	}
	return nil
}

func (e *Engine) Die(ref string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.find(ref)
	if c == nil {
		return fmt.Errorf("No such container: %s", ref)
	}
	e.stop(c)
	e.endExecs(c.id)
	now := time.Now()
	for s := byte(1); s <= 2; s++ {
		c.logged(c.copiers[s].end(s, now))
	}
	c.running = false
	c.paused = false
	e.emit("container", "die", c.id, map[string]string{"name": c.name, "image": c.image, "exitCode": "0"})
	return nil
}

func (e *Engine) Start(ref string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.find(ref)
	if c == nil {
		return fmt.Errorf("No such container: %s", ref)
	}
	if c.running {
		return nil
	}
	c.running = true
	if c.connected {
		if err := e.publish(c); err != nil {
			return err
		}
	}
	e.emit("container", "start", c.id, map[string]string{"name": c.name, "image": c.image})
	return nil
}

func (e *Engine) Disconnect(ref string) error { return e.network(ref, false) }
func (e *Engine) Connect(ref string) error    { return e.network(ref, true) }

func (e *Engine) network(ref string, connect bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.find(ref)
	if c == nil {
		return fmt.Errorf("No such container: %s", ref)
	}
	action := "disconnect"
	if connect && !c.connected && c.running {
		if err := e.publish(c); err != nil {
			return err
		}
	}
	if connect {
		action = "connect"
	} else {
		e.stop(c)
	}
	c.connected = connect
	e.emit("network", action, "f00d"+strings.Repeat("0", 60), map[string]string{"container": c.id, "name": "bridge", "type": "bridge"})
	return nil
}

func (e *Engine) Emit(typ, action, id string, attrs map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.emit(typ, action, id, attrs)
}

func (e *Engine) DropEvents() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for ch := range e.subs {
		close(ch)
		delete(e.subs, ch)
	}
}

func (e *Engine) emit(typ, action, id string, attrs map[string]string) {
	now := time.Now()
	msg := map[string]any{
		"Type": typ, "Action": action,
		"Actor": map[string]any{"ID": id, "Attributes": attrs},
		"scope": "local", "time": now.Unix(), "timeNano": now.UnixNano(),
	}
	if typ == "container" {
		msg["status"], msg["id"], msg["from"] = action, id, attrs["image"]
	}
	line, _ := json.Marshal(msg)
	line = append(line, '\n')
	for ch := range e.subs {
		select {
		case ch <- line:
		default:
			close(ch)
			delete(e.subs, ch)
		}
	}
}

var versionPrefix = regexp.MustCompile(`^/v[0-9]+\.[0-9]+/`)

func fail(w http.ResponseWriter, status int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"message": fmt.Sprintf(format, args...)})
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) { e.handle(w, r) }

func (e *Engine) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if loc := versionPrefix.FindStringIndex(path); loc != nil {
		path = path[loc[1]-1:]
	}
	e.mu.Lock()
	e.stats.Requests++
	e.stats.Paths = append(e.stats.Paths, r.Method+" "+path)
	e.mu.Unlock()

	h := w.Header()
	h.Set("Api-Version", APIVersion)
	h.Set("Ostype", "linux")
	h.Set("Server", "Docker/29.9.0 (linux)")
	h.Set("Docker-Experimental", "false")

	rest := func(prefix string) (string, bool) { return strings.CutPrefix(path, prefix) }
	switch {
	case path == "/_ping":
		h.Set("Builder-Version", "2")
		h.Set("Swarm", "inactive")
		h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Content-Length", "2")
		if r.Method != http.MethodHead {
			io.WriteString(w, "OK")
		}
	case path == "/version":
		reply(w, map[string]any{
			"Platform": map[string]string{"Name": "fev fake engine"},
			"Version":  "29.9.0", "ApiVersion": APIVersion, "MinAPIVersion": "1.44",
			"Os": "linux", "Arch": "arm64", "KernelVersion": "0.0.0-fake", "GoVersion": "go0", "GitCommit": "fake",
			"Components": []map[string]any{{"Name": "Engine", "Version": "29.9.0", "Details": map[string]string{"ApiVersion": APIVersion, "Os": "linux", "Arch": "arm64"}}},
		})
	case path == "/info":
		e.mu.Lock()
		total, running := len(e.containers), 0
		for _, c := range e.containers {
			if c.running {
				running++
			}
		}
		e.mu.Unlock()
		reply(w, map[string]any{
			"ID": "fake", "Name": "fev-fake", "ServerVersion": "29.9.0",
			"Containers": total, "ContainersRunning": running, "ContainersPaused": 0, "ContainersStopped": total - running,
			"Images": 0, "Driver": "overlayfs", "OSType": "linux", "Architecture": "aarch64", "OperatingSystem": "fev fake engine",
			"KernelVersion": "0.0.0-fake", "NCPU": 1, "MemTotal": 1 << 30, "CgroupDriver": "cgroupfs", "CgroupVersion": "2",
			"DockerRootDir": "/var/lib/docker", "Plugins": map[string]any{}, "Swarm": map[string]any{"LocalNodeState": "inactive"},
			"Runtimes": map[string]any{}, "DefaultRuntime": "runc", "SecurityOptions": []string{}, "Warnings": []string{},
		})
	case path == "/auth" && r.Method == http.MethodPost:
		e.auth(w, r)
	case path == "/_fake/auth":
		e.logins(w)
	case path == "/events":
		e.events(w, r)
	case path == "/containers/json":
		reply(w, e.list(r.URL.Query().Get("all") != "" && r.URL.Query().Get("all") != "0"))
	case path == "/build" && r.Method == http.MethodPost:
		sum := sha256.New()
		n, err := io.Copy(sum, r.Body)
		if err != nil {
			fail(w, http.StatusBadRequest, "reading the build context: %v", err)
			return
		}
		reply(w, map[string]string{"stream": fmt.Sprintf("sha256:%x %d\n", sum.Sum(nil), n)})
	case path == "/_fake/run" && r.Method == http.MethodPost:
		var spec Spec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		id, err := e.Run(spec)
		if err != nil {
			fail(w, http.StatusConflict, "%v", err)
			return
		}
		reply(w, map[string]string{"id": id})
	case strings.HasPrefix(path, "/_fake/host-stream/") && r.Method == http.MethodPost:
		if e.HostStream == nil {
			fail(w, http.StatusNotImplemented, "this fake guest asks the host for no streams")
			return
		}
		payload, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		answer, err := e.HostStream(strings.TrimPrefix(path, "/_fake/host-stream/"), payload, r.URL.Query().Get("reply") == "framed")
		if err != nil {
			fail(w, http.StatusBadGateway, "%v", err)
			return
		}
		h.Set("Content-Type", "application/octet-stream")
		w.Write(answer)
	case path == "/_fake/stats":
		reply(w, e.Stats())
	case path == "/_fake/drop-events" && r.Method == http.MethodPost:
		e.DropEvents()
		reply(w, map[string]string{})
	case path == "/_fake/emit" && r.Method == http.MethodPost:
		var ev struct {
			Type, Action, ID string
			Attributes       map[string]string
		}
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		e.Emit(ev.Type, ev.Action, ev.ID, ev.Attributes)
		reply(w, map[string]string{})
	default:
		if e.execRoute(w, r, path) {
			return
		}
		if e.volumeRoute(w, r, path) || e.imageTarRoute(w, r, path) || e.appRoute(w, r, path) {
			return
		}
		for prefix, do := range map[string]func(string) error{
			"/_fake/die/": e.Die, "/_fake/start/": e.Start, "/_fake/disconnect/": e.Disconnect, "/_fake/connect/": e.Connect,
		} {
			if ref, ok := rest(prefix); ok && r.Method == http.MethodPost {
				if err := do(ref); err != nil {
					fail(w, http.StatusNotFound, "%v", err)
					return
				}
				reply(w, map[string]string{})
				return
			}
		}
		if ref, ok := rest("/containers/"); ok {
			if id, ok := strings.CutSuffix(ref, "/json"); ok && r.Method == http.MethodGet {
				e.inspect(w, id)
				return
			}
			if id, ok := strings.CutSuffix(ref, "/attach"); ok && r.Method == http.MethodPost {
				e.attach(w, id)
				return
			}
		}
		fail(w, http.StatusNotFound, "page not found")
	}
}

func (e *Engine) events(w http.ResponseWriter, r *http.Request) {
	ch := make(chan []byte, 1024)
	e.mu.Lock()
	e.subs[ch] = struct{}{}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		if _, ok := e.subs[ch]; ok {
			delete(e.subs, ch)
		}
		e.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	http.NewResponseController(w).Flush()
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				panic(http.ErrAbortHandler)
			}
			w.Write(line)
			http.NewResponseController(w).Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (e *Engine) list(all bool) []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []map[string]any{}
	for _, c := range e.containers {
		if !c.running && !all {
			continue
		}
		ports := []map[string]any{}
		if c.running && c.connected {
			for _, p := range c.ports {
				for _, ip := range hostIPs(p) {
					ports = append(ports, map[string]any{"IP": ip, "PrivatePort": p.ContainerPort, "PublicPort": p.HostPort, "Type": p.Proto})
				}
			}
		}
		state, status := "running", "Up 1 second"
		if c.paused {
			state, status = "paused", "Up 1 second (Paused)"
		}
		if !c.running {
			state, status = "exited", "Exited (0) 1 second ago"
		}
		mounts := []map[string]any{}
		for _, v := range c.volumes {
			mounts = append(mounts, map[string]any{"Type": "volume", "Name": v, "Destination": "/vol/" + v})
		}
		out = append(out, map[string]any{
			"Id": c.id, "Names": []string{"/" + c.name}, "Image": c.image, "ImageID": imageID(c.image),
			"Command": "fake", "Created": c.created.Unix(), "State": state, "Status": status, "Ports": ports,
			"Labels": labelsOf(c), "HostConfig": map[string]string{"NetworkMode": "bridge"},
			"NetworkSettings": map[string]any{"Networks": map[string]any{}}, "Mounts": mounts,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["Created"].(int64) > out[j]["Created"].(int64) })
	return out
}

func (e *Engine) Running() (ids, names []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.containers {
		if c.running {
			ids = append(ids, c.id)
			names = append(names, c.name)
		}
	}
	return ids, names
}

func hostIPs(p Port) []string {
	if p.HostIP == "" {
		return []string{"0.0.0.0", "::"}
	}
	return []string{p.HostIP}
}

func (e *Engine) inspect(w http.ResponseWriter, ref string) {
	e.mu.Lock()
	c := e.find(ref)
	if c == nil {
		e.mu.Unlock()
		fail(w, http.StatusNotFound, "No such container: %s", ref)
		return
	}
	ports := map[string][]map[string]string{}
	if c.running && c.connected {
		for _, p := range c.ports {
			key := fmt.Sprintf("%d/%s", p.ContainerPort, p.Proto)
			for _, ip := range hostIPs(p) {
				ports[key] = append(ports[key], map[string]string{"HostIp": ip, "HostPort": strconv.Itoa(int(p.HostPort))})
			}
		}
	}
	status := "running"
	if !c.running {
		status = "exited"
	}
	mounts := []map[string]any{}
	for i, src := range c.binds {
		mounts = append(mounts, map[string]any{"Type": "bind", "Source": src, "Destination": "/bind" + strconv.Itoa(i), "Mode": "", "RW": true, "Propagation": "rprivate"})
	}
	for _, name := range c.volumes {
		mounts = append(mounts, map[string]any{"Type": "volume", "Name": name, "Source": "/var/lib/docker/volumes/" + name + "/_data", "Destination": "/vol/" + name, "Driver": "local", "RW": true})
	}
	doc := map[string]any{
		"Id": c.id, "Name": "/" + c.name, "Created": c.created.UTC().Format(time.RFC3339Nano), "Image": imageID(c.image),
		"State":           map[string]any{"Status": status, "Running": c.running, "Paused": c.paused, "Restarting": false, "Pid": 1},
		"Config":          map[string]any{"Image": c.image, "Labels": labelsOf(c), "Env": []string{"PATH=/usr/bin:/bin"}, "Cmd": []string{"fake"}},
		"HostConfig":      map[string]any{"NetworkMode": "bridge"},
		"NetworkSettings": map[string]any{"Ports": ports, "Networks": map[string]any{}},
		"Mounts":          mounts,
	}
	e.mu.Unlock()
	reply(w, doc)
}

func (e *Engine) attach(w http.ResponseWriter, ref string) {
	e.mu.Lock()
	c := e.find(ref)
	e.mu.Unlock()
	if c == nil {
		fail(w, http.StatusNotFound, "No such container: %s", ref)
		return
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	defer conn.Close()
	io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	io.Copy(conn, rw.Reader)
	io.WriteString(conn, AttachEOF)
}

const AttachEOF = "\n<fake engine: end of input seen>\n"

func serveTCP(l net.Listener, greeting string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) { io.Copy(w, r.Body) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Remote-Addr", r.RemoteAddr)
		fmt.Fprintf(w, "hello from %s\n", greeting)
	})
	(&http.Server{Handler: mux}).Serve(l)
}

func serveUDP(pc net.PacketConn, greeting string) {
	buf := make([]byte, 65535)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if string(buf[:n]) == "no reply" {
			continue
		}
		pc.WriteTo(append([]byte(greeting+" "), buf[:n]...), from)
	}
}
