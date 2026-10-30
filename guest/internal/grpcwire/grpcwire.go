package grpcwire

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

type Conn struct {
	Dial   func(ctx context.Context) (net.Conn, error)
	Header map[string]string
	Limit  int64

	client *http.Client
}

const CallWait = 30 * time.Second

func Unix(path string, header map[string]string) *Conn {
	return &Conn{Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}, Header: header}
}

func (c *Conn) http() *http.Client {
	if c.client == nil {
		var p http.Protocols
		p.SetUnencryptedHTTP2(true)
		c.client = &http.Client{Transport: &http.Transport{
			Protocols:       &p,
			DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return c.Dial(ctx) },
			IdleConnTimeout: 10 * time.Second,
		}}
	}
	return c.client
}

func (c *Conn) Close() {
	if c.client != nil {
		c.client.CloseIdleConnections()
	}
}

type Error struct {
	Method  string
	Code    int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: code %d: %s", e.Method, e.Code, e.Message)
}

const (
	NotFound      = 5
	AlreadyExists = 6
	Unimplemented = 12
	Unavailable   = 14
)

func Code(err error) int {
	var g *Error
	if errors.As(err, &g) {
		return g.Code
	}
	return -1
}

func (c *Conn) Call(ctx context.Context, method string, msg []byte) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, CallWait)
		defer cancel()
	}
	body := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(body[1:], uint32(len(msg)))
	copy(body[5:], msg)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://grpc"+method, bytes.NewReader(body))
	req.Header.Set("content-type", "application/grpc")
	req.Header.Set("te", "trailers")
	for k, v := range c.Header {
		req.Header.Set(k, v)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	limit := c.Limit
	if limit == 0 {
		limit = 16 << 20
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+5))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
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
		code, err := strconv.Atoi(status)
		if err != nil {
			code = -1
		}
		return nil, &Error{Method: method, Code: code, Message: m}
	}
	if len(data) < 5 {
		return nil, nil
	}
	n := int(binary.BigEndian.Uint32(data[1:5]))
	if data[0] != 0 || 5+n > len(data) {
		return nil, fmt.Errorf("%s: an answer framed wrongly (or over %d bytes)", method, limit)
	}
	return data[5 : 5+n], nil
}

func String(field int, s string) []byte {
	return Bytes(field, []byte(s))
}

func Bytes(field int, b []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(field<<3|2))
	out = binary.AppendUvarint(out, uint64(len(b)))
	return append(out, b...)
}

func Varint(field int, v uint64) []byte {
	out := binary.AppendUvarint(nil, uint64(field<<3))
	return binary.AppendUvarint(out, v)
}

func MapEntry(field int, k, v string) []byte {
	return Bytes(field, append(String(1, k), String(2, v)...))
}

type Fields struct {
	Len    map[int][][]byte
	Varint map[int][]uint64
}

func (f Fields) Str(n int) string {
	if len(f.Len[n]) > 0 {
		return string(f.Len[n][0])
	}
	return ""
}

func (f Fields) Int(n int) uint64 {
	if v := f.Varint[n]; len(v) > 0 {
		return v[len(v)-1]
	}
	return 0
}

func (f Fields) Map(n int) (map[string]string, error) {
	out := map[string]string{}
	for _, e := range f.Len[n] {
		ef, err := Decode(e)
		if err != nil {
			return nil, err
		}
		out[ef.Str(1)] = ef.Str(2)
	}
	return out, nil
}

func Decode(b []byte) (Fields, error) {
	out := Fields{Len: map[int][][]byte{}, Varint: map[int][]uint64{}}
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return out, errors.New("a protobuf tag cut short")
		}
		b = b[n:]
		field, wire := int(tag>>3), tag&7
		switch wire {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return out, errors.New("a protobuf varint cut short")
			}
			out.Varint[field] = append(out.Varint[field], v)
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return out, errors.New("protobuf cut short")
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return out, errors.New("protobuf cut short")
			}
			b = b[4:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return out, errors.New("a protobuf field cut short")
			}
			out.Len[field] = append(out.Len[field], b[n:n+int(l)])
			b = b[n+int(l):]
		default:
			return out, fmt.Errorf("protobuf wire type %d", wire)
		}
	}
	return out, nil
}

func Frame(msg []byte) []byte {
	out := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(out[1:], uint32(len(msg)))
	copy(out[5:], msg)
	return out
}
