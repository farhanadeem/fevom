package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fev/guest/internal/grpcwire"
)

func TestTheExitsAnOutageLostAreForwarded(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "exits")
	defer os.RemoveAll(dir)
	var (
		mu        sync.Mutex
		forwarded []grpcwire.Fields
	)
	exitedAt := append(grpcwire.Varint(1, 1700000000), grpcwire.Varint(2, 5)...)
	proc := func(id string, status, exit uint64) []byte {
		p := append(grpcwire.String(1, id), grpcwire.String(2, id)...)
		p = append(p, grpcwire.Varint(3, 4242)...)
		p = append(p, grpcwire.Varint(4, status)...)
		p = append(p, grpcwire.Varint(9, exit)...)
		if status == taskStopped {
			p = append(p, grpcwire.Bytes(10, exitedAt)...)
		}
		return grpcwire.Bytes(1, p)
	}
	sock := filepath.Join(dir, "s")
	_, stop, err := grpcwire.Fake(sock, func(method string, h http.Header, msg grpcwire.Fields) ([]byte, int, string) {
		if h.Get("containerd-namespace") != "moby" {
			return nil, 3, "namespace"
		}
		switch method {
		case tasksList:
			return append(proc("brief", taskStopped, 7), proc("long", 2, 0)...), 0, ""
		case eventsForward:
			env, _ := grpcwire.Decode(msg.Len[1][0])
			mu.Lock()
			forwarded = append(forwarded, env)
			mu.Unlock()
			return nil, 0, ""
		}
		return nil, grpcwire.Unimplemented, method
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dsock := filepath.Join(dir, "d")
	dl, err := net.Listen("unix", dsock)
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(dl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"Id":"brief"},{"Id":"long"},{"Id":"gone"}]`))
	}))
	defer dl.Close()
	n, all, err := forwardLostExits(ctx, sock, dsock)
	if err != nil || n != 2 || all != 3 || len(forwarded) != 2 {
		t.Fatalf("forwarded %d (%v): %v", n, forwarded, err)
	}
	env := forwarded[0]
	if env.Str(2) != "moby" || env.Str(3) != "/tasks/exit" {
		t.Errorf("namespace %q topic %q", env.Str(2), env.Str(3))
	}
	any, _ := grpcwire.Decode(env.Len[4][0])
	if any.Str(1) != "containerd.events.TaskExit" {
		t.Errorf("type %q", any.Str(1))
	}
	ev, _ := grpcwire.Decode(any.Len[2][0])
	if ev.Str(1) != "brief" || ev.Str(2) != "brief" || ev.Int(3) != 4242 || ev.Int(4) != 7 || string(ev.Len[5][0]) != string(exitedAt) {
		t.Errorf("the exit %v %v", ev.Len, ev.Varint)
	}
	any, _ = grpcwire.Decode(forwarded[1].Len[4][0])
	ev, _ = grpcwire.Decode(any.Len[2][0])
	if ev.Str(1) != "gone" || ev.Int(4) != 255 || len(ev.Len[5]) != 1 {
		t.Errorf("the lost task's exit %v %v", ev.Len, ev.Varint)
	}
}
