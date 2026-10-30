package fakeguest

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"fev/host/netstack"
	"fev/proto/relay"
)

type Relay struct {
	Engine  *Engine
	Stream  func(token string, conn net.Conn) bool
	Exec    func(token string, conn net.Conn) bool
	MTCP    func(machine, target string, conn net.Conn) bool
	mu      sync.Mutex
	opened  map[string]int
	Streams atomic.Int32
}

func (r *Relay) Opened(line string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.opened[line]
}

func (r *Relay) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go r.ServeConn(conn)
	}
}

func (r *Relay) ServeConn(conn net.Conn) {
	defer conn.Close()
	req, err := relay.ReadRequest(conn)
	if err != nil {
		relay.WriteError(conn, relay.CodeBadRequest, err.Error())
		return
	}
	if req.Kind == relay.Exec {
		if r.Exec == nil || !r.Exec(req.Target, conn) {
			relay.WriteError(conn, relay.CodeDenied, "no such session")
		}
		return
	}
	if req.Kind == relay.MTCP {
		if r.MTCP == nil || !r.MTCP(req.Machine, req.Target, conn) {
			relay.WriteError(conn, relay.CodeDenied, "no such running machine")
		}
		return
	}
	if req.Kind == relay.Stream {
		if r.Stream == nil || !r.Stream(req.Target, conn) {
			relay.WriteError(conn, relay.CodeDenied, "no such stream request")
		}
		return
	}
	real, ok := r.Engine.resolve(string(req.Kind), req.Target)
	if !ok {
		relay.WriteError(conn, relay.CodeRefused, "connection refused")
		return
	}
	backend, err := net.Dial(string(req.Kind), real)
	if err != nil {
		relay.WriteError(conn, relay.CodeRefused, err.Error())
		return
	}
	defer backend.Close()
	if err := relay.WriteOK(conn); err != nil {
		return
	}
	r.mu.Lock()
	if r.opened == nil {
		r.opened = map[string]int{}
	}
	r.opened[req.String()]++
	r.mu.Unlock()
	r.Streams.Add(1)
	defer r.Streams.Add(-1)

	if req.Kind == relay.TCP {
		netstack.Splice(conn, backend)
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, relay.MaxDatagram)
		for {
			n, err := backend.Read(buf)
			if err != nil {
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
			if !errors.Is(err, io.EOF) {
				conn.Close()
			}
			break
		}
		if _, err := backend.Write(buf[:n]); err != nil {
			break
		}
	}
	backend.Close()
	<-done
}

type Guest struct {
	Engine *Engine
	Relay  *Relay
}

func New() *Guest {
	e := NewEngine()
	return &Guest{Engine: e, Relay: &Relay{Engine: e}}
}

const (
	PortEngine = 2375
	PortRelay  = relay.Port
)

func (g *Guest) Take(port uint32, l net.Listener) bool {
	switch port {
	case PortEngine:
		go g.Engine.Serve(l)
	case PortRelay:
		go g.Relay.Serve(l)
	default:
		return false
	}
	return true
}
