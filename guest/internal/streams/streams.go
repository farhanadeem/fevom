package streams

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"fev/proto/relay"
	"fev/proto/viewhops"
)

type halfCloser interface {
	CloseWrite() error
}

func Splice(a, b net.Conn) { splice(a, b, nil, nil) }

type copied func(p []byte, read, written int64)

func splice(a, b net.Conn, ab, ba copied) {
	var wg sync.WaitGroup
	one := func(dst, src net.Conn, hook copied) {
		defer wg.Done()
		err := copyStream(dst, src, hook)
		if hc, ok := dst.(halfCloser); ok && err == nil {
			if hc.CloseWrite() == nil {
				return
			}
		}
		a.Close()
		b.Close()
	}
	wg.Add(2)
	go one(a, b, ba)
	go one(b, a, ab)
	wg.Wait()
	a.Close()
	b.Close()
}

const (
	smallCopyBuffer = 32 << 10
	largeCopyBuffer = 256 << 10
	largeBufferIdle = 20 * time.Millisecond
)

const maxLargeBuffers = 64

var largeLent atomic.Int64

func borrowLarge() *[]byte {
	if largeLent.Add(1) > maxLargeBuffers {
		largeLent.Add(-1)
		return nil
	}
	return largeCopyBuffers.Get().(*[]byte)
}

var largeCopyBuffers = sync.Pool{New: func() any {
	b := make([]byte, largeCopyBuffer)
	return &b
}}

func copyStream(dst, src net.Conn, hook copied) error {
	small := make([]byte, smallCopyBuffer)
	buf := small
	var large *[]byte
	release := func() {
		if large != nil {
			largeCopyBuffers.Put(large)
			largeLent.Add(-1)
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
			var t0 int64
			if hook != nil {
				t0 = viewhops.Now()
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return err
			}
			if hook != nil {
				hook(buf[:n], t0, viewhops.Now())
			}
		}
		var timeout interface{ Timeout() bool }
		switch {
		case err == io.EOF:
			return nil
		case err != nil && large != nil && errors.As(err, &timeout) && timeout.Timeout():
			release()
		case err != nil:
			return err
		case n == len(buf) && large == nil:
			if large = borrowLarge(); large != nil {
				buf = *large
			}
		case n < smallCopyBuffer:
			release()
		}
	}
}

type acceptWait struct{ delay time.Duration }

func (w *acceptWait) again(err error) bool {
	for _, errno := range []syscall.Errno{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM, syscall.ECONNABORTED, syscall.EINTR, syscall.EAGAIN} {
		if errors.Is(err, errno) {
			w.delay = min(max(2*w.delay, 5*time.Millisecond), time.Second)
			time.Sleep(w.delay)
			return true
		}
	}
	return false
}

func (w *acceptWait) reset() { w.delay = 0 }

func ServeSplice(l net.Listener, dial func() (net.Conn, error)) error {
	var wait acceptWait
	for {
		conn, err := l.Accept()
		if err != nil {
			if wait.again(err) {
				continue
			}
			return err
		}
		wait.reset()
		go func() {
			target, err := dial()
			if err != nil {
				conn.Close()
				return
			}
			Splice(conn, target)
		}()
	}
}

type Relay struct {
	Local    func(netip.Addr) bool
	Exec     func(token string, conn net.Conn)
	Stream   func(token string, conn net.Conn)
	Machine  func(machine string, target netip.AddrPort, timeout time.Duration) (net.Conn, error)
	View     func(timeout time.Duration) (net.Conn, error)
	ViewHops string
	Logf     func(format string, args ...any)
	Max      int

	active atomic.Int64
}

const descriptorReserve = 64

func StreamsFor(descriptors uint64) int {
	if descriptors <= descriptorReserve+2 {
		return 1
	}
	return int(min((descriptors-descriptorReserve)/2, 1<<20))
}

const (
	requestTimeout = 5 * time.Second
	dialTimeout    = 10 * time.Second
)

func (r *Relay) Serve(l net.Listener) error {
	var wait acceptWait
	for {
		conn, err := l.Accept()
		if err != nil {
			if wait.again(err) {
				continue
			}
			return err
		}
		wait.reset()
		if !r.admit() {
			r.refuse(conn)
			continue
		}
		go func() {
			defer r.active.Add(-1)
			r.serve(conn)
		}()
	}
}

func (r *Relay) admit() bool {
	if n := r.active.Add(1); r.Max > 0 && n > int64(r.Max) {
		r.active.Add(-1)
		return false
	}
	return true
}

func (r *Relay) refuse(conn net.Conn) {
	conn.SetWriteDeadline(time.Now().Add(time.Second))
	relay.WriteError(conn, relay.CodeFailed, "the guest's relay has all "+strconv.Itoa(r.Max)+" of its streams open")
	conn.Close()
}

func (r *Relay) ServeConn(conn net.Conn) {
	if !r.admit() {
		r.refuse(conn)
		return
	}
	defer r.active.Add(-1)
	r.serve(conn)
}

