package vmmctl

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"fev/proto/jsonrpc"
)

type Client struct {
	conn *jsonrpc.Conn

	mu    sync.Mutex
	reset func(Reset)
	space func(Space)
}

func NewClient(rw io.ReadWriteCloser) *Client {
	c := &Client{}
	c.conn = jsonrpc.NewConn(rw, jsonrpc.HandlerFunc(c.handle))
	return c
}

func (c *Client) handle(_ context.Context, _ *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
	if method == NotifySpace {
		sp, err := jsonrpc.Bind[Space](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		deliver := c.space
		c.mu.Unlock()
		if deliver != nil {
			deliver(sp)
		}
		return nil, nil
	}
	if method != NotifyReset {
		return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method %q not found", method)
	}
	r, err := jsonrpc.Bind[Reset](params)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	deliver := c.reset
	c.mu.Unlock()
	if deliver != nil {
		deliver(r)
	}
	return nil, nil
}

func (c *Client) OnReset(deliver func(Reset)) {
	c.mu.Lock()
	c.reset = deliver
	c.mu.Unlock()
}

func (c *Client) OnSpace(deliver func(Space)) {
	c.mu.Lock()
	c.space = deliver
	c.mu.Unlock()
}

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) Done() <-chan struct{} { return c.conn.Done() }

func (c *Client) Pause(ctx context.Context) (*PauseResult, error) {
	var r PauseResult
	return &r, c.conn.Call(ctx, MethodPause, nil, &r)
}

func (c *Client) Resume(ctx context.Context) (*ResumeResult, error) {
	var r ResumeResult
	return &r, c.conn.Call(ctx, MethodResume, nil, &r)
}

func (c *Client) Stats(ctx context.Context) (*Stats, error) {
	var r Stats
	return &r, c.conn.Call(ctx, MethodStats, nil, &r)
}

func (c *Client) Budget(ctx context.Context, b Baseline) (*BudgetResult, error) {
	var r BudgetResult
	return &r, c.conn.Call(ctx, MethodBudget, b, &r)
}

func (c *Client) Stop(ctx context.Context) error {
	return c.conn.Call(ctx, MethodStop, nil, nil)
}

func (c *Client) Kill(ctx context.Context) error {
	err := c.conn.Call(ctx, MethodKill, nil, nil)
	if errors.Is(err, jsonrpc.ErrClosed) {
		return nil
	}
	return err
}

func IsStopping(err error) bool {
	var e *jsonrpc.Error
	return errors.As(err, &e) && e.Code == CodeStopping
}
