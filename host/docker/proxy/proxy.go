package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"fev/host/netstack"
)

type Dialer func(ctx context.Context) (net.Conn, error)

type mode int

const (
	holding mode = iota
	passing
	refusing
	onDemand
	parked
)

var WaitedAfter = 5 * time.Second

type Proxy struct {
	path string
	log  *slog.Logger
	l    *net.UnixListener

	mu            sync.Mutex
	mode          mode
	dial          Dialer
	reason        string
	held          []*net.UnixConn
	active        map[*pair]struct{}
	closed        bool
	spliced       uint64
	wg            sync.WaitGroup
	start         func() error
	started       bool
	waited        func()
	waitTimer     *time.Timer
	nudge         func()
	longLived     time.Duration
	longClose     func()
	fence         int
	fenceGate     chan struct{}
	fenceHeld     []*net.UnixConn
	tracked       map[*tracked]struct{}
	flightChanged chan struct{}
}

type pair struct{ client, engine net.Conn }

func Listen(path string, log *slog.Logger) (*Proxy, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	p := &Proxy{path: path, log: log, l: l, active: map[*pair]struct{}{},
		tracked: map[*tracked]struct{}{}, flightChanged: make(chan struct{})}
	p.wg.Add(1)
	go p.accept()
	return p, nil
}

func (p *Proxy) OnLongClose(lived time.Duration, fn func()) {
	p.mu.Lock()
	p.longLived, p.longClose = lived, fn
	p.mu.Unlock()
}

func (p *Proxy) Path() string { return p.path }

func (p *Proxy) accept() {
	defer p.wg.Done()
	var delay time.Duration
	for {
		conn, err := p.l.AcceptUnix()
		if err != nil {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if closed {
				return
			}
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			p.log.Warn("docker socket: accept failed", "err", err, "retry_in", delay)
			time.Sleep(delay)
			continue
		}
		delay = 0
		p.arrived(conn)
	}
}

func (p *Proxy) arrived(conn *net.UnixConn) {
	p.mu.Lock()
	switch {
	case p.closed:
		p.mu.Unlock()
		conn.Close()
	case p.mode == holding:
		p.held = append(p.held, conn)
		p.mu.Unlock()
	case p.mode == parked:
		p.held = append(p.held, conn)
		if p.waitTimer == nil && p.waited != nil {
			p.waitTimer = time.AfterFunc(WaitedAfter, p.waited)
		}
		nudge := p.nudge
		p.mu.Unlock()
		if nudge != nil {
			go nudge()
		}
	case p.mode == onDemand:
		p.held = append(p.held, conn)
		p.startLocked()
		p.mu.Unlock()
	case p.mode == passing && p.fence == fenceAll:
		p.fenceHeld = append(p.fenceHeld, conn)
		p.mu.Unlock()
	case p.mode == passing:
		dial := p.dial
		p.wg.Add(1)
		p.mu.Unlock()
		go p.join(conn, dial)
	default:
		reason := p.reason
		p.wg.Add(1)
		p.mu.Unlock()
		go p.refuse(conn, reason)
	}
}

type Released struct {
	Joined    int
	Abandoned int
}

func (p *Proxy) Release(dial Dialer) Released {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return Released{}
	}
	p.mode, p.dial = passing, dial
	p.disarm()
	held := p.held
	p.held = nil
	var r Released
	for i, isGone := range gone(held) {
		if isGone {
			held[i].Close()
			r.Abandoned++
			continue
		}
		r.Joined++
		p.wg.Add(1)
		go p.join(held[i], dial)
	}
	return r
}

func (p *Proxy) Hold() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.mode, p.dial = holding, nil
	p.disarm()
	p.endActive()
}

func (p *Proxy) Park(nudge, waited func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.disarm()
	p.mode, p.nudge, p.waited = parked, nudge, waited
	if len(p.held) > 0 && waited != nil {
		p.waitTimer = time.AfterFunc(WaitedAfter, waited)
	}
}

func (p *Proxy) StartOnDemand(start func() error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.disarm()
	p.mode, p.dial, p.start, p.started = onDemand, nil, start, false
	p.endActive()
	if len(p.held) > 0 {
		p.startLocked()
	}
}

func (p *Proxy) startLocked() {
	if p.started {
		return
	}
	p.started = true
	start := p.start
	go func() {
		if err := start(); err != nil {
			p.mu.Lock()
			still := p.mode == onDemand
			p.mu.Unlock()
			if still {
				p.Refuse("the VM could not be started: " + err.Error())
			}
		}
	}()
}

