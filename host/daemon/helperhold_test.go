package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	priv "fev/proto/privhelper"
)

func TestAPassedDescriptorIsHeldUntilItsReceiverHasIt(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	h, err := NewStandInHelper("")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "ph.sock")
	if err := h.Listen(sock); err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
				if err == nil {
					syscall.Close(fds[0])
					syscall.Close(fds[1])
				}
			}
		}()
	}
	defer func() { close(stop); wg.Wait() }()

	const rounds = 150
	flushed, lost := 0, 0
	for i := range rounds {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var tun priv.Tunnel
		f, err := priv.Call(ctx, sock, priv.MethodTunnelOpen, priv.TunnelParams{MTU: 1500}, &tun)
		cancel()
		if err != nil {
			t.Fatalf("round %d: tunnel.open: %v", i, err)
		}
		if !tunnelWhole(t, f, h.lastPeer()) {
			flushed++
		}
		f.Close()
		h.mu.Lock()
		h.peers[len(h.peers)-1].Close()
		h.peers = h.peers[:len(h.peers)-1]
		h.mu.Unlock()

		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		var bound priv.Bound
		b, err := priv.Call(ctx, sock, priv.MethodPortBind, priv.PortParams{Proto: "tcp", Family: 4, Port: 80}, &bound)
		cancel()
		if err != nil {
			t.Fatalf("round %d: port.bind: %v", i, err)
		}
		if !listenerWhole(b) {
			lost++
		}
		b.Close()
	}
	if flushed != 0 || lost != 0 {
		t.Errorf("of %d rounds: %d tunnel ends flushed (read end of file at once), %d passed listeners that did not accept", rounds, flushed, lost)
	}
}

func tunnelWhole(t *testing.T, f, peer *os.File) bool {
	fd := int(f.Fd())
	syscall.SetNonblock(fd, true)
	buf := make([]byte, 64)
	if n, err := syscall.Read(fd, buf); !errors.Is(err, syscall.EAGAIN) {
		t.Logf("the tunnel's end read %d bytes, %v, with nothing sent", n, err)
		return false
	}
	if _, err := syscall.Write(int(peer.Fd()), []byte{0, 0, 0, 2}); err != nil {
		t.Logf("the stand-in's end: %v", err)
		return false
	}
	syscall.SetNonblock(fd, false)
	tv := syscall.Timeval{Sec: 1}
	syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	if n, err := syscall.Read(fd, buf); n != 4 || err != nil {
		t.Logf("the tunnel's end read %d bytes, %v, of a datagram of 4", n, err)
		return false
	}
	return true
}

func listenerWhole(f *os.File) bool {
	l, err := net.FileListener(f)
	if err != nil {
		return false
	}
	defer l.Close()
	c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	l.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second))
	a, err := l.Accept()
	if err != nil {
		return false
	}
	a.Close()
	return true
}
