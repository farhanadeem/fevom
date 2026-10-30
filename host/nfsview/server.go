package nfsview

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"fev/proto/relay"
	"fev/proto/viewhops"
)

var hops = viewhops.Open(os.Getenv("FEV_VIEW_HOPS"), viewhops.Fevd)

type Opener interface {
	Open(ctx context.Context, req relay.Request) (*net.UnixConn, error)
}

var Request = relay.Request{Kind: relay.View, Target: relay.ViewNFS}

const openTimeout = 30 * time.Second

const BlockingConns = 64

type Server struct {
	ln   net.Listener
	path string
	port int
	open Opener
	log  *slog.Logger

	mu      sync.Mutex
	conns   map[net.Conn]bool
	watches map[*watch]bool
	closed  bool
	wg      sync.WaitGroup

	stall      func() bool
	checking   bool
	stallAfter time.Duration

	paused   atomic.Bool
	answered atomic.Int64

	blocking atomic.Int32

	qmu      sync.Mutex
	inflight int64
	drained  chan struct{}
}

func (s *Server) addInflight(d int64) {
	s.qmu.Lock()
	defer s.qmu.Unlock()
	s.inflight += d
	if s.inflight <= 0 {
		s.inflight = 0
		if s.drained != nil {
			close(s.drained)
			s.drained = nil
		}
	}
}

func (s *Server) WaitQuiet(deadline time.Time) int {
	s.qmu.Lock()
	if s.inflight == 0 {
		s.qmu.Unlock()
		return 0
	}
	if s.drained == nil {
		s.drained = make(chan struct{})
	}
	ch := s.drained
	s.qmu.Unlock()
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	}
	s.qmu.Lock()
	defer s.qmu.Unlock()
	return int(s.inflight)
}

func (s *Server) SetPaused(paused bool) { s.paused.Store(paused) }

func (s *Server) Out() int {
	s.mu.Lock()
	ws := make([]*watch, 0, len(s.watches))
	for w := range s.watches {
		ws = append(ws, w)
	}
	s.mu.Unlock()
	n := 0
	for _, w := range ws {
		w.mu.Lock()
		n += w.out
		w.mu.Unlock()
	}
	return n
}

func (s *Server) Answered() int64 { return s.answered.Load() }

var StallAfter = time.Second

func (s *Server) OnStall(stall func() bool) {
	s.mu.Lock()
	s.stall = stall
	s.mu.Unlock()
}

func (s *Server) Rearm() {
	s.mu.Lock()
	ws := make([]*watch, 0, len(s.watches))
	for w := range s.watches {
		ws = append(ws, w)
	}
	s.mu.Unlock()
	for _, w := range ws {
		w.rearm()
	}
}

func Listen(path string, open Opener, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	dir := filepath.Dir(path)
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != uint32(os.Getuid()) || fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("view socket: %s must be the user's and closed to others (it is %v): it is all that guards the view's mount", dir, fi.Mode().Perm())
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != os.ModeSocket {
			return nil, fmt.Errorf("view socket: %s is there and is not a socket", path)
		}
		os.Remove(path)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	ln.SetUnlinkOnClose(true)
	s := &Server{ln: ln, path: path, open: open, log: log, conns: map[net.Conn]bool{}, watches: map[*watch]bool{}, stallAfter: StallAfter}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

func ListenLoopback(open Opener, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, port: ln.Addr().(*net.TCPAddr).Port, open: open, log: log, conns: map[net.Conn]bool{}, watches: map[*watch]bool{}, stallAfter: StallAfter}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

func (s *Server) Path() string { return s.path }

func (s *Server) Endpoint() Endpoint { return Endpoint{Port: s.port, Socket: s.path} }

func (s *Server) peerAllowed(c net.Conn) bool {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return true
	}
	uid, err := peerUID(tc.LocalAddr().(*net.TCPAddr), tc.RemoteAddr().(*net.TCPAddr))
	if err != nil {
		s.log.Warn("view: a connection refused, its other end not found", "peer", tc.RemoteAddr(), "err", err)
		return false
	}
	if !uidServed(uid) {
		s.log.Warn("view: a connection refused, another user's", "peer", tc.RemoteAddr(), "uid", uid)
		return false
	}
	tc.SetNoDelay(true)
	return true
}

func uidServed(uid uint32) bool { return uid == 0 || uid == uint32(os.Getuid()) }

type halfConn interface {
	net.Conn
	CloseWrite() error
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		ac, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Warn("view socket: accept", "err", err)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if !s.peerAllowed(ac) {
			ac.Close()
			continue
		}
		c := ac.(halfConn)
		blocking := s.takeBlocking()
		if blocking {
			if f := newFDConn(ac); f != nil {
				c = f
			} else {
				s.blocking.Add(-1)
				blocking = false
			}
		}
		if !s.track(c, true) {
			c.Close()
			release(c)
			if blocking {
				s.blocking.Add(-1)
			}
			return
		}
		s.wg.Add(1)
		go s.carry(c, blocking)
	}
}

