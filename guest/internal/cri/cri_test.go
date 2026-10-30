package cri

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"fev/guest/internal/grpcwire"
)

type fakeRuntime struct {
	mu        sync.Mutex
	running   map[string]Container
	sandboxes []string
	stops     []string
	sandboxOp []string
}

func (f *fakeRuntime) handle(method string, _ http.Header, msg grpcwire.Fields) ([]byte, int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case runtimeService + "ListContainers":
		want := -1
		if len(msg.Len[1]) > 0 {
			ff, _ := grpcwire.Decode(msg.Len[1][0])
			if len(ff.Len[2]) > 0 {
				sf, _ := grpcwire.Decode(ff.Len[2][0])
				want = int(sf.Int(1))
			}
		}
		var ans []byte
		var ids []string
		for id := range f.running {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			c := f.running[id]
			if want >= 0 && c.State != want {
				continue
			}
			m := append(grpcwire.String(1, c.ID), grpcwire.String(2, c.Sandbox)...)
			m = append(m, grpcwire.Bytes(3, grpcwire.String(1, c.Name))...)
			m = append(m, grpcwire.Varint(6, uint64(c.State))...)
			for k, v := range c.Annotations {
				m = append(m, grpcwire.MapEntry(9, k, v)...)
			}
			ans = append(ans, grpcwire.Bytes(1, m)...)
		}
		return ans, 0, ""
	case runtimeService + "StopContainer":
		id := msg.Str(1)
		c, ok := f.running[id]
		if !ok {
			return nil, grpcwire.NotFound, "no such container"
		}
		c.State = ContainerExited
		f.running[id] = c
		f.stops = append(f.stops, id+" "+time.Duration(msg.Int(2)*uint64(time.Second)).String())
		return nil, 0, ""
	case runtimeService + "ListPodSandbox":
		var ans []byte
		for _, id := range f.sandboxes {
			ans = append(ans, grpcwire.Bytes(1, grpcwire.String(1, id))...)
		}
		return ans, 0, ""
	case runtimeService + "StopPodSandbox":
		f.sandboxOp = append(f.sandboxOp, "stop "+msg.Str(1))
		return nil, 0, ""
	case runtimeService + "RemovePodSandbox":
		f.sandboxOp = append(f.sandboxOp, "remove "+msg.Str(1))
		return nil, 0, ""
	}
	return nil, grpcwire.Unimplemented, "unknown " + method
}

func serve(t *testing.T, f *fakeRuntime) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cri")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	conn, stop, err := grpcwire.Fake(filepath.Join(dir, "s"), f.handle)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return Over(conn)
}

func TestDrainStopsEachPodWithItsGrace(t *testing.T) {
	f := &fakeRuntime{running: map[string]Container{
		"aaa": {ID: "aaa", Sandbox: "s1", Name: "db", State: ContainerRunning, Annotations: map[string]string{GraceAnnotation: "45"}},
		"bbb": {ID: "bbb", Sandbox: "s1", Name: "side", State: ContainerRunning, Annotations: map[string]string{GraceAnnotation: "2"}},
		"ccc": {ID: "ccc", Sandbox: "s2", Name: "web", State: ContainerRunning},
		"ddd": {ID: "ddd", Sandbox: "s2", Name: "init", State: ContainerExited, Annotations: map[string]string{GraceAnnotation: "1"}},
		"eee": {ID: "eee", Sandbox: "s3", Name: "now", State: ContainerRunning, Annotations: map[string]string{GraceAnnotation: "0"}},
	}, sandboxes: []string{"s1", "s2", "s3"}}
	c := serve(t, f)
	d := c.Drain(context.Background(), 40*time.Second)
	if len(d.Errors) > 0 {
		t.Fatalf("errors: %v", d.Errors)
	}
	sort.Strings(f.stops)
	want := []string{"aaa 40s", "bbb 2s", "ccc 30s", "eee 0s"}
	if !reflect.DeepEqual(f.stops, want) {
		t.Errorf("stops %v, want %v", f.stops, want)
	}
	if d.Containers != 4 || d.Sandboxes != 3 {
		t.Errorf("drained %+v", d)
	}
	wantSB := []string{"stop s1", "remove s1", "stop s2", "remove s2", "stop s3", "remove s3"}
	if !reflect.DeepEqual(f.sandboxOp, wantSB) {
		t.Errorf("sandbox calls %v, want %v", f.sandboxOp, wantSB)
	}
}

func TestContainersReadTheirAnnotations(t *testing.T) {
	f := &fakeRuntime{running: map[string]Container{
		"x": {ID: "x", Sandbox: "sb", Name: "app", State: ContainerRunning, Annotations: map[string]string{GraceAnnotation: "7", "other": "v"}},
	}}
	cs, err := serve(t, f).Containers(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Name != "app" || cs[0].Sandbox != "sb" || cs[0].Grace() != 7*time.Second || cs[0].Annotations["other"] != "v" {
		t.Fatalf("got %+v", cs)
	}
}

func TestDrainWithNoRuntimeSaysSo(t *testing.T) {
	c := New("/tmp/no-such-cri-socket")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d := c.Drain(ctx, time.Second)
	if len(d.Errors) != 2 || d.Containers != 0 {
		t.Fatalf("got %+v", d)
	}
}
