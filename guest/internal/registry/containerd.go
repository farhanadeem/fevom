package registry

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"fev/guest/internal/grpcwire"
)

type Containerd struct {
	conn  *grpcwire.Conn
	blobs string
}

func NewContainerd(socket, ns, content string) *Containerd {
	return &Containerd{conn: grpcwire.Unix(socket, map[string]string{"containerd-namespace": ns}), blobs: filepath.Join(content, "blobs", "sha256")}
}

func Over(c *grpcwire.Conn, ns, content string) *Containerd {
	c.Header = map[string]string{"containerd-namespace": ns}
	return &Containerd{conn: c, blobs: filepath.Join(content, "blobs", "sha256")}
}

func (c *Containerd) Image(ctx context.Context, name string) (Descriptor, error) {
	ans, err := c.conn.Call(ctx, "/containerd.services.images.v1.Images/Get", grpcwire.String(1, name))
	if err != nil {
		if grpcwire.Code(err) == grpcwire.NotFound {
			return Descriptor{}, ErrNotFound
		}
		return Descriptor{}, err
	}
	f, err := grpcwire.Decode(ans)
	if err != nil || len(f.Len[1]) == 0 {
		return Descriptor{}, ErrNotFound
	}
	img, err := grpcwire.Decode(f.Len[1][0])
	if err != nil || len(img.Len[3]) == 0 {
		return Descriptor{}, ErrNotFound
	}
	t, err := grpcwire.Decode(img.Len[3][0])
	if err != nil {
		return Descriptor{}, err
	}
	return Descriptor{MediaType: t.Str(1), Digest: t.Str(2), Size: int64(t.Int(3))}, nil
}

func (c *Containerd) Info(ctx context.Context, digest string) (Info, error) {
	ans, err := c.conn.Call(ctx, "/containerd.services.content.v1.Content/Info", grpcwire.String(1, digest))
	if err != nil {
		if grpcwire.Code(err) == grpcwire.NotFound {
			return Info{}, ErrNotFound
		}
		return Info{}, err
	}
	f, err := grpcwire.Decode(ans)
	if err != nil || len(f.Len[1]) == 0 {
		return Info{}, ErrNotFound
	}
	in, err := grpcwire.Decode(f.Len[1][0])
	if err != nil {
		return Info{}, err
	}
	labels, err := in.Map(5)
	if err != nil {
		return Info{}, err
	}
	return Info{Size: int64(in.Int(2)), Labels: labels}, nil
}

func (c *Containerd) Open(digest string) (*os.File, error) {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || strings.ContainsAny(hex, "/.") || len(hex) != 64 {
		return nil, ErrNotFound
	}
	return os.Open(filepath.Join(c.blobs, hex))
}
