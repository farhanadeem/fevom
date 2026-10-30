package nfsview

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"testing"
)

func pbVarintField(field int, v uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, uint64(field<<3)), v)
}

func grpcFrame(m []byte) []byte {
	f := make([]byte, 5+len(m))
	binary.BigEndian.PutUint32(f[1:], uint32(len(m)))
	copy(f[5:], m)
	return f
}

func TestANamespacesSnapshotsAreListedAndTheirUsageSummed(t *testing.T) {
	sizes := map[string]uint64{"sha256:a": 4096, "sha256:b": 7 << 20, "pod-rootfs": 12288}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/grpc")
		w.Header().Set("Trailer", "grpc-status, grpc-message")
		if r.Header.Get("containerd-namespace") != "k8s.io" {
			w.WriteHeader(200)
			w.Header().Set("grpc-status", "3")
			return
		}
		f, _ := pbFields(body[5:])
		w.WriteHeader(200)
		switch r.URL.Path {
		case snapshotsService + "List":
			for _, names := range [][]string{{"sha256:a", "sha256:b"}, {"pod-rootfs", "gone"}} {
				var m []byte
				for _, n := range names {
					info := pbString(1, n)
					m = append(m, append(binary.AppendUvarint(binary.AppendUvarint(nil, 1<<3|2), uint64(len(info))), info...)...)
				}
				w.Write(grpcFrame(m))
			}
			w.Header().Set("grpc-status", "0")
		case snapshotsService + "Usage":
			key := string(f[2][0])
			size, ok := sizes[key]
			if !ok {
				w.Header().Set("grpc-status", "5")
				return
			}
			w.Write(grpcFrame(append(pbVarintField(1, size), pbVarintField(2, 3)...)))
			w.Header().Set("grpc-status", "0")
		}
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: h, Protocols: &p}
	go srv.Serve(l)
	defer srv.Close()
	c := &Containerd{Dial: func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", l.Addr().String()) }, Namespace: "k8s.io", Snapshotter: "overlayfs"}
	n, count, err := c.SnapshotsUsage(context.Background())
	if err != nil || n != 4096+7<<20+12288 || count != 3 {
		t.Fatalf("usage %d of %d: %v", n, count, err)
	}
	c.Namespace = "moby"
	if _, _, err := c.SnapshotsUsage(context.Background()); err == nil {
		t.Fatal("another namespace's answer was taken")
	}
}
