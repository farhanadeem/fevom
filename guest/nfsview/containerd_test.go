package nfsview

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"reflect"
	"sync"
	"testing"
)

type fakeContainerd struct {
	mu     sync.Mutex
	asked  []string
	leases map[string]bool
	views  map[string]string
}

func (f *fakeContainerd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("content-type", "application/grpc")
	w.Header().Set("Trailer", "grpc-status, grpc-message")
	fail := func(code, msg string) {
		w.WriteHeader(200)
		w.Header().Set("grpc-status", code)
		w.Header().Set("grpc-message", msg)
	}
	if r.ProtoMajor != 2 || r.Header.Get("containerd-namespace") != "moby" || len(body) < 5 {
		fail("3", "bad request")
		return
	}
	fields, err := pbFields(body[5:])
	if err != nil {
		fail("3", err.Error())
		return
	}
	str := func(n int) string {
		if len(fields[n]) > 0 {
			return string(fields[n][0])
		}
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, r.URL.Path+" lease="+r.Header.Get("containerd-lease"))
	var ans []byte
	switch r.URL.Path {
	case leasesService + "Create":
		if f.leases[str(1)] {
			fail("6", "lease exists")
			return
		}
		f.leases[str(1)] = true
	case leasesService + "Delete":
		if !f.leases[str(1)] {
			fail("5", "no lease")
			return
		}
		delete(f.leases, str(1))
	case leasesService + "List":
		for id := range f.leases {
			l := pbString(1, id)
			ans = append(ans, append(binary.AppendUvarint(binary.AppendUvarint(nil, 1<<3|2), uint64(len(l))), l...)...)
		}
	case snapshotsService + "View":
		if str(1) != "overlayfs" {
			fail("3", "snapshotter")
			return
		}
		f.views[str(2)] = str(3)
		m := append(append(pbString(1, "overlay"), pbString(2, "overlay")...),
			pbString(4, "lowerdir=/var/lib/docker/containerd/daemon/snap/2/fs:/var/lib/docker/containerd/daemon/snap/1/fs")...)
		m = append(m, pbString(4, "index=on")...)
		ans = append(binary.AppendUvarint(binary.AppendUvarint(nil, 1<<3|2), uint64(len(m))), m...)
	case snapshotsService + "Remove":
		if _, ok := f.views[str(2)]; !ok {
			fail("5", "no snapshot")
			return
		}
		delete(f.views, str(2))
	default:
		fail("12", "unimplemented")
		return
	}
	frame := make([]byte, 5+len(ans))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(ans)))
	copy(frame[5:], ans)
	w.WriteHeader(200)
	w.Write(frame)
	w.Header().Set("grpc-status", "0")
}

func TestTheContainerdCallsAreGRPCs(t *testing.T) {
	fake := &fakeContainerd{leases: map[string]bool{}, views: map[string]string{}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: fake, Protocols: &p}
	go srv.Serve(l)
	defer srv.Close()
	c := &Containerd{Dial: func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", l.Addr().String()) }, Namespace: "moby", Snapshotter: "overlayfs"}
	ctx := context.Background()
	if err := c.CreateLease(ctx, "fev-view-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateLease(ctx, "fev-view-1"); !IsAlreadyExists(err) {
		t.Errorf("a second lease: %v", err)
	}
	if ids, err := c.Leases(ctx); err != nil || !reflect.DeepEqual(ids, []string{"fev-view-1"}) {
		t.Errorf("leases: %v %v", ids, err)
	}
	mounts, err := c.View(ctx, "fev-view-1", "fev-view-1", "sha256:chain")
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := ViewLayers(mounts, "/var/lib/docker", "/data/@docker")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/data/@docker/containerd/daemon/snap/2/fs", "/data/@docker/containerd/daemon/snap/1/fs"}; !reflect.DeepEqual(dirs, want) {
		t.Errorf("layers %v, want %v", dirs, want)
	}
	if err := c.Remove(ctx, "fev-view-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, "fev-view-1"); !IsNotFound(err) {
		t.Errorf("a second remove: %v", err)
	}
	if err := c.DeleteLease(ctx, "fev-view-1"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		leasesService + "Create lease=", leasesService + "Create lease=", leasesService + "List lease=", snapshotsService + "View lease=fev-view-1",
		snapshotsService + "Remove lease=", snapshotsService + "Remove lease=", leasesService + "Delete lease=",
	}
	if !reflect.DeepEqual(fake.asked, want) {
		t.Errorf("asked %v", fake.asked)
	}
	if _, err := ViewLayers([]ContainerdMount{{Type: "bind", Source: "/elsewhere/fs"}}, "/var/lib/docker", "/data/@docker"); err == nil {
		t.Error("a layer outside the engine's root was taken")
	}
	if d, err := ViewLayers([]ContainerdMount{{Type: "bind", Source: "/var/lib/docker/c/1/fs", Options: []string{"ro", "rbind"}}}, "/var/lib/docker", "/data/@docker"); err != nil || !reflect.DeepEqual(d, []string{"/data/@docker/c/1/fs"}) {
		t.Errorf("one layer: %v %v", d, err)
	}
}

func TestChainID(t *testing.T) {
	if got := ChainID([]string{"sha256:a"}); got != "sha256:a" {
		t.Error(got)
	}
	h := sha256.Sum256([]byte("sha256:a sha256:b"))
	ab := "sha256:" + hex.EncodeToString(h[:])
	h = sha256.Sum256([]byte(ab + " sha256:c"))
	if got, want := ChainID([]string{"sha256:a", "sha256:b", "sha256:c"}), "sha256:"+hex.EncodeToString(h[:]); got != want {
		t.Errorf("%s, want %s", got, want)
	}
}
