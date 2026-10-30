package fakeguest

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAppEndpoints(t *testing.T) {
	e := NewEngine()
	srv := httptest.NewServer(e)
	defer srv.Close()
	defer e.Close()
	long := strings.Repeat("x", 40<<10)
	id, err := e.Run(Spec{Name: "web", Image: "nginx", Labels: map[string]string{"com.docker.compose.project": "shop"}, Volumes: []string{"data"}, Logs: []string{"one", long}})
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) (int, []byte) {
		req, _ := http.NewRequest(method, srv.URL+"/v1.44"+path, strings.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	_, b := do("GET", "/containers/json?all=1", "")
	var list []struct {
		Labels map[string]string
		State  string
	}
	json.Unmarshal(b, &list)
	if len(list) != 1 || list[0].Labels["com.docker.compose.project"] != "shop" {
		t.Fatalf("list: %s", b)
	}
	if code, _ := do("POST", "/containers/web/pause", ""); code != 204 {
		t.Fatalf("pause: %d", code)
	}
	_, b = do("GET", "/containers/json?all=1", "")
	if !strings.Contains(string(b), `"State":"paused"`) {
		t.Fatalf("not paused: %s", b)
	}
	if code, _ := do("DELETE", "/containers/web", ""); code != 409 {
		t.Fatalf("a running container removed without force: %d", code)
	}
	if code, _ := do("DELETE", "/volumes/data", ""); code != 409 {
		t.Fatalf("a volume in use removed: %d", code)
	}

	frames := func(query string) ([]int, string) {
		code, b := do("GET", "/containers/"+id+"/logs?stdout=1&stderr=1"+query, "")
		if code != 200 {
			t.Fatal(code)
		}
		var sizes []int
		var text []byte
		for r := bufio.NewReader(strings.NewReader(string(b))); ; {
			var h [8]byte
			if _, err := io.ReadFull(r, h[:]); err != nil {
				break
			}
			n := int(binary.BigEndian.Uint32(h[4:]))
			p := make([]byte, n)
			io.ReadFull(r, p)
			sizes = append(sizes, n)
			text = append(text, p...)
		}
		return sizes, string(text)
	}
	if sizes, text := frames("&tail=4"); text != "one\n"+long+"\n" || len(sizes) != 4 || sizes[1] != logPart {
		t.Fatalf("frames %v, %d bytes", sizes, len(text))
	}
	if sizes, text := frames("&tail=2"); text != long[logPart:]+"\n" || len(sizes) != 2 {
		t.Fatalf("tail=2: frames %v, %d bytes", sizes, len(text))
	}

	if code, _ := do("POST", "/containers/web/stop", ""); code != 204 {
		t.Fatal(code)
	}
	if code, _ := do("DELETE", "/containers/web", ""); code != 204 {
		t.Fatal(code)
	}
	if code, _ := do("POST", "/volumes/create", `{"Name":"cache"}`); code != 200 {
		t.Fatal(code)
	}
	_, b = do("GET", "/volumes", "")
	if !strings.Contains(string(b), `"Name":"cache"`) || strings.Contains(string(b), `"Name":"data"`) {
		t.Fatalf("volumes: %s", b)
	}
	do("POST", "/_fake/image", `{"RepoTags":["alpine:3"],"Size":9000000}`)
	_, b = do("GET", "/images/json", "")
	if !strings.Contains(string(b), "alpine:3") {
		t.Fatalf("images: %s", b)
	}
	if code, _ := do("DELETE", "/images/alpine:3", ""); code != 200 {
		t.Fatal(code)
	}
}

func TestAppLogsFollow(t *testing.T) {
	e := NewEngine()
	srv := httptest.NewServer(e)
	defer srv.Close()
	defer e.Close()
	if _, err := e.Run(Spec{Name: "a", Logs: []string{"old"}}); err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(srv.URL + "/v1.44/containers/a/logs?follow=1&timestamps=1&tail=0")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if _, err := http.Post(srv.URL+"/_fake/log/a", "text/plain", strings.NewReader("new one\nnew two\n")); err != nil {
		t.Fatal(err)
	}
	var h [8]byte
	var got []string
	for len(got) < 2 {
		if _, err := io.ReadFull(res.Body, h[:]); err != nil {
			t.Fatal(err)
		}
		p := make([]byte, binary.BigEndian.Uint32(h[4:]))
		io.ReadFull(res.Body, p)
		got = append(got, string(p))
	}
	for i, want := range []string{" new one\n", " new two\n"} {
		if !strings.HasSuffix(got[i], want) || !strings.Contains(got[i], "T") || !strings.HasPrefix(got[i], "20") {
			t.Fatalf("line %d: %q", i, got[i])
		}
	}
}

func TestAppLogsLongLinesInParts(t *testing.T) {
	e := NewEngine()
	srv := httptest.NewServer(e)
	defer srv.Close()
	defer e.Close()
	long := strings.Repeat("y", 2*logPart+100)
	exact := strings.Repeat("z", logPart)
	if _, err := e.Run(Spec{Name: "a", Logs: []string{long, exact}}); err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(srv.URL + "/v1.44/containers/a/logs?stdout=1&timestamps=1")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var parts []string
	for {
		var h [8]byte
		if _, err := io.ReadFull(res.Body, h[:]); err != nil {
			break
		}
		p := make([]byte, binary.BigEndian.Uint32(h[4:]))
		io.ReadFull(res.Body, p)
		parts = append(parts, string(p))
	}
	if len(parts) != 5 {
		t.Fatalf("%d frames, want 5 (three parts and two)", len(parts))
	}
	ts := parts[0][:31]
	if len(ts) != 31 || ts[30] != ' ' || ts[29] != 'Z' || ts[19] != '.' {
		t.Fatalf("not the fixed-width moment: %q", ts)
	}
	wants := []string{strings.Repeat("y", logPart), strings.Repeat("y", logPart), strings.Repeat("y", 100) + "\n", exact, "\n"}
	for i, p := range parts {
		if i == 3 {
			ts = p[:31]
		}
		if p[:31] != ts || p[31:] != wants[i] {
			t.Fatalf("part %d: %q... (%d bytes)", i, p[:min(len(p), 40)], len(p))
		}
	}
}

func TestAppImageIDs(t *testing.T) {
	e := NewEngine()
	srv := httptest.NewServer(e)
	defer srv.Close()
	defer e.Close()
	for _, s := range []Spec{{Name: "a", Image: "docker.io/library/nginx"}, {Name: "b", Image: "nginx:latest"}, {Name: "c", Image: "host:5000/app"}} {
		if _, err := e.Run(s); err != nil {
			t.Fatal(err)
		}
	}
	get := func(path string, v any) {
		res, err := http.Get(srv.URL + "/v1.44" + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		json.NewDecoder(res.Body).Decode(v)
	}
	var cs []struct {
		Names          []string
		Image, ImageID string
	}
	get("/containers/json?all=1", &cs)
	var ims []struct {
		Id       string
		RepoTags []string
	}
	get("/images/json", &ims)
	tags := map[string]string{}
	for _, im := range ims {
		tags[im.Id] = strings.Join(im.RepoTags, ",")
	}
	byName := map[string]string{}
	for _, c := range cs {
		byName[c.Names[0]] = tags[c.ImageID]
	}
	if len(ims) != 2 || byName["/a"] != "nginx:latest" || byName["/b"] != "nginx:latest" || byName["/c"] != "host:5000/app:latest" {
		t.Fatalf("images %v, containers' %v", ims, byName)
	}
	for ref, want := range map[string]string{"nginx": "docker.io/library/nginx:latest", "user/app:2": "docker.io/user/app:2", "localhost/x": "localhost/x:latest", "ghcr.io/o/p@sha256:ab": "ghcr.io/o/p@sha256:ab"} {
		if got := normalRef(ref); got != want {
			t.Errorf("normalRef(%q) = %q, want %q", ref, got, want)
		}
	}
}

func readMsgs(t *testing.T, r io.Reader, n int) (streams []byte, texts []string) {
	t.Helper()
	for n < 0 || len(texts) < n {
		var h [8]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			if n >= 0 {
				t.Fatalf("after %d messages: %v", len(texts), err)
			}
			return
		}
		p := make([]byte, binary.BigEndian.Uint32(h[4:]))
		io.ReadFull(r, p)
		streams = append(streams, h[0])
		texts = append(texts, string(p))
	}
	return
}

func TestAppLogsCopier(t *testing.T) {
	e := NewEngine()
	srv := httptest.NewServer(e)
	defer srv.Close()
	defer e.Close()
	if _, err := e.Run(Spec{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	post := func(text, stream string) {
		t.Helper()
		res, err := http.Post(srv.URL+"/_fake/log/a?stream="+stream, "text/plain", strings.NewReader(text))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	get := func(query string) ([]byte, []string) {
		t.Helper()
		res, err := http.Get(srv.URL + "/v1.44/containers/a/logs?stdout=1&stderr=1&timestamps=1" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return readMsgs(t, res.Body, -1)
	}
	post("first\n"+strings.Repeat("p", logPart+10), "1")
	_, texts := get("")
	if len(texts) != 2 || texts[0][31:] != "first\n" || texts[1][31:] != strings.Repeat("p", logPart) {
		t.Fatalf("a part of 16 KiB and no more: %d messages", len(texts))
	}
	moment := texts[1][:31]
	time.Sleep(1100 * time.Millisecond)
	post("err line\n", "2")
	post(strings.Repeat("p", 5)+"\n", "1")
	streams, texts := get("")
	if len(texts) != 4 || texts[2][31:] != "err line\n" || streams[2] != 2 {
		t.Fatalf("messages %d", len(texts))
	}
	if texts[3] != moment+strings.Repeat("p", 15)+"\n" {
		t.Fatalf("the line's end: %q", texts[3])
	}
	errAt, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(texts[2][:31]))
	_, texts = get(fmt.Sprintf("&since=%d", errAt.Unix()))
	if len(texts) != 2 || texts[0][31:] != "err line\n" || texts[1][:31] != moment {
		t.Fatalf("since: %q", texts)
	}
	post("no newline", "1")
	if _, texts = get(""); len(texts) != 4 {
		t.Fatalf("a line still being written is not logged: %d messages", len(texts))
	}
	if err := e.Die("a"); err != nil {
		t.Fatal(err)
	}
	if _, texts = get("&tail=1"); len(texts) != 1 || texts[0][31:] != "no newline" {
		t.Fatalf("at the stop: %q", texts)
	}
}
