package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fev/guest/internal/grpcwire"
)

type fakeStore struct {
	dir    string
	images map[string]Descriptor
	labels map[string]map[string]string
}

func (f *fakeStore) Image(_ context.Context, name string) (Descriptor, error) {
	if d, ok := f.images[name]; ok {
		return d, nil
	}
	return Descriptor{}, ErrNotFound
}

func (f *fakeStore) Info(_ context.Context, digest string) (Info, error) {
	st, err := os.Stat(filepath.Join(f.dir, digest))
	if err != nil {
		return Info{}, ErrNotFound
	}
	return Info{Size: st.Size(), Labels: f.labels[digest]}, nil
}

func (f *fakeStore) Open(digest string) (*os.File, error) {
	return os.Open(filepath.Join(f.dir, digest))
}

func (f *fakeStore) blob(t *testing.T, data string, labels map[string]string) string {
	t.Helper()
	s := sha256.Sum256([]byte(data))
	d := "sha256:" + hex.EncodeToString(s[:])
	os.WriteFile(filepath.Join(f.dir, d), []byte(data), 0o644)
	if labels != nil {
		f.labels[d] = labels
	}
	return d
}

const indexType = "application/vnd.oci.image.index.v1+json"

func newFake(t *testing.T) (*fakeStore, *httptest.Server) {
	f := &fakeStore{dir: t.TempDir(), images: map[string]Descriptor{}, labels: map[string]map[string]string{}}
	ts := httptest.NewServer(&Server{Store: f})
	t.Cleanup(ts.Close)
	return f, ts
}

func get(t *testing.T, method, url string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestTheRegistryAnswersDockersOwnTagsByRegistry(t *testing.T) {
	f, ts := newFake(t)
	built := f.blob(t, `{"mediaType":"`+indexType+`","manifests":[]}`, nil)
	other := f.blob(t, `{"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{}}`, nil)
	pulled := f.blob(t, `{"mediaType":"`+indexType+`","pulled":true}`, map[string]string{SourceLabel + "docker.io": "library/nginx,library/other"})
	f.images["docker.io/me/app:1"] = Descriptor{MediaType: indexType, Digest: built}
	f.images["ghcr.io/me/app:1"] = Descriptor{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: other}
	f.images["docker.io/library/nginx:latest"] = Descriptor{MediaType: indexType, Digest: pulled}
	f.images["docker.io/library/web:latest"] = Descriptor{MediaType: indexType, Digest: pulled}
	f.images["localhost:5000/app:dev"] = Descriptor{MediaType: indexType, Digest: built}
	fromPort := f.blob(t, `{"mediaType":"`+indexType+`","port":true}`, map[string]string{SourceLabel + "localhost": "pulled"})
	f.images["localhost:5000/pulled:1"] = Descriptor{MediaType: indexType, Digest: fromPort}

	cases := []struct {
		path, digest string
		status       int
	}{
		{"/v2/me/app/manifests/1?ns=docker.io", built, 200},
		{"/v2/me/app/manifests/1?ns=ghcr.io", other, 200},
		{"/v2/me/app/manifests/1", "", 404},
		{"/v2/me/app/manifests/2?ns=docker.io", "", 404},
		{"/v2/library/nginx/manifests/latest?ns=docker.io", "", 404},
		{"/v2/library/web/manifests/latest?ns=docker.io", pulled, 200},
		{"/v2/app/manifests/dev?ns=localhost:5000", built, 200},
		{"/v2/app/manifests/dev?ns=evil/../x", "", 404},
		{"/v2/pulled/manifests/1?ns=localhost:5000", "", 404},
	}
	for _, c := range cases {
		resp, body := get(t, "GET", ts.URL+c.path)
		if resp.StatusCode != c.status {
			t.Errorf("%s: %d %s", c.path, resp.StatusCode, body)
			continue
		}
		if c.status == 200 {
			if got := resp.Header.Get("Docker-Content-Digest"); got != c.digest {
				t.Errorf("%s: digest %s, want %s", c.path, got, c.digest)
			}
			data, _ := os.ReadFile(filepath.Join(f.dir, c.digest))
			if body != string(data) {
				t.Errorf("%s: body %q", c.path, body)
			}
		}
	}
	resp, body := get(t, "HEAD", ts.URL+"/v2/me/app/manifests/1?ns=docker.io")
	if resp.StatusCode != 200 || body != "" || resp.Header.Get("Content-Type") != indexType || resp.Header.Get("Docker-Content-Digest") != built {
		t.Errorf("HEAD %d %q %v", resp.StatusCode, body, resp.Header)
	}
}

func TestTheRegistryServesDigestsAndRefusesWrites(t *testing.T) {
	f, ts := newFake(t)
	m := f.blob(t, `{"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`, map[string]string{SourceLabel + "docker.io": "library/nginx"})
	layer := f.blob(t, strings.Repeat("layer", 1000), nil)
	resp, _ := get(t, "GET", ts.URL+"/v2/library/nginx/manifests/"+m+"?ns=docker.io")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/vnd.docker.distribution.manifest.v2+json" {
		t.Errorf("manifest by digest: %d %v", resp.StatusCode, resp.Header)
	}
	resp, body := get(t, "GET", ts.URL+"/v2/library/nginx/blobs/"+layer)
	if resp.StatusCode != 200 || len(body) != 5000 {
		t.Errorf("blob: %d %d", resp.StatusCode, len(body))
	}
	req, _ := http.NewRequest("GET", ts.URL+"/v2/library/nginx/blobs/"+layer, nil)
	req.Header.Set("Range", "bytes=4990-")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 206 || string(b) != "layerlayer" {
		t.Errorf("range: %d %q", r2.StatusCode, b)
	}
	absent := "sha256:" + strings.Repeat("0", 64)
	if resp, _ := get(t, "GET", ts.URL+"/v2/library/nginx/manifests/"+absent+"?ns=docker.io"); resp.StatusCode != 404 {
		t.Errorf("absent manifest: %d", resp.StatusCode)
	}
	if resp, _ := get(t, "GET", ts.URL+"/v2/library/nginx/blobs/"+absent); resp.StatusCode != 404 {
		t.Errorf("absent blob: %d", resp.StatusCode)
	}
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		if resp, _ := get(t, method, ts.URL+"/v2/me/app/blobs/uploads/"); resp.StatusCode != 405 {
			t.Errorf("%s: %d", method, resp.StatusCode)
		}
	}
	if resp, _ := get(t, "GET", ts.URL+"/v2/"); resp.StatusCode != 200 || resp.Header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
		t.Errorf("/v2/: %d", resp.StatusCode)
	}
}

