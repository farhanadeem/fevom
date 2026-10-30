package fakeguest

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const logPart = 16 << 10

func frame(stream byte, b []byte) []byte {
	var h [8]byte
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(b)))
	return append(h[:], b...)
}

func normalRef(ref string) string {
	name, digest, _ := strings.Cut(ref, "@")
	domain, rest, ok := strings.Cut(name, "/")
	if !ok || !(strings.ContainsAny(domain, ".:") || domain == "localhost") {
		domain, rest = "docker.io", name
	}
	if domain == "docker.io" && !strings.Contains(rest, "/") {
		rest = "library/" + rest
	}
	full := domain + "/" + rest
	if digest != "" {
		return full + "@" + digest
	}
	if strings.LastIndex(full, ":") <= strings.LastIndex(full, "/") {
		full += ":latest"
	}
	return full
}

func familiarRef(ref string) string {
	n := strings.TrimPrefix(normalRef(ref), "docker.io/")
	return strings.TrimPrefix(n, "library/")
}

func imageID(ref string) string {
	if strings.HasPrefix(ref, "sha256:") {
		return ref
	}
	sum := sha256.Sum256([]byte(normalRef(ref)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func labelsOf(c *container) map[string]string {
	if c.labels == nil {
		return map[string]string{}
	}
	return c.labels
}

type fakeImage struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Size     int64    `json:"Size"`
	Created  int64    `json:"Created"`
}

func (e *Engine) appRoute(w http.ResponseWriter, r *http.Request, path string) bool {
	q := r.URL.Query()
	if strings.HasPrefix(path, "/networks") && e.networkRoute(w, r, path) {
		return true
	}
	switch {
	case path == "/images/json" && r.Method == http.MethodGet:
		reply(w, e.images())
	case path == "/volumes" && r.Method == http.MethodGet:
		reply(w, map[string]any{"Volumes": e.volumes(), "Warnings": []string{}})
	case path == "/volumes/create" && r.Method == http.MethodPost:
		var v struct{ Name string }
		json.NewDecoder(r.Body).Decode(&v)
		if v.Name == "" {
			v.Name = newID()
		}
		e.mu.Lock()
		e.addVolume(v.Name)
		e.emit("volume", "create", v.Name, map[string]string{"driver": "local"})
		e.mu.Unlock()
		reply(w, map[string]any{"Name": v.Name, "Driver": "local", "Mountpoint": "/var/lib/docker/volumes/" + v.Name + "/_data", "Scope": "local"})
	case path == "/_fake/volume" && r.Method == http.MethodPost:
		var v struct{ Name string }
		json.NewDecoder(r.Body).Decode(&v)
		e.mu.Lock()
		e.addVolume(v.Name)
		e.mu.Unlock()
		reply(w, map[string]string{})
	case path == "/_fake/image" && r.Method == http.MethodPost:
		var im fakeImage
		json.NewDecoder(r.Body).Decode(&im)
		e.mu.Lock()
		if e.extraImages == nil {
			e.extraImages = map[string]fakeImage{}
		}
		im.ID = "sha256:" + newID()
		for i, t := range im.RepoTags {
			im.RepoTags[i] = familiarRef(t)
			if i == 0 {
				im.ID = imageID(t)
			}
		}
		im.Created = time.Now().Unix()
		e.extraImages[im.ID] = im
		e.mu.Unlock()
		reply(w, map[string]string{"id": im.ID})
	case strings.HasPrefix(path, "/volumes/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(path, "/volumes/")
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, c := range e.containers {
			for _, v := range c.volumes {
				if v == name {
					fail(w, http.StatusConflict, "remove %s: volume is in use - [%s]", name, c.id)
					return true
				}
			}
		}
		if !e.extraVolumes[name] {
			fail(w, http.StatusNotFound, "get %s: no such volume", name)
			return true
		}
		delete(e.extraVolumes, name)
		e.emit("volume", "destroy", name, map[string]string{"driver": "local"})
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/images/") && r.Method == http.MethodDelete:
		ref := strings.TrimPrefix(path, "/images/")
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, c := range e.containers {
			if imageID(c.image) == imageID(ref) && q.Get("force") == "" {
				fail(w, http.StatusConflict, "conflict: unable to remove repository reference %q (must force) - container %s is using its referenced image", ref, c.id[:12])
				return true
			}
		}
		for k, im := range e.extraImages {
			for _, t := range im.RepoTags {
				if t == familiarRef(ref) || im.ID == imageID(ref) {
					delete(e.extraImages, k)
					e.emit("image", "delete", im.ID, map[string]string{"name": ref})
					reply(w, []map[string]string{{"Untagged": ref}, {"Deleted": im.ID}})
					return true
				}
			}
		}
		fail(w, http.StatusNotFound, "No such image: %s", ref)
	case strings.HasPrefix(path, "/containers/") && r.Method == http.MethodDelete:
		ref := strings.TrimPrefix(path, "/containers/")
		e.mu.Lock()
		defer e.mu.Unlock()
		c := e.find(ref)
		if c == nil {
			fail(w, http.StatusNotFound, "No such container: %s", ref)
			return true
		}
		if c.running && q.Get("force") == "" {
			fail(w, http.StatusConflict, "cannot remove container %q: container is running: stop the container before removing or force remove", c.name)
			return true
		}
		e.stop(c)
		e.endLogs(c)
		delete(e.containers, c.id)
		e.emit("container", "destroy", c.id, map[string]string{"name": c.name, "image": c.image})
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/logs") && r.Method == http.MethodGet:
		e.logs(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/logs"))
	case strings.HasPrefix(path, "/_fake/log/") && r.Method == http.MethodPost:
		ref := strings.TrimPrefix(path, "/_fake/log/")
		body := make([]byte, 0, 4096)
		buf := make([]byte, 64<<10)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		stream := byte(1)
		if q.Get("stream") == "2" {
			stream = 2
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		c := e.find(ref)
		if c == nil {
			fail(w, http.StatusNotFound, "No such container: %s", ref)
			return true
		}
		c.logged(c.copiers[stream].write(stream, body, time.Now()))
		reply(w, map[string]string{})
	case strings.HasPrefix(path, "/containers/") && r.Method == http.MethodPost:
		ref, action, ok := strings.Cut(strings.TrimPrefix(path, "/containers/"), "/")
		if !ok {
			return false
		}
		var err error
		switch action {
		case "start":
			err = e.Start(ref)
		case "stop", "kill":
			err = e.Die(ref)
		case "restart":
			if err = e.Die(ref); err == nil {
				err = e.Start(ref)
			}
		case "pause", "unpause":
			err = e.pause(ref, action == "pause")
		default:
			return false
		}
		if err != nil {
			fail(w, http.StatusNotFound, "%v", err)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		return false
	}
	return true
}

func (e *Engine) addVolume(name string) {
	if e.extraVolumes == nil {
		e.extraVolumes = map[string]bool{}
	}
	e.extraVolumes[name] = true
}

func (e *Engine) pause(ref string, on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.find(ref)
	if c == nil {
		return errNoSuch(ref)
	}
	c.paused = on && c.running
	action := "unpause"
	if on {
		action = "pause"
	}
	e.emit("container", action, c.id, map[string]string{"name": c.name, "image": c.image})
	return nil
}

type errNoSuch string

func (e errNoSuch) Error() string { return "No such container: " + string(e) }

func (e *Engine) images() []fakeImage {
	e.mu.Lock()
	defer e.mu.Unlock()
	byID := map[string]fakeImage{}
	for _, c := range e.containers {
		if c.image == "" || strings.HasPrefix(c.image, "sha256:") {
			continue
		}
		id := imageID(c.image)
		if _, ok := byID[id]; !ok {
			byID[id] = fakeImage{ID: id, RepoTags: []string{familiarRef(c.image)}, Size: 7 << 20, Created: c.created.Unix()}
		}
	}
	for _, im := range e.extraImages {
		byID[im.ID] = im
	}
	out := []fakeImage{}
	for _, im := range byID {
		out = append(out, im)
	}
	sort.Slice(out, func(i, j int) bool { return strings.Join(out[i].RepoTags, ",") < strings.Join(out[j].RepoTags, ",") })
	return out
}

func (e *Engine) volumes() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	names := map[string]bool{}
	for _, c := range e.containers {
		for _, v := range c.volumes {
			names[v] = true
		}
	}
	for v := range e.extraVolumes {
		names[v] = true
	}
	out := []map[string]any{}
	for _, v := range sortedNames(names) {
		out = append(out, map[string]any{"Name": v, "Driver": "local", "Mountpoint": "/var/lib/docker/volumes/" + v + "/_data", "Scope": "local", "Labels": map[string]string{}, "CreatedAt": "2026-10-16T00:00:00Z"})
	}
	return out
}

func sortedNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type logMsg struct {
	at      time.Time
	stream  byte
	text    []byte
	partial bool
}

type copier struct {
	buf       []byte
	partialAt time.Time
}

func (cp *copier) write(stream byte, b []byte, now time.Time) []logMsg {
	cp.buf = append(cp.buf, b...)
	var out []logMsg
	for {
		at := now
		if !cp.partialAt.IsZero() {
			at = cp.partialAt
		}
		if i := bytes.IndexByte(cp.buf[:min(len(cp.buf), logPart)], '\n'); i >= 0 {
			out = append(out, logMsg{at: at, stream: stream, text: bytes.Clone(cp.buf[:i])})
			cp.buf = cp.buf[i+1:]
			cp.partialAt = time.Time{}
			continue
		}
		if len(cp.buf) < logPart {
			return out
		}
		out = append(out, logMsg{at: at, stream: stream, text: bytes.Clone(cp.buf[:logPart]), partial: true})
		cp.buf = cp.buf[logPart:]
		cp.partialAt = at
	}
}

func (cp *copier) end(stream byte, now time.Time) []logMsg {
	defer func() { cp.buf, cp.partialAt = nil, time.Time{} }()
	if len(cp.buf) == 0 {
		return nil
	}
	at := now
	if !cp.partialAt.IsZero() {
		at = cp.partialAt
	}
	return []logMsg{{at: at, stream: stream, text: bytes.Clone(cp.buf), partial: true}}
}

func (c *container) logged(msgs []logMsg) {
	for _, m := range msgs {
		c.logs = append(c.logs, m)
		for ch := range c.logSubs {
			select {
			case ch <- m:
			default:
			}
		}
	}
}

func (m logMsg) render(timestamps bool) []byte {
	var b []byte
	if timestamps {
		b = append([]byte(m.at.UTC().Format(rfc3339NanoFixed)), ' ')
	}
	b = append(b, m.text...)
	if !m.partial {
		b = append(b, '\n')
	}
	return frame(m.stream, b)
}

const rfc3339NanoFixed = "2006-01-02T15:04:05.000000000Z07:00"

func (e *Engine) logs(w http.ResponseWriter, r *http.Request, ref string) {
	q := r.URL.Query()
	ts := q.Get("timestamps") == "1" || q.Get("timestamps") == "true"
	var since time.Time
	if s := q.Get("since"); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			sec := int64(f)
			since = time.Unix(sec, int64((f-float64(sec))*1e9))
		}
	}
	e.mu.Lock()
	c := e.find(ref)
	if c == nil {
		e.mu.Unlock()
		fail(w, http.StatusNotFound, "No such container: %s", ref)
		return
	}
	backlog := c.logs
	if t, err := strconv.Atoi(q.Get("tail")); err == nil && t >= 0 && t < len(backlog) {
		backlog = backlog[len(backlog)-t:]
	}
	passed := func(m logMsg) bool {
		if since.IsZero() || !m.at.Before(since) {
			since = time.Time{}
			return true
		}
		return false
	}
	for len(backlog) > 0 && !passed(backlog[0]) {
		backlog = backlog[1:]
	}
	var ch chan logMsg
	if q.Get("follow") == "1" || q.Get("follow") == "true" {
		ch = make(chan logMsg, 1<<16)
		if c.logSubs == nil {
			c.logSubs = map[chan logMsg]struct{}{}
		}
		c.logSubs[ch] = struct{}{}
		defer func() {
			e.mu.Lock()
			delete(c.logSubs, ch)
			e.mu.Unlock()
		}()
	}
	e.mu.Unlock()
	w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
	w.WriteHeader(http.StatusOK)
	for _, m := range backlog {
		w.Write(m.render(ts))
	}
	rc := http.NewResponseController(w)
	rc.Flush()
	if ch == nil {
		return
	}
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return
			}
			if passed(m) {
				w.Write(m.render(ts))
				rc.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (e *Engine) endLogs(c *container) {
	for ch := range c.logSubs {
		close(ch)
		delete(c.logSubs, ch)
	}
}
