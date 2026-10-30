package main

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fev/guest/debug/store"
	"fev/proto/debugstore"
	"fev/proto/jsonrpc"
)

func TestServeTellsAnInstallsProgress(t *testing.T) {
	st, err := store.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{st: st, authz: func(net.Conn) error { return nil }}
	s.install = func(ctx context.Context, names []string, progress func(debugstore.Progress)) (*debugstore.Installed, error) {
		if progress == nil {
			t.Error("the install was given no progress")
			return &debugstore.Installed{}, nil
		}
		progress(debugstore.Progress{Phase: debugstore.PhaseFetch, PathsDone: 1, PathsTotal: 1})
		return &debugstore.Installed{}, nil
	}
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go s.serve(ln, time.Minute)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	c := jsonrpc.NewConn(conn, jsonrpc.HandlerFunc(func(_ context.Context, _ *jsonrpc.Conn, method string, _ json.RawMessage) (any, error) {
		mu.Lock()
		got = append(got, method)
		mu.Unlock()
		return nil, nil
	}))
	defer c.Close()
	var r debugstore.Installed
	if err := c.Call(context.Background(), debugstore.MethodInstall, debugstore.InstallParams{Names: []string{"x"}}, &r); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		first := ""
		if n > 0 {
			first = got[0]
		}
		mu.Unlock()
		if n > 0 {
			if first != debugstore.NotifyProgress {
				t.Fatalf("notification %q", first)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no store.progress notification")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServeLeavesWhenIdle(t *testing.T) {
	st, _ := store.Open(t.TempDir(), "")
	s := &server{st: st}
	sock := filepath.Join(t.TempDir(), "s")
	ln, _ := net.Listen("unix", sock)
	done := make(chan error, 1)
	go func() { done <- s.serve(ln, 200*time.Millisecond) }()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("left with a connection open")
	case <-time.After(600 * time.Millisecond):
	}
	conn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not leave after the last connection")
	}
}