func TestTheContainerdStoreReadsTheAPI(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "reg")
	defer os.RemoveAll(dir)
	digest := "sha256:" + strings.Repeat("ab", 32)
	conn, stop, err := grpcwire.Fake(filepath.Join(dir, "s"), func(method string, h http.Header, msg grpcwire.Fields) ([]byte, int, string) {
		if h.Get("containerd-namespace") != "moby" {
			return nil, 3, "namespace"
		}
		switch method {
		case "/containerd.services.images.v1.Images/Get":
			if msg.Str(1) != "docker.io/me/app:1" {
				return nil, grpcwire.NotFound, "image not found"
			}
			target := append(append(grpcwire.String(1, indexType), grpcwire.String(2, digest)...), grpcwire.Varint(3, 512)...)
			img := append(grpcwire.String(1, msg.Str(1)), grpcwire.Bytes(3, target)...)
			return grpcwire.Bytes(1, img), 0, ""
		case "/containerd.services.content.v1.Content/Info":
			if msg.Str(1) != digest {
				return nil, grpcwire.NotFound, "content not found"
			}
			info := append(append(grpcwire.String(1, digest), grpcwire.Varint(2, 512)...), grpcwire.MapEntry(5, SourceLabel+"docker.io", "library/x")...)
			return grpcwire.Bytes(1, info), 0, ""
		}
		return nil, grpcwire.Unimplemented, method
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	s := Over(conn, "moby", dir)
	d, err := s.Image(context.Background(), "docker.io/me/app:1")
	if err != nil || d.Digest != digest || d.MediaType != indexType || d.Size != 512 {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := s.Image(context.Background(), "docker.io/me/app:2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("absent image: %v", err)
	}
	in, err := s.Info(context.Background(), digest)
	if err != nil || in.Size != 512 || in.Labels[SourceLabel+"docker.io"] != "library/x" {
		t.Fatalf("%+v %v", in, err)
	}
	if _, err := s.Open("sha256:../../etc"); err == nil {
		t.Error("a path was opened")
	}
}

func TestTheCheckedListenerRefusesOthers(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	n := 0
	cl := &Checked{Listener: l, Check: func(net.Conn) (bool, error) { n++; return n > 1, nil }}
	defer cl.Close()
	var dialled []net.Conn
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		dialled = append(dialled, c)
	}
	defer func() {
		for _, c := range dialled {
			c.Close()
		}
	}()
	c, err := cl.Accept()
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if cl.refused != 1 || n != 2 {
		t.Errorf("refused %d, checked %d", cl.refused, n)
	}
}
