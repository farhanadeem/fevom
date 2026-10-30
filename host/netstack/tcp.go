package netstack

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const tcpDialTimeout = 30 * time.Second

const slowDial = 500 * time.Millisecond

func (s *Stack) forwardTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	asked := netip.AddrPortFrom(fromAddr(id.LocalAddress), id.LocalPort)
	dst, ok := s.route(asked)
	if !ok {
		r.Complete(true)
		return
	}
	s.flows.Add(1)
	if p := s.transparentProxy(asked, dst); p != nil {
		go func() {
			defer s.flows.Done()
			p.transparent(r, dst)
		}()
		return
	}
	go func() {
		defer s.flows.Done()
		s.forwardDirect(r, dst)
	}()
}

func (s *Stack) forwardDirect(r *tcp.ForwarderRequest, dst netip.AddrPort) {
	id := r.ID()
	syn, haveSyn := s.syns.take(id)
	ctx, cancel := context.WithTimeout(s.ctx, tcpDialTimeout)
	started := time.Now()
	host, err := s.host.DialTCP(ctx, dst)
	cancel()
	if took := time.Since(started); took > slowDial {
		s.log.Warn("tcp: the Mac took a long time to connect", "dst", dst, "took", took.Round(time.Millisecond), "err", err)
	}
	if err != nil {
		s.log.Debug("tcp: dial failed", "dst", dst, "err", err)
		if haveSyn && s.cfg.Gateway6.IsValid() && noRoute(err) {
			r.Complete(false)
			s.injectICMP6(syn.mac, s.cfg.Gateway6, fromAddr(id.RemoteAddress), header.ICMPv6DstUnreachable, header.ICMPv6NetworkUnreachable, syn.quote[:])
			return
		}
		r.Complete(true)
		return
	}
	defer host.Close()

	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		r.Complete(true)
		return
	}
	r.Complete(false)
	guest := gonet.NewTCPConn(&wq, ep)
	defer guest.Close()
	stop := context.AfterFunc(s.ctx, func() {
		guest.Close()
		host.Close()
	})
	defer stop()
	splice(guest, host)
}

func noRoute(err error) bool {
	return errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.EADDRNOTAVAIL)
}

type halfCloser interface {
	CloseWrite() error
}

const (
	smallCopyBuffer = 32 << 10
	largeCopyBuffer = 512 << 10
	largeBufferIdle = 20 * time.Millisecond
)

var largeCopyBuffers = sync.Pool{New: func() any {
	b := make([]byte, largeCopyBuffer)
	return &b
}}

func copyStream(dst, src net.Conn) error {
	small := make([]byte, smallCopyBuffer)
	buf := small
	var large *[]byte
	release := func() {
		if large != nil {
			largeCopyBuffers.Put(large)
			large, buf = nil, small
			src.SetReadDeadline(time.Time{})
		}
	}
	defer release()
	for {
		if large != nil {
			src.SetReadDeadline(time.Now().Add(largeBufferIdle))
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return err
			}
		}
		switch {
		case err == io.EOF:
			return nil
		case err != nil && large != nil && isTimeout(err):
			release()
		case err != nil:
			return err
		case n == len(buf) && large == nil:
			large = largeCopyBuffers.Get().(*[]byte)
			buf = *large
		case n < smallCopyBuffer:
			release()
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func Splice(a, b net.Conn) { splice(a, b) }

func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	copyHalf := func(dst, src net.Conn) {
		defer wg.Done()
		err := copyStream(dst, src)
		if hc, ok := dst.(halfCloser); ok && err == nil {
			hc.CloseWrite()
			return
		}
		dst.Close()
		src.Close()
	}
	wg.Add(2)
	go copyHalf(a, b)
	go copyHalf(b, a)
	wg.Wait()
}
