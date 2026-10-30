package privhelper_test

import (
	"errors"
	"sync"
	"syscall"
	"testing"

	api "fev/proto/privhelper"
)

func TestTheTunnelPassedIsHeldUntilFevdHasIt(t *testing.T) {
	r := newRig(t, nil)
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
				if fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0); err == nil {
					syscall.Close(fds[0])
					syscall.Close(fds[1])
				}
			}
		}()
	}
	defer func() { close(stop); wg.Wait() }()
	const rounds = 100
	flushed := 0
	for i := range rounds {
		var tun api.Tunnel
		f, err := r.call(api.MethodTunnelOpen, api.TunnelParams{MTU: 1500}, &tun)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		fd := int(f.Fd())
		syscall.SetNonblock(fd, true)
		b := make([]byte, 64)
		n, err := syscall.Read(fd, b)
		switch {
		case !errors.Is(err, syscall.EAGAIN):
			t.Logf("round %d: fevd's end read %d bytes, %v, with nothing sent", i, n, err)
			flushed++
		default:
			if _, err := syscall.Write(r.sys.PeerOf(tun.Interface), []byte("pkt!")); err != nil {
				t.Fatalf("round %d: the far end: %v", i, err)
			}
			syscall.SetNonblock(fd, false)
			if n, err := syscall.Read(fd, b); n != 4 || err != nil {
				t.Logf("round %d: fevd's end read %d bytes, %v, of a packet of 4", i, n, err)
				flushed++
			}
		}
		f.Close()
		r.sys.Detach(tun.Interface)
	}
	if flushed != 0 {
		t.Errorf("%d of %d tunnels passed were flushed in flight", flushed, rounds)
	}
}
