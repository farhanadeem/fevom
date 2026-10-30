package nfsview

import (
	"io"
	"net"
	"testing"
	"time"
)

func pairFD(t *testing.T) (*fdConn, net.Conn) {
	t.Helper()
	dir := t.TempDir()
	ln, err := net.Listen("unix", dir+"/s")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a, err := net.Dial("unix", dir+"/s")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	f := newFDConn(a)
	if f == nil {
		t.Fatal("newFDConn refused a unix socket")
	}
	t.Cleanup(func() { f.release(); b.Close() })
	return f, b
}

func TestFDConnCarriesAndEnds(t *testing.T) {
	f, peer := pairFD(t)
	if _, err := peer.Write([]byte("call")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(f, got); err != nil || string(got) != "call" {
		t.Fatalf("read %q, %v", got, err)
	}
	if _, err := f.Write([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	got = make([]byte, 5)
	if _, err := io.ReadFull(peer, got); err != nil || string(got) != "reply" {
		t.Fatalf("the peer read %q, %v", got, err)
	}
	if err := f.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := peer.Read(got); n != 0 || err != io.EOF {
		t.Fatalf("after CloseWrite the peer read %d, %v; want the end", n, err)
	}
	peer.(*net.UnixConn).CloseWrite()
	if n, err := f.Read(got); n != 0 || err != io.EOF {
		t.Fatalf("after the peer's CloseWrite read %d, %v; want the end", n, err)
	}
}

func TestFDConnCloseWakesABlockedRead(t *testing.T) {
	f, _ := pairFD(t)
	done := make(chan error, 1)
	go func() {
		_, err := f.Read(make([]byte, 16))
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	f.Close()
	select {
	case err := <-done:
		if err != errClosed {
			t.Fatalf("the blocked read ended with %v, want errClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not wake the blocked read")
	}
	if _, err := f.Read(make([]byte, 1)); err != errClosed {
		t.Fatalf("a read after Close: %v", err)
	}
}
