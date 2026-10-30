package vsock

import (
	"context"
	"errors"
	"net"
	"syscall"
	"time"

	"fev/proto/relay"
)

const refusedFor = 2 * time.Second

const SendBuffer = 256 << 10

func Dial(ctx context.Context, path string) (*net.UnixConn, error) {
	var d net.Dialer
	delay := time.Millisecond
	var waited time.Duration
	for {
		conn, err := d.DialContext(ctx, "unix", path)
		if err == nil {
			c := conn.(*net.UnixConn)
			c.SetWriteBuffer(SendBuffer)
			return c, nil
		}
		if !errors.Is(err, syscall.ECONNREFUSED) || waited >= refusedFor {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(delay):
		}
		waited += delay
		delay = min(2*delay, 50*time.Millisecond)
	}
}

const handshakeTimeout = 30 * time.Second

type Relay struct{ Path string }

func (r Relay) Open(ctx context.Context, req relay.Request) (*net.UnixConn, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	conn, err := Dial(ctx, r.Path)
	if err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(handshakeTimeout)
	}
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	err = relay.Open(conn, req)
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}
