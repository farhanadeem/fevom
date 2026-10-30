package attach

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeEngine struct {
	containers map[string]string
	images     map[string]string
}

func (f *fakeEngine) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest, ok := strings.CutPrefix(r.URL.Path, "/containers/"); ok {
			id := strings.TrimSuffix(rest, "/json")
			if body, ok := f.containers[id]; ok {
				w.Write([]byte(body))
				return
			}
		}
		if rest, ok := strings.CutPrefix(r.URL.Path, "/images/"); ok {
			ref := strings.TrimSuffix(rest, "/json")
			if body, ok := f.images[ref]; ok {
				w.Write([]byte(body))
				return
			}
		}
		http.Error(w, `{"message":"No such object"}`, http.StatusNotFound)
	})
}

func dialTo(srv *httptest.Server) func(ctx context.Context) (net.Conn, error) {
	addr := srv.Listener.Addr().String()
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

func TestResolveRunningContainer(t *testing.T) {
	f := &fakeEngine{containers: map[string]string{
		"web": `{"Id":"ctr123","Name":"/web","State":{"Running":true,"Pid":4242},
			"Config":{"Env":["PATH=/usr/bin"],"WorkingDir":"/app","Image":"nginx:1.29"}}`,
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	r, err := Resolve(context.Background(), dialTo(srv), "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != DebugKindRunning || r.Pid != 4242 || r.ID != "ctr123" || r.Name != "web" || r.Image != "nginx:1.29" {
		t.Fatalf("resolved %+v", r)
	}
	if r.Paused {
		t.Fatal("a running container reported paused")
	}
}

func TestResolvePausedContainer(t *testing.T) {
	f := &fakeEngine{containers: map[string]string{
		"p": `{"Id":"c","Name":"/p","State":{"Running":true,"Paused":true,"Pid":9}}`,
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	r, err := Resolve(context.Background(), dialTo(srv), "p", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != DebugKindRunning || !r.Paused {
		t.Fatalf("expected a paused running container, got %+v", r)
	}
}

func TestResolveStoppedContainerLayers(t *testing.T) {
	f := &fakeEngine{containers: map[string]string{
		"old": `{"Id":"c9","Name":"/old","State":{"Running":false},
			"Config":{"Env":["PATH=/bin"],"WorkingDir":"/","Image":"alpine"},
			"HostConfig":{"CapDrop":["ALL"],"CapAdd":["NET_RAW"]},
			"GraphDriver":{"Name":"overlay2","Data":{"UpperDir":"/var/lib/docker/overlay2/c9/diff","LowerDir":"/var/lib/docker/overlay2/l/diff"}}}`,
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	r, err := Resolve(context.Background(), dialTo(srv), "old", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != DebugKindStopped {
		t.Fatalf("kind %q, want stopped", r.Kind)
	}
	lowers, err := OverlayLowers(r.Graph)
	if err != nil {
		t.Fatal(err)
	}
	if len(lowers) != 2 || lowers[0] != "/var/lib/docker/overlay2/c9/diff" {
		t.Fatalf("lowers %v", lowers)
	}
	if !r.HasCaps || r.Caps != (1<<13) {
		t.Fatalf("inspect caps %#x (hasCaps=%v), want only NET_RAW", r.Caps, r.HasCaps)
	}
	got := StoppedImageCaps(r.Caps, r.HasCaps)
	if got&(1<<13) == 0 || got&(1<<CapSysPtrace) == 0 {
		t.Fatalf("session caps %#x should keep NET_RAW and SYS_PTRACE", got)
	}
	if got&^((1<<13)|(1<<CapSysPtrace)) != 0 {
		t.Fatalf("session caps %#x has more than NET_RAW and SYS_PTRACE", got)
	}
}

func TestResolveImageWhenNoContainer(t *testing.T) {
	f := &fakeEngine{images: map[string]string{
		"alpine:latest": `{"Id":"sha256:img","Config":{"Env":["PATH=/usr/bin"],"WorkingDir":"/"},"RepoTags":["alpine:latest"]}`,
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	r, err := Resolve(context.Background(), dialTo(srv), "alpine:latest", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != DebugKindImage || r.ID != "sha256:img" || r.Name != "alpine:latest" {
		t.Fatalf("resolved %+v", r)
	}
	if r.Image != "" {
		t.Fatal("an image target should carry no container image")
	}
}

func TestResolveImageKindSkipsContainer(t *testing.T) {
	f := &fakeEngine{
		containers: map[string]string{"x": `{"Id":"c","Name":"/x","State":{"Running":true,"Pid":1}}`},
		images:     map[string]string{"x": `{"Id":"i","RepoTags":["x:1"]}`},
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	r, err := Resolve(context.Background(), dialTo(srv), "x", DebugRequestImage)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != DebugKindImage {
		t.Fatalf("kind %q, want image", r.Kind)
	}
}

func TestResolveNotHere(t *testing.T) {
	srv := httptest.NewServer((&fakeEngine{}).handler())
	defer srv.Close()
	_, err := Resolve(context.Background(), dialTo(srv), "ghost", "")
	if err == nil {
		t.Fatal("a missing target resolved")
	}
	if _, ok := err.(*NotHereError); !ok {
		t.Fatalf("error %T %v, want NotHereError", err, err)
	}
	if !strings.Contains(err.Error(), "docker pull ghost first") {
		t.Fatalf("message %q", err.Error())
	}
}

func TestResolveIDPrefix(t *testing.T) {
	f := &fakeEngine{containers: map[string]string{
		"abc": `{"Id":"abc123def","Name":"/w","State":{"Running":true,"Pid":7}}`,
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	r, err := Resolve(context.Background(), dialTo(srv), "abc", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "abc123def" {
		t.Fatalf("id %q", r.ID)
	}
}