func (p *Proxy) disarm() {
	if p.waitTimer != nil {
		p.waitTimer.Stop()
		p.waitTimer = nil
	}
}

func (p *Proxy) Refuse(reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.mode, p.dial, p.reason = refusing, nil, reason
	p.disarm()
	p.endActive()
	held := p.held
	p.held = nil
	for i, isGone := range gone(held) {
		if isGone {
			held[i].Close()
			continue
		}
		p.wg.Add(1)
		go p.refuse(held[i], reason)
	}
}

func (p *Proxy) endActive() {
	for pr := range p.active {
		pr.client.Close()
		pr.engine.Close()
	}
}

func (p *Proxy) Counts() (held, active int, joined uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.held), len(p.active), p.spliced
}

func (p *Proxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.disarm()
	p.l.Close()
	for _, c := range p.held {
		c.Close()
	}
	p.held = nil
	for _, c := range p.fenceHeld {
		c.Close()
	}
	p.fenceHeld = nil
	if p.fence != fenceOff {
		p.fence = fenceOff
		close(p.fenceGate)
	}
	p.endActive()
	p.mu.Unlock()
	p.wg.Wait()
	os.Remove(p.path)
}

func (p *Proxy) join(client *net.UnixConn, dial Dialer) {
	defer p.wg.Done()
	engine, err := dial(context.Background())
	if err != nil {
		p.log.Warn("docker socket: cannot reach the engine", "err", err)
		writeUnavailable(client, "cannot reach the Docker engine: "+err.Error())
		client.Close()
		return
	}
	pr := &pair{client, engine}
	p.mu.Lock()
	if p.closed || p.mode != passing {
		p.mu.Unlock()
		client.Close()
		engine.Close()
		return
	}
	p.active[pr] = struct{}{}
	p.spliced++
	p.mu.Unlock()

	joined := time.Now()
	t := p.track()
	netstack.Splice(&trackedConn{Conn: client, read: t.fromClient}, &trackedConn{Conn: engine, read: t.fromEngine})
	t.untrack()
	client.Close()
	engine.Close()
	p.mu.Lock()
	delete(p.active, pr)
	lived, told := p.longLived, p.longClose
	p.mu.Unlock()
	if told != nil && time.Since(joined) >= lived {
		told()
	}
}

func (p *Proxy) refuse(client *net.UnixConn, reason string) {
	defer p.wg.Done()
	writeUnavailable(client, reason)
	client.Close()
}

func Unavailable(reason string) []byte {
	body, _ := json.Marshal(struct {
		Message string `json:"message"`
	}{"fev: " + reason})
	return fmt.Appendf(nil, "HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/json\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
}

const lingerFor = 2 * time.Second

func writeUnavailable(client *net.UnixConn, reason string) {
	client.SetDeadline(time.Now().Add(lingerFor))
	client.Read(make([]byte, 4096))
	client.Write(Unavailable(reason))
	client.CloseWrite()
	io.Copy(io.Discard, client)
}

func gone(conns []*net.UnixConn) []bool {
	out := make([]bool, len(conns))
	if len(conns) == 0 {
		return out
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return out
	}
	defer unix.Close(kq)
	for i, c := range conns {
		raw, err := c.SyscallConn()
		if err != nil {
			out[i] = true
			continue
		}
		raw.Control(func(fd uintptr) {
			change := []unix.Kevent_t{{Ident: uint64(fd), Filter: unix.EVFILT_WRITE, Flags: unix.EV_ADD | unix.EV_ONESHOT}}
			var events [1]unix.Kevent_t
			n, err := unix.Kevent(kq, change, events[:], &unix.Timespec{})
			if err != nil || n == 0 {
				change[0].Flags = unix.EV_DELETE
				unix.Kevent(kq, change, nil, &unix.Timespec{})
				return
			}
			out[i] = events[0].Flags&unix.EV_EOF != 0
		})
	}
	return out
}

func RaiseFileLimit() (uint64, error) {
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		return 0, err
	}
	want := lim.Max
	if perProc, err := unix.SysctlUint32("kern.maxfilesperproc"); err == nil && uint64(perProc) < want {
		want = uint64(perProc)
	}
	if want <= lim.Cur {
		return lim.Cur, nil
	}
	lim.Cur = want
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		return 0, err
	}
	return lim.Cur, nil
}
