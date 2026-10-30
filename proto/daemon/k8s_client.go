package daemon

import (
	"context"
	"encoding/json"
	"errors"

	"fev/proto/jsonrpc"
)

func (c *Client) handleK8s(method string, params json.RawMessage) error {
	switch method {
	case NotifyK8sChanged:
		n, err := jsonrpc.Bind[K8sChanged](params)
		if err != nil {
			return err
		}
		c.mu.Lock()
		deliver := c.k8sWatches[n.Subscription]
		c.mu.Unlock()
		if deliver != nil {
			deliver(n)
		}
	case NotifyK8sLog:
		n, err := jsonrpc.Bind[K8sLog](params)
		if err != nil {
			return err
		}
		c.mu.Lock()
		sub := c.k8sLogs[n.Subscription]
		c.mu.Unlock()
		if sub != nil && sub.text != nil {
			sub.text(n.Text)
		}
	case NotifyK8sLogEnd:
		n, err := jsonrpc.Bind[K8sLogEnd](params)
		if err != nil {
			return err
		}
		c.mu.Lock()
		sub := c.k8sLogs[n.Subscription]
		delete(c.k8sLogs, n.Subscription)
		c.mu.Unlock()
		if sub != nil && sub.end != nil {
			var err error
			if n.Error != "" {
				err = errors.New(n.Error)
			}
			sub.end(err)
		}
	}
	return nil
}

func (c *Client) K8sStatus(ctx context.Context) (*K8sStatus, error) {
	var r K8sStatus
	return &r, c.conn.Call(ctx, MethodK8sStatus, Empty{}, &r)
}

func (c *Client) K8sEnable(ctx context.Context) (*Operation, error) {
	var r Operation
	return &r, c.conn.Call(ctx, MethodK8sEnable, Empty{}, &r)
}

func (c *Client) K8sDisable(ctx context.Context) (*Operation, error) {
	var r Operation
	return &r, c.conn.Call(ctx, MethodK8sDisable, Empty{}, &r)
}

func (c *Client) K8sReset(ctx context.Context, p K8sResetParams) (*Operation, error) {
	var r Operation
	return &r, c.conn.Call(ctx, MethodK8sReset, p, &r)
}

func (c *Client) K8sWatch(ctx context.Context, p K8sWatchParams, deliver func(K8sChanged)) (*K8sWatching, func(), error) {
	var r K8sWatching
	err := c.conn.CallThen(ctx, MethodK8sWatch, p, &r, func() {
		c.mu.Lock()
		c.k8sWatches[r.Subscription] = deliver
		c.mu.Unlock()
	})
	if err != nil {
		return nil, nil, err
	}
	return &r, func() {
		c.mu.Lock()
		delete(c.k8sWatches, r.Subscription)
		c.mu.Unlock()
		c.conn.Call(context.Background(), MethodK8sUnwatch, Subscription{Subscription: r.Subscription}, nil)
	}, nil
}

func (c *Client) K8sGet(ctx context.Context, ref K8sRef) (*K8sManifest, error) {
	var r K8sManifest
	return &r, c.conn.Call(ctx, MethodK8sGet, ref, &r)
}

func (c *Client) K8sApply(ctx context.Context, p K8sApplyParams) (*K8sApplyResult, error) {
	var r K8sApplyResult
	return &r, c.conn.Call(ctx, MethodK8sApply, p, &r)
}

func (c *Client) K8sUpdate(ctx context.Context, p K8sUpdateParams) (*K8sApplyResult, error) {
	var r K8sApplyResult
	return &r, c.conn.Call(ctx, MethodK8sUpdate, p, &r)
}

func (c *Client) K8sScale(ctx context.Context, p K8sScaleParams) (*K8sObject, error) {
	var r K8sObject
	return &r, c.conn.Call(ctx, MethodK8sScale, p, &r)
}

func (c *Client) K8sRestart(ctx context.Context, ref K8sRef) (*K8sObject, error) {
	var r K8sObject
	return &r, c.conn.Call(ctx, MethodK8sRestart, ref, &r)
}

func (c *Client) K8sDelete(ctx context.Context, ref K8sRef) error {
	return c.conn.Call(ctx, MethodK8sDelete, ref, nil)
}

func (c *Client) K8sLogs(ctx context.Context, p K8sLogsParams, text func(string), end func(error)) (func(), error) {
	var sub Subscription
	err := c.conn.CallThen(ctx, MethodK8sLogs, p, &sub, func() {
		c.mu.Lock()
		c.k8sLogs[sub.Subscription] = &k8sLogSub{text: text, end: end}
		c.mu.Unlock()
	})
	if err != nil {
		return nil, err
	}
	return func() {
		c.mu.Lock()
		_, open := c.k8sLogs[sub.Subscription]
		delete(c.k8sLogs, sub.Subscription)
		c.mu.Unlock()
		if open {
			c.conn.Call(context.Background(), MethodK8sUnwatch, sub, nil)
		}
	}, nil
}

func (c *Client) K8sExec(ctx context.Context, p K8sExecParams, exited func(SessionExit)) (*K8sExecStarted, error) {
	var r K8sExecStarted
	err := c.conn.CallThen(ctx, MethodK8sExec, p, &r, func() {
		if exited != nil {
			c.mu.Lock()
			c.sessions[r.ID] = exited
			c.mu.Unlock()
		}
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}