func (s *Server) takeBlocking() bool {
	for {
		n := s.blocking.Load()
		if n >= BlockingConns {
			return false
		}
		if s.blocking.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

func (s *Server) track(c net.Conn, client bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = client
	return true
}

func (s *Server) forget(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *Server) carry(c halfConn, blocking bool) {
	defer s.wg.Done()
	if blocking {
		defer s.blocking.Add(-1)
	}
	defer s.forget(c)
	w := &watch{s: s}
	s.mu.Lock()
	s.watches[w] = true
	s.mu.Unlock()
	defer func() {
		w.end()
		s.mu.Lock()
		delete(s.watches, w)
		s.mu.Unlock()
	}()
	p := &pipe{s: s, client: c, w: w, blocking: blocking}
	if !s.paused.Load() && !p.open() {
		c.Close()
		release(c)
		return
	}
	p.up()
	p.down.Wait()
	s.addInflight(-p.inFlight.Load())
	c.Close()
	release(c)
	if p.guest != nil {
		p.guest.Close()
		s.forget(p.guest)
		release(p.guest)
	}
}

func release(c net.Conn) {
	if f, ok := c.(*fdConn); ok {
		f.release()
	}
}

type pipe struct {
	s      *Server
	client halfConn
	w      *watch
	guest  halfConn
	down   sync.WaitGroup

	blocking bool

	cmu     sync.Mutex
	replies records
	pending []byte

	inFlight atomic.Int64
}

func (p *pipe) begun() {
	p.inFlight.Add(1)
	p.s.addInflight(1)
}

func (p *pipe) replied() {
	p.w.reply()
	if p.inFlight.Load() > 0 {
		p.inFlight.Add(-1)
		p.s.addInflight(-1)
	}
}

const pendingMax = 64 << 10

func (p *pipe) open() bool {
	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	stream, err := p.s.open.Open(ctx, Request)
	cancel()
	if err != nil {
		p.s.log.Debug("view socket: the guest did not take the connection", "err", err)
		return false
	}
	var guest halfConn = stream
	if p.blocking {
		if f := newFDConn(stream); f != nil {
			guest = f
		}
	}
	if !p.s.track(guest, false) {
		guest.Close()
		release(guest)
		return false
	}
	p.guest = guest
	p.down.Add(1)
	go p.downHalf()
	return true
}

func (p *pipe) up() {
	u := upstream{begun: p.begun}
	buf := make([]byte, 128<<10)
	var marks viewhops.Marks
	for {
		n, err := p.client.Read(buf)
		if n > 0 {
			var t0 int64
			if hops != nil {
				t0 = viewhops.Now()
			}
			fwd, answers := u.feed(buf[:n], p.s.paused.Load, p.w.call)
			for _, c := range answers {
				p.s.answered.Add(1)
				p.s.log.Debug("view socket: answered in the guest's place", "xid", fmt.Sprintf("%08x", c.xid), "proc", c.proc, "tag", string(bytes.TrimSpace(c.tag)), "op", c.op)
				if p.answer(c) != nil {
					p.closeBoth()
					return
				}
			}
			for _, b := range fwd {
				if p.guest == nil && !p.open() {
					return
				}
				if _, werr := p.guest.Write(b); werr != nil {
					p.closeBoth()
					return
				}
			}
			if hops != nil {
				t1 := viewhops.Now()
				marks.Feed(buf[:n], func(xid, size uint32, rest []byte) {
					hops.Add(viewhops.Call, viewhops.MainOp(rest), xid, size, t0, t1)
				})
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && p.guest != nil {
				p.guest.CloseWrite()
			} else {
				p.closeBoth()
			}
			return
		}
	}
}

func (p *pipe) downHalf() {
	defer p.down.Done()
	buf := make([]byte, 128<<10)
	var marks viewhops.Marks
	for {
		n, err := p.guest.Read(buf)
		if n > 0 {
			var t0 int64
			if hops != nil {
				t0 = viewhops.Now()
			}
			p.cmu.Lock()
			_, werr := p.client.Write(buf[:n])
			if hops != nil {
				t1 := viewhops.Now()
				marks.Feed(buf[:n], func(xid, size uint32, _ []byte) { hops.Add(viewhops.Reply, 0, xid, size, t0, t1) })
			}
			p.replies.feed(buf[:n], p.replied)
			if werr == nil && len(p.pending) > 0 && p.replies.between() {
				_, werr = p.client.Write(p.pending)
				p.pending = nil
			}
			p.cmu.Unlock()
			if werr != nil {
				p.closeBoth()
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				p.client.CloseWrite()
			} else {
				p.closeBoth()
			}
			return
		}
	}
}

func (p *pipe) answer(c callHead) error {
	a := pausedAnswer(c)
	p.cmu.Lock()
	defer p.cmu.Unlock()
	if !p.replies.between() {
		if len(p.pending)+len(a) <= pendingMax {
			p.pending = append(p.pending, a...)
		}
		return nil
	}
	_, err := p.client.Write(a)
	return err
}

func (p *pipe) closeBoth() {
	p.client.Close()
	if p.guest != nil {
		p.guest.Close()
	}
}

type upstream struct {
	mark   [4]byte
	nmark  int
	left   uint32
	last   bool
	inFrag bool
	inRec  bool
	how    int
	head   []byte
	held   [][]byte
	c      callHead
	begun  func()
}

const (
	forward = iota
	undecided
	answered
)

func (u *upstream) feed(p []byte, paused func() bool, called func()) (fwd [][]byte, answers []callHead) {
	for len(p) > 0 || (u.inFrag && u.left == 0) {
		if !u.inRec {
			u.inRec = true
			u.how = forward
			if paused() {
				u.how = undecided
			} else if u.begun != nil {
				u.begun()
			}
		}
		var piece []byte
		payload := false
		if !u.inFrag {
			n := copy(u.mark[u.nmark:], p)
			piece, p = p[:n], p[n:]
			u.nmark += n
			if u.nmark == 4 {
				v := binary.BigEndian.Uint32(u.mark[:])
				u.nmark = 0
				u.left, u.last, u.inFrag = v&0x7fffffff, v&0x80000000 != 0, true
			}
		} else {
			n := min(uint32(len(p)), u.left)
			piece, p = p[:n], p[n:]
			u.left -= n
			payload = true
		}
		switch u.how {
		case forward:
			if len(piece) > 0 {
				fwd = append(fwd, piece)
			}
		case undecided:
			u.held = append(u.held, bytes.Clone(piece))
			if payload && len(u.head) < headMax {
				u.head = append(u.head, piece[:min(len(piece), headMax-len(u.head))]...)
			}
		}
		ended := false
		if u.inFrag && u.left == 0 {
			u.inFrag = false
			ended = u.last
		}
		if u.how == undecided && (payload || ended) {
			switch c, r := parseHead(u.head, ended); r {
			case headOurs:
				u.how, u.c = answered, c
				u.held = nil
			case headNotOurs:
				u.how = forward
				if u.begun != nil {
					u.begun()
				}
				fwd = append(fwd, u.held...)
				u.held = nil
			}
		}
		if ended {
			switch u.how {
			case forward:
				called()
			case answered:
				answers = append(answers, u.c)
			}
			u.inRec, u.head, u.held, u.c = false, u.head[:0], nil, callHead{}
		}
	}
	return fwd, answers
}

type records struct {
	mark   [4]byte
	nmark  int
	left   uint32
	last   bool
	inFrag bool
	open   bool
}

func (r *records) feed(p []byte, each func()) {
	for len(p) > 0 || (r.inFrag && r.left == 0) {
		if !r.inFrag {
			n := copy(r.mark[r.nmark:], p)
			r.nmark += n
			p = p[n:]
			r.open = true
			if r.nmark < 4 {
				return
			}
			v := binary.BigEndian.Uint32(r.mark[:])
			r.nmark = 0
			r.left, r.last, r.inFrag = v&0x7fffffff, v&0x80000000 != 0, true
		}
		n := min(uint32(len(p)), r.left)
		p = p[n:]
		r.left -= n
		if r.left == 0 {
			r.inFrag = false
			if r.last {
				r.open = false
				each()
			}
		}
	}
}

func (r *records) between() bool { return !r.open }

type watch struct {
	s     *Server
	mu    sync.Mutex
	out   int
	timer *time.Timer
	ended bool
}

func (w *watch) armLocked() {
	if w.timer == nil {
		w.timer = time.AfterFunc(w.s.stallAfter, w.fire)
		return
	}
	w.timer.Reset(w.s.stallAfter)
}

func (w *watch) call() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.out++
	if w.out == 1 && !w.ended {
		w.armLocked()
	}
}

func (w *watch) reply() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.out > 0 {
		w.out--
	}
	switch {
	case w.out > 0 && !w.ended:
		w.armLocked()
	case w.timer != nil:
		w.timer.Stop()
	}
}

func (w *watch) rearm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.out > 0 && !w.ended {
		w.armLocked()
	}
}

func (w *watch) end() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ended = true
	if w.timer != nil {
		w.timer.Stop()
	}
}

func (w *watch) fire() {
	w.mu.Lock()
	quiet := w.out > 0 && !w.ended
	w.mu.Unlock()
	if !quiet {
		return
	}
	if w.s.stalled() {
		w.rearm()
	}
}

func (s *Server) stalled() bool {
	s.mu.Lock()
	fn := s.stall
	if fn == nil || s.closed {
		s.mu.Unlock()
		return false
	}
	if s.checking {
		s.mu.Unlock()
		return true
	}
	s.checking = true
	s.mu.Unlock()
	again := fn()
	s.mu.Lock()
	s.checking = false
	s.mu.Unlock()
	return again
}

func (s *Server) Conns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, client := range s.conns {
		if client {
			n++
		}
	}
	return n
}

func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	err := s.ln.Close()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return err
}
