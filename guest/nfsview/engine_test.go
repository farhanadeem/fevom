package nfsview

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeEngine struct {
	mu         sync.Mutex
	containers []dockerContainer
	volumes    []dockerVolume
	images     []dockerImage
	layers     map[string][]string
	inspects   int
	events     chan string
	subscribed chan struct{}
}

func (e *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case r.URL.Path == "/events":
		e.mu.Unlock()
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		e.subscribed <- struct{}{}
		for ev := range e.events {
			if ev == "" {
				break
			}
			fmt.Fprintln(w, ev)
			w.(http.Flusher).Flush()
		}
		e.mu.Lock()
	case r.URL.Path == "/containers/json":
		out := e.containers
		if f := r.URL.Query().Get("filters"); f != "" {
			var q struct{ ID []string }
			json.Unmarshal([]byte(f), &q)
			out = nil
			for _, c := range e.containers {
				if len(q.ID) > 0 && c.ID == q.ID[0] {
					out = append(out, c)
				}
			}
		}
		json.NewEncoder(w).Encode(out)
	case r.URL.Path == "/volumes":
		json.NewEncoder(w).Encode(map[string]any{"Volumes": e.volumes})
	case r.URL.Path == "/images/json":
		json.NewEncoder(w).Encode(e.images)
	case strings.HasPrefix(r.URL.Path, "/images/"):
		e.inspects++
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/json")
		json.NewEncoder(w).Encode(map[string]any{"RootFS": map[string]any{"Layers": e.layers[id]}})
	default:
		http.NotFound(w, r)
	}
}

func newFollower(t *testing.T) (*Follower, *fakeEngine, *fakeOps, *Objects) {
	t.Helper()
	eng := &fakeEngine{layers: map[string][]string{}, events: make(chan string, 16), subscribed: make(chan struct{}, 4)}
	srv := httptest.NewServer(eng)
	t.Cleanup(func() { close(eng.events); srv.Close() })
	o, ops, _ := newObjects(t)
	f := &Follower{
		Objects:  o,
		Dial:     func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", srv.Listener.Addr().String()) },
		EngineNS: func() (string, func(), bool) { return "/proc/self/fd/7", func() {}, true },
		Bundle:   func(id string) (int, []string, error) { return 77, []string{"/data"}, nil },
		DataRoot: "/data/@docker",
		Logf:     func(f string, a ...any) { t.Logf(f, a...) },
	}
	return f, eng, ops, o
}

func cid(c byte) string { return strings.Repeat(string(c), 64) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never: %s", what)
}