func (r *Relay) serve(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(requestTimeout))
	req, err := relay.ReadRequest(conn)
	if err != nil {
		relay.WriteError(conn, relay.CodeBadRequest, err.Error())
		return
	}
	conn.SetReadDeadline(time.Time{})

	switch req.Kind {
	case relay.Exec:
		if r.Exec == nil {
			relay.WriteError(conn, relay.CodeDenied, "no such session")
			return
		}
		r.Exec(req.Target, conn)
	case relay.Stream:
		if r.Stream == nil {
			relay.WriteError(conn, relay.CodeDenied, "no such stream request")
			return
		}
		r.Stream(req.Target, conn)
	case relay.MTCP:
		if r.Machine == nil {
			relay.WriteError(conn, relay.CodeDenied, "this guest runs no machines")
			return
		}
		r.mtcp(conn, req.Machine, netip.MustParseAddrPort(req.Target))
	case relay.View:
		if r.View == nil {
			relay.WriteError(conn, relay.CodeDenied, "this guest has no view")
			return
		}
		r.view(conn)
	case relay.TCP, relay.UDP:
		target := netip.MustParseAddrPort(req.Target)
		if !r.Local(target.Addr().Unmap()) {
			relay.WriteError(conn, relay.CodeDenied, target.Addr().String()+" is not an address of the guest")
			return
		}
		if req.Kind == relay.TCP {
			r.tcp(conn, target)
		} else {
			r.udp(conn, target)
		}
	}
}

func refusal(err error) string {
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return relay.CodeRefused
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETDOWN):
		return relay.CodeUnreachable
	case errors.As(err, &timeout) && timeout.Timeout():
		return relay.CodeTimeout
	}
	return relay.CodeFailed
}

func reuseAddr(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return serr
}

func (r *Relay) tcp(conn net.Conn, target netip.AddrPort) {
	d := net.Dialer{Timeout: dialTimeout, Control: reuseAddr}
	peer, err := d.Dial("tcp", target.String())
	if err != nil {
		relay.WriteError(conn, refusal(err), err.Error())
		return
	}
	if relay.WriteOK(conn) != nil {
		peer.Close()
		return
	}
	Splice(conn, peer)
}

func (r *Relay) mtcp(conn net.Conn, machine string, target netip.AddrPort) {
	peer, err := r.Machine(machine, target, dialTimeout)
	if err != nil {
		code := refusal(err)
		if errors.Is(err, ErrNoMachine) {
			code = relay.CodeDenied
		}
		relay.WriteError(conn, code, err.Error())
		return
	}
	if relay.WriteOK(conn) != nil {
		peer.Close()
		return
	}
	Splice(conn, peer)
}

func (r *Relay) view(conn net.Conn) {
	peer, err := r.View(dialTimeout)
	if err != nil {
		code := refusal(err)
		if errors.Is(err, ErrViewStopped) {
			code = relay.CodeRefused
		}
		relay.WriteError(conn, code, err.Error())
		return
	}
	if relay.WriteOK(conn) != nil {
		peer.Close()
		return
	}
	if r.ViewHops == "" {
		Splice(conn, peer)
		return
	}
	hopsOnce.Do(func() { hopsRec = viewhops.Open(r.ViewHops, viewhops.Guest) })
	var calls, replies viewhops.Marks
	splice(conn, peer, func(p []byte, t0, t1 int64) {
		calls.Feed(p, func(xid, size uint32, rest []byte) {
			hopsRec.Add(viewhops.Call, viewhops.MainOp(rest), xid, size, t0, t1)
		})
	}, func(p []byte, t0, t1 int64) {
		replies.Feed(p, func(xid, size uint32, _ []byte) { hopsRec.Add(viewhops.Reply, 0, xid, size, t0, t1) })
	})
}

var (
	hopsOnce sync.Once
	hopsRec  *viewhops.Recorder
)

var ErrViewStopped = errors.New("the view is not running")

var ErrNoMachine = errors.New("no such running machine")

func (r *Relay) udp(conn net.Conn, target netip.AddrPort) {
	peer, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(target))
	if err != nil {
		relay.WriteError(conn, refusal(err), err.Error())
		return
	}
	defer peer.Close()
	if relay.WriteOK(conn) != nil {
		return
	}
	go func() {
		buf := make([]byte, relay.MaxDatagram)
		for {
			n, err := peer.Read(buf)
			if err != nil {
				if errors.Is(err, syscall.ECONNREFUSED) {
					continue
				}
				conn.Close()
				return
			}
			if relay.WriteDatagram(conn, buf[:n]) != nil {
				return
			}
		}
	}()
	buf := make([]byte, relay.MaxDatagram)
	for {
		n, err := relay.ReadDatagram(conn, buf)
		if err != nil {
			return
		}
		if _, err := peer.Write(buf[:n]); err != nil && !errors.Is(err, syscall.ECONNREFUSED) {
			return
		}
	}
}

func LocalAddress(addr netip.Addr) bool {
	if addr.IsLoopback() {
		return true
	}
	if addr.IsUnspecified() || addr.IsMulticast() {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(ipnet.IP); ok && ip.Unmap() == addr {
				return true
			}
		}
	}
	return false
}
