package nfsview

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
)

func (c *Containerd) SnapshotsUsage(ctx context.Context) (bytes int64, count int, err error) {
	frames, err := c.stream(ctx, snapshotsService+"List", pbString(1, c.Snapshotter))
	if err != nil {
		return 0, 0, err
	}
	var keys []string
	for _, fr := range frames {
		f, err := pbFields(fr)
		if err != nil {
			return 0, 0, err
		}
		for _, info := range f[1] {
			inf, err := pbFields(info)
			if err != nil {
				return 0, 0, err
			}
			if len(inf[1]) > 0 {
				keys = append(keys, string(inf[1][0]))
			}
		}
	}
	for _, k := range keys {
		ans, err := c.call(ctx, snapshotsService+"Usage", append(pbString(1, c.Snapshotter), pbString(2, k)...), "")
		if IsNotFound(err) {
			continue
		}
		if err != nil {
			return 0, 0, err
		}
		size, err := pbVarint(ans, 1)
		if err != nil {
			return 0, 0, err
		}
		bytes += int64(size)
		count++
	}
	return bytes, count, nil
}

func (c *Containerd) stream(ctx context.Context, method string, msg []byte) ([][]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callWait)
	defer cancel()
	body := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(body[1:], uint32(len(msg)))
	copy(body[5:], msg)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://containerd"+method, bytes.NewReader(body))
	req.Header.Set("content-type", "application/grpc")
	req.Header.Set("te", "trailers")
	req.Header.Set("containerd-namespace", c.Namespace)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("nfsview: containerd %s: %w", method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
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
	var out [][]byte
	for len(data) > 0 {
		if len(data) < 5 || data[0] != 0 {
			return nil, fmt.Errorf("nfsview: containerd %s: an answer framed wrongly", method)
		}
		n := int(binary.BigEndian.Uint32(data[1:5]))
		if 5+n > len(data) {
			return nil, fmt.Errorf("nfsview: containerd %s: an answer cut short", method)
		}
		out = append(out, data[5:5+n])
		data = data[5+n:]
	}
	return out, nil
}

func pbVarint(b []byte, field int) (uint64, error) {
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return 0, errors.New("nfsview: a protobuf tag cut short")
		}
		b = b[n:]
		f, wire := int(tag>>3), tag&7
		switch wire {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return 0, errors.New("nfsview: a protobuf varint cut short")
			}
			if f == field {
				return v, nil
			}
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return 0, errors.New("nfsview: protobuf cut short")
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return 0, errors.New("nfsview: protobuf cut short")
			}
			b = b[4:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return 0, errors.New("nfsview: a protobuf field cut short")
			}
			b = b[n+int(l):]
		default:
			return 0, fmt.Errorf("nfsview: protobuf wire type %d", wire)
		}
	}
	return 0, nil
}