func TestTheFollowerReconcilesThenFollows(t *testing.T) {
	f, eng, ops, o := newFollower(t)
	a, b, c := cid('a'), cid('b'), cid('c')
	eng.containers = []dockerContainer{
		{ID: a, Names: []string{"/web"}, State: "running"},
		{ID: b, Names: []string{"/db", "/web/db"}, State: "running"},
		{ID: c, Names: []string{"/old"}, State: "exited"},
	}
	eng.volumes = []dockerVolume{
		{Name: "pg", Driver: "local", Mountpoint: "/var/lib/docker/volumes/pg/_data"},
		{Name: "far", Driver: "nfsplugin", Mountpoint: "/mnt/far"},
	}
	eng.images = []dockerImage{{ID: "sha256:" + cid('1'), RepoTags: []string{"nginx:1.29", "web:latest"}}, {ID: "sha256:" + cid('2')}}
	eng.layers["sha256:"+cid('1')] = []string{"sha256:l1"}
	eng.layers["sha256:"+cid('2')] = []string{"sha256:l2"}
	o.Release(b)
	if err := o.Attach("ns", c, "old"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	<-eng.subscribed
	waitFor(t, "the reconciliation", func() bool { return len(o.List()) >= 6 })
	var paths []string
	for _, v := range o.List() {
		paths = append(paths, v.Kind+":"+v.Path)
	}
	want := []string{
		"container:docker/containers/web", "bind:docker/containers/web/data",
		"image:docker/images/222222222222", "image:docker/images/nginx:1.29", "image:docker/images/web:latest",
		"volume:docker/volumes/pg",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("shown:\n%v\nwant\n%v", paths, want)
	}
	if !strings.Contains(strings.Join(ops.calls, "\n"), "clone /proc/self/fd/7 pid=77 /data /v/docker/containers/web/data") {
		t.Errorf("binds: %v", ops.calls)
	}
	if got := ops.mounts["/v/docker/volumes/pg"]; got != "/data/@docker/volumes/pg/_data" {
		t.Errorf("the volume's source: %q", got)
	}
	eng.events <- `{"Type":"container","Action":"rename","Actor":{"ID":"` + a + `","Attributes":{"name":"api","oldName":"/web"}}}`
	waitFor(t, "the rename", func() bool { return o.Containers()[a] == "api" })
	eng.mu.Lock()
	eng.volumes = append(eng.volumes, dockerVolume{Name: "cache", Driver: "local", Mountpoint: "/var/lib/docker/volumes/cache/_data"})
	eng.images[0].RepoTags = []string{"nginx:1.29"}
	inspects := eng.inspects
	eng.mu.Unlock()
	eng.events <- `{"Type":"volume","Action":"create","Actor":{"ID":"cache"}}`
	eng.events <- `{"Type":"image","Action":"untag","Actor":{"ID":"sha256:` + cid('1') + `"}}`
	eng.events <- `{"Type":"container","Action":"destroy","Actor":{"ID":"` + a + `","Attributes":{"name":"api"}}}`
	waitFor(t, "the events", func() bool {
		var n []string
		for _, v := range o.List() {
			n = append(n, v.Path)
		}
		s := strings.Join(n, " ")
		return !strings.Contains(s, "containers/api") && strings.Contains(s, "volumes/cache") && !strings.Contains(s, "web:latest")
	})
	eng.mu.Lock()
	if eng.inspects != inspects {
		t.Errorf("images inspected again: %d, were %d", eng.inspects, inspects)
	}
	eng.mu.Unlock()
	eng.events <- ""
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not end with the stream")
	}
}

func TestANameComesFromAListOfTheOneID(t *testing.T) {
	f, eng, _, _ := newFollower(t)
	eng.containers = []dockerContainer{{ID: cid('e'), Names: []string{"/fresh"}, State: "created"}}
	if n := f.Name(context.Background(), cid('e')); n != "fresh" {
		t.Errorf("name: %q", n)
	}
	if n := f.Name(context.Background(), cid('f')); n != "" {
		t.Errorf("an unknown id's name: %q", n)
	}
}

func TestBindDestsAreVolumesBindsAndTmpfs(t *testing.T) {
	spec := `{"mounts":[
	 {"destination":"/proc","type":"proc","source":"proc"},
	 {"destination":"/dev","type":"tmpfs","source":"tmpfs"},
	 {"destination":"/dev/shm","type":"tmpfs","source":"shm"},
	 {"destination":"/sys/fs/cgroup","type":"cgroup","source":"cgroup"},
	 {"destination":"/data","type":"bind","source":"/var/lib/docker/volumes/vol1/_data","options":["rbind"]},
	 {"destination":"/t","type":"tmpfs","source":"tmpfs","options":["noexec"]},
	 {"destination":"/src","type":"bind","source":"/Users/me/src","options":["rbind"]},
	 {"destination":"/m","source":"/mnt/mac/x","options":["rbind"]},
	 {"destination":"/host-etc","source":"/etc","options":["rbind","ro"]},
	 {"destination":"/etc/hosts","type":"bind","source":"/var/lib/docker/containers/x/hosts","options":["rbind"]},
	 {"destination":"/","type":"bind","source":"/x"},
	 {"destination":"/data/","type":"bind","source":"/y"}
	]}`
	got, err := BindDests([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/data", "/t", "/host-etc"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%v, want %v", got, want)
	}
}

func TestImageNames(t *testing.T) {
	if got := ImageNames("sha256:0123456789abcdef", []string{"b:1", "a:2"}); !reflect.DeepEqual(got, []string{"a:2", "b:1"}) {
		t.Error(got)
	}
	if got := ImageNames("sha256:0123456789abcdef", []string{"<none>:<none>"}); !reflect.DeepEqual(got, []string{"0123456789ab"}) {
		t.Error(got)
	}
}
