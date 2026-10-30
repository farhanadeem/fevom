package netstack

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

type sourceHost struct {
	*fakeHost
	size int64
}

func (h sourceHost) DialTCP(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer l.Close()
	client, err := net.Dial("tcp4", l.Addr().String())
	if err != nil {
		return nil, err
	}
	server, err := l.Accept()
	if err != nil {
		return nil, err
	}
	go func() {
		defer server.Close()
		go io.Copy(io.Discard, server)
		buf := make([]byte, 256<<10)
		for sent := int64(0); sent < h.size; sent += int64(len(buf)) {
			if _, err := server.Write(buf); err != nil {
				return
			}
		}
	}()
	return client, nil
}

func BenchmarkTCPDownload(b *testing.B) {
	b.Run("no offload", func(b *testing.B) { benchDownload(b, 0) })
	b.Run("segmentation offload", func(b *testing.B) { benchDownload(b, acceptsPartialCsum|acceptsTSO4) })
}

func benchDownload(b *testing.B, accepts uint32) {
	const size = 256 << 20
	hostEnd, guestEnd := socketPair(b)
	announce(b, guestEnd, accepts)
	ns, err := New(hostEnd, Options{Host: sourceHost{newFakeHost(), size}})
	if err != nil {
		b.Fatal(err)
	}
	defer ns.Close()
	w := &world{host: newFakeHost(), ns: ns}
	w.guest = newGuestStack(b, guestEnd, accepts != 0)
	b.SetBytes(size)
	b.ResetTimer()
	for range b.N {
		c, err := w.dialTCP(b, "93.184.215.14:80")
		if err != nil {
			b.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(60 * time.Second))
		n, err := io.Copy(io.Discard, c)
		if err != nil || n < size {
			b.Fatalf("read %d bytes: %v", n, err)
		}
		c.Close()
	}
}
