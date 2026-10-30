package cri

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"fev/guest/internal/grpcwire"
)

const runtimeService = "/runtime.v1.RuntimeService/"

const (
	ContainerCreated = 0
	ContainerRunning = 1
	ContainerExited  = 2
)

const GraceAnnotation = "io.kubernetes.pod.terminationGracePeriod"

const DefaultGrace = 30 * time.Second

type Container struct {
	ID, Sandbox, Name string
	State             int
	Annotations       map[string]string
	Labels            map[string]string
}

func (c Container) Grace() time.Duration {
	if v, ok := c.Annotations[GraceAnnotation]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return DefaultGrace
}

type Client struct{ c *grpcwire.Conn }

func New(path string) *Client { return &Client{c: grpcwire.Unix(path, nil)} }

func Over(c *grpcwire.Conn) *Client { return &Client{c: c} }

func (c *Client) Close() { c.c.Close() }

func (c *Client) Containers(ctx context.Context, state int) ([]Container, error) {
	var req []byte
	if state >= 0 {
		req = grpcwire.Bytes(1, grpcwire.Bytes(2, grpcwire.Varint(1, uint64(state))))
	}
	ans, err := c.c.Call(ctx, runtimeService+"ListContainers", req)
	if err != nil {
		return nil, err
	}
	f, err := grpcwire.Decode(ans)
	if err != nil {
		return nil, err
	}
	var out []Container
	for _, b := range f.Len[1] {
		cf, err := grpcwire.Decode(b)
		if err != nil {
			return nil, err
		}
		ct := Container{ID: cf.Str(1), Sandbox: cf.Str(2), State: int(cf.Int(6))}
		if len(cf.Len[3]) > 0 {
			if mf, err := grpcwire.Decode(cf.Len[3][0]); err == nil {
				ct.Name = mf.Str(1)
			}
		}
		if ct.Labels, err = cf.Map(8); err != nil {
			return nil, err
		}
		if ct.Annotations, err = cf.Map(9); err != nil {
			return nil, err
		}
		out = append(out, ct)
	}
	return out, nil
}

func (c *Client) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	req := append(grpcwire.String(1, id), grpcwire.Varint(2, uint64(timeout/time.Second))...)
	_, err := c.c.Call(ctx, runtimeService+"StopContainer", req)
	return err
}

func (c *Client) Sandboxes(ctx context.Context) ([]string, error) {
	ans, err := c.c.Call(ctx, runtimeService+"ListPodSandbox", nil)
	if err != nil {
		return nil, err
	}
	f, err := grpcwire.Decode(ans)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, b := range f.Len[1] {
		sf, err := grpcwire.Decode(b)
		if err != nil {
			return nil, err
		}
		out = append(out, sf.Str(1))
	}
	return out, nil
}

func (c *Client) StopSandbox(ctx context.Context, id string) error {
	_, err := c.c.Call(ctx, runtimeService+"StopPodSandbox", grpcwire.String(1, id))
	return err
}

func (c *Client) RemoveSandbox(ctx context.Context, id string) error {
	_, err := c.c.Call(ctx, runtimeService+"RemovePodSandbox", grpcwire.String(1, id))
	return err
}

type Drained struct {
	Containers, Sandboxes int
	Longest               time.Duration
	Errors                []string
}

func (c *Client) Drain(ctx context.Context, limit time.Duration) Drained {
	var d Drained
	cs, err := c.Containers(ctx, ContainerRunning)
	if err != nil {
		d.Errors = append(d.Errors, fmt.Sprintf("list containers: %v", err))
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for _, ct := range cs {
		grace := min(ct.Grace(), limit)
		d.Containers++
		wg.Add(1)
		go func() {
			defer wg.Done()
			began := time.Now()
			cctx, cancel := context.WithTimeout(ctx, grace+10*time.Second)
			err := c.StopContainer(cctx, ct.ID, grace)
			cancel()
			took := time.Since(began)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				d.Errors = append(d.Errors, fmt.Sprintf("stop %s (%s): %v", ct.Name, short(ct.ID), err))
			}
			d.Longest = max(d.Longest, took)
		}()
	}
	wg.Wait()
	ids, err := c.Sandboxes(ctx)
	if err != nil {
		d.Errors = append(d.Errors, fmt.Sprintf("list sandboxes: %v", err))
	}
	for _, id := range ids {
		if err := c.StopSandbox(ctx, id); err != nil {
			d.Errors = append(d.Errors, fmt.Sprintf("stop sandbox %s: %v", short(id), err))
			continue
		}
		if err := c.RemoveSandbox(ctx, id); err != nil {
			d.Errors = append(d.Errors, fmt.Sprintf("remove sandbox %s: %v", short(id), err))
			continue
		}
		d.Sandboxes++
	}
	return d
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
