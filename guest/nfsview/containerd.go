package nfsview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Containerd struct {
	Dial        func(ctx context.Context) (net.Conn, error)
	Namespace   string
	Snapshotter string

	client *http.Client
}

type ContainerdMount struct {
	Type, Source, Target string
	Options              []string
}

const (
	leasesService    = "/containerd.services.leases.v1.Leases/"
	snapshotsService = "/containerd.services.snapshots.v1.Snapshots/"
)

const callWait = 10 * time.Second

func (c *Containerd) http() *http.Client {
	if c.client == nil {
		var p http.Protocols
		p.SetUnencryptedHTTP2(true)
		c.client = &http.Client{Transport: &http.Transport{
			Protocols:         &p,
			DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return c.Dial(ctx) },
			DisableKeepAlives: true,
		}}
	}
	return c.client
}

func (c *Containerd) call(ctx context.Context, method string, msg []byte, lease string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callWait)
	defer cancel()
	body := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(body[1:], uint32(len(msg)))
	copy(body[5:], msg)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://containerd"+method, bytes.NewReader(body))
	req.Header.Set("content-type", "application/grpc")
	req.Header.Set("te", "trailers")
	req.Header.Set("containerd-namespace", c.Namespace)
	if lease != "" {
		req.Header.Set("containerd-lease", lease)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("nfsview: containerd %s: %w", method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("nfsview: containerd %s: %w", method, err)
	}
	status := resp.Trailer.Get("grpc-status")
	if status == "" {
		status = resp.Header.Get("grpc-status")
	}
	if resp.StatusCode != http.StatusOK || status != "0" {
		m := resp.Trailer.Get("grpc-message")
		if m == "" {
			m = resp.Header.Get("grpc-message")
		}
		return nil, &GRPCError{Method: method, Code: atoiOr(status, -1), Message: m, HTTP: resp.StatusCode}
	}
	if len(data) < 5 {
		return nil, nil
	}
	n := int(binary.BigEndian.Uint32(data[1:5]))
	if data[0] != 0 || 5+n > len(data) {
		return nil, fmt.Errorf("nfsview: containerd %s: an answer framed wrongly", method)
	}
	return data[5 : 5+n], nil
}

type GRPCError struct {
	Method  string
	Code    int
	Message string
	HTTP    int
}

func (e *GRPCError) Error() string {
	return fmt.Sprintf("nfsview: containerd %s: code %d: %s", e.Method, e.Code, e.Message)
}

const (
	grpcNotFound      = 5
	grpcAlreadyExists = 6
)

func IsNotFound(err error) bool {
	var g *GRPCError
	return errors.As(err, &g) && g.Code == grpcNotFound
}

func IsAlreadyExists(err error) bool {
	var g *GRPCError
	return errors.As(err, &g) && g.Code == grpcAlreadyExists
}

func atoiOr(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return d
}

func (c *Containerd) CreateLease(ctx context.Context, id string) error {
	_, err := c.call(ctx, leasesService+"Create", pbString(1, id), "")
	return err
}

func (c *Containerd) DeleteLease(ctx context.Context, id string) error {
	_, err := c.call(ctx, leasesService+"Delete", pbString(1, id), "")
	return err
}

func (c *Containerd) Leases(ctx context.Context) ([]string, error) {
	ans, err := c.call(ctx, leasesService+"List", nil, "")
	if err != nil {
		return nil, err
	}
	f, err := pbFields(ans)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, lb := range f[1] {
		lf, err := pbFields(lb)
		if err != nil {
			return nil, err
		}
		if len(lf[1]) > 0 {
			out = append(out, string(lf[1][0]))
		}
	}
	return out, nil
}

func (c *Containerd) View(ctx context.Context, lease, key, parent string) ([]ContainerdMount, error) {
	msg := append(append(pbString(1, c.Snapshotter), pbString(2, key)...), pbString(3, parent)...)
	ans, err := c.call(ctx, snapshotsService+"View", msg, lease)
	if err != nil {
		return nil, err
	}
	return decodeMounts(ans)
}

func (c *Containerd) Mounts(ctx context.Context, key string) ([]ContainerdMount, error) {
	ans, err := c.call(ctx, snapshotsService+"Mounts", append(pbString(1, c.Snapshotter), pbString(2, key)...), "")
	if err != nil {
		return nil, err
	}
	return decodeMounts(ans)
}

func (c *Containerd) Remove(ctx context.Context, key string) error {
	_, err := c.call(ctx, snapshotsService+"Remove", append(pbString(1, c.Snapshotter), pbString(2, key)...), "")
	return err
}

func pbString(field int, s string) []byte {
	b := binary.AppendUvarint(nil, uint64(field<<3|2))
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

func pbFields(b []byte) (map[int][][]byte, error) {
	out := map[int][][]byte{}
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errors.New("nfsview: a protobuf tag cut short")
		}
		b = b[n:]
		field, wire := int(tag>>3), tag&7
		switch wire {
		case 0:
			_, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, errors.New("nfsview: a protobuf varint cut short")
			}
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return nil, errors.New("nfsview: protobuf cut short")
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return nil, errors.New("nfsview: protobuf cut short")
			}
			b = b[4:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return nil, errors.New("nfsview: a protobuf field cut short")
			}
			out[field] = append(out[field], b[n:n+int(l)])
			b = b[n+int(l):]
		default:
			return nil, fmt.Errorf("nfsview: protobuf wire type %d", wire)
		}
	}
	return out, nil
}

func decodeMounts(b []byte) ([]ContainerdMount, error) {
	f, err := pbFields(b)
	if err != nil {
		return nil, err
	}
	var out []ContainerdMount
	for _, mb := range f[1] {
		mf, err := pbFields(mb)
		if err != nil {
			return nil, err
		}
		first := func(n int) string {
			if len(mf[n]) > 0 {
				return string(mf[n][0])
			}
			return ""
		}
		m := ContainerdMount{Type: first(1), Source: first(2), Target: first(3)}
		for _, o := range mf[4] {
			m.Options = append(m.Options, string(o))
		}
		out = append(out, m)
	}
	return out, nil
}

func ChainID(diffIDs []string) string {
	if len(diffIDs) == 0 {
		return ""
	}
	chain := diffIDs[0]
	for _, d := range diffIDs[1:] {
		h := sha256.Sum256([]byte(chain + " " + d))
		chain = "sha256:" + hex.EncodeToString(h[:])
	}
	return chain
}

func ViewLayers(mounts []ContainerdMount, from, root string) ([]string, error) {
	if len(mounts) != 1 {
		return nil, fmt.Errorf("nfsview: a view of %d mounts", len(mounts))
	}
	m := mounts[0]
	var dirs []string
	switch m.Type {
	case "overlay":
		for _, o := range m.Options {
			if v, ok := strings.CutPrefix(o, "lowerdir="); ok {
				dirs = strings.Split(v, ":")
			}
		}
	case "bind":
		dirs = []string{m.Source}
	default:
		return nil, fmt.Errorf("nfsview: a view mounted as %q", m.Type)
	}
	if len(dirs) == 0 {
		return nil, errors.New("nfsview: a view with no layers")
	}
	for i, d := range dirs {
		rest, ok := strings.CutPrefix(d, from+"/")
		if !ok || strings.Contains(rest, "..") {
			return nil, fmt.Errorf("nfsview: a layer outside %s: %s", from, d)
		}
		dirs[i] = root + "/" + rest
	}
	return dirs, nil
}
