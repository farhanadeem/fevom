package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"fev/guest/internal/grpcwire"
)

const (
	tasksList      = "/containerd.services.tasks.v1.Tasks/List"
	eventsForward  = "/containerd.services.events.v1.Events/Forward"
	taskExitTopic  = "/tasks/exit"
	taskExitType   = "containerd.events.TaskExit"
	taskStopped    = 3
	deadShimStatus = 255
)

type lostExit struct {
	container, id string
	pid, status   uint32
	exitedAt      []byte
	state         uint64
}

func listTasks(ctx context.Context, c *grpcwire.Conn) (map[string]lostExit, error) {
	ans, err := c.Call(ctx, tasksList, nil)
	if err != nil {
		return nil, err
	}
	f, err := grpcwire.Decode(ans)
	if err != nil {
		return nil, err
	}
	out := map[string]lostExit{}
	for _, b := range f.Len[1] {
		p, err := grpcwire.Decode(b)
		if err != nil {
			return nil, err
		}
		e := lostExit{container: p.Str(1), id: p.Str(2), pid: uint32(p.Int(3)), status: uint32(p.Int(9)), state: p.Int(4)}
		if len(p.Len[10]) > 0 {
			e.exitedAt = p.Len[10][0]
		}
		if e.id == "" {
			e.id = e.container
		}
		out[e.container] = e
	}
	return out, nil
}

func exitEnvelope(ns string, e lostExit, now time.Time) []byte {
	ts := append(grpcwire.Varint(1, uint64(now.Unix())), grpcwire.Varint(2, uint64(now.Nanosecond()))...)
	ev := append(grpcwire.String(1, e.container), grpcwire.String(2, e.id)...)
	ev = append(ev, grpcwire.Varint(3, uint64(e.pid))...)
	ev = append(ev, grpcwire.Varint(4, uint64(e.status))...)
	if e.exitedAt != nil {
		ev = append(ev, grpcwire.Bytes(5, e.exitedAt)...)
	} else {
		ev = append(ev, grpcwire.Bytes(5, ts)...)
	}
	any := append(grpcwire.String(1, taskExitType), grpcwire.Bytes(2, ev)...)
	env := append(grpcwire.Bytes(1, ts), grpcwire.String(2, ns)...)
	env = append(env, grpcwire.String(3, taskExitTopic)...)
	env = append(env, grpcwire.Bytes(4, any)...)
	return grpcwire.Bytes(1, env)
}

func runningContainers(ctx context.Context, dockerSock string) ([]string, error) {
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", dockerSock)
	}, DisableKeepAlives: true}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://engine/containers/json", nil)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list []struct{ Id string }
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&list); err != nil {
		return nil, err
	}
	var out []string
	for _, c := range list {
		out = append(out, c.Id)
	}
	return out, nil
}

func forwardLostExits(ctx context.Context, containerdSock, dockerSock string) (int, int, error) {
	running, err := runningContainers(ctx, dockerSock)
	if err != nil {
		return 0, 0, fmt.Errorf("ask dockerd what runs: %w", err)
	}
	c := grpcwire.Unix(containerdSock, map[string]string{"containerd-namespace": "moby"})
	defer c.Close()
	tasks, err := listTasks(ctx, c)
	if err != nil {
		return 0, len(running), fmt.Errorf("list Docker's tasks: %w", err)
	}
	n := 0
	for _, id := range running {
		t, ok := tasks[id]
		switch {
		case ok && t.state != taskStopped:
			continue
		case !ok:
			t = lostExit{container: id, id: id, status: deadShimStatus}
		}
		if _, err := c.Call(ctx, eventsForward, exitEnvelope("moby", t, time.Now())); err != nil {
			return n, len(running), fmt.Errorf("forward the exit of %s: %w", id, err)
		}
		n++
	}
	return n, len(running), nil
}
