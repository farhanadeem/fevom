package k8s

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

const (
	MethodStart      = "cluster.start"
	MethodLAN        = "cluster.lan"
	NotifyReach      = "cluster.reach"
	MethodExecResize = "exec.resize"
	MethodExecClose  = "exec.close"
	NotifyExecExit   = "exec.exit"
)

type LANParams struct {
	LAN bool `json:"lan"`
}

type ExecOpened struct {
	ID        uint64 `json:"id"`
	Token     string `json:"token"`
	Container string `json:"container"`
}

type ExecResize struct {
	ID   uint64 `json:"id"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
}

type ExecID struct {
	ID uint64 `json:"id"`
}

type ExecExit struct {
	ID       uint64 `json:"id"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

const streamAttach = 10 * time.Second

type Server struct {
	B Backend

	mu     sync.Mutex
	nextID uint64
	execs  map[string]*serverExec
	byID   map[uint64]*serverExec
}

type serverExec struct {
	id     uint64
	conn   *jsonrpc.Conn
	p      api.K8sExecParams
	resize chan [2]int
	stream chan net.Conn
	cancel context.CancelFunc
}

func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.execs == nil {
		s.execs, s.byID = map[string]*serverExec{}, map[uint64]*serverExec{}
	}
	s.mu.Unlock()
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go s.accept(c)
	}
}

func (s *Server) accept(c net.Conn) {
	r := bufio.NewReader(c)
	head, err := r.Peek(5)
	if err != nil {
		c.Close()
		return
	}
	if string(head) == "exec " {
		line, err := r.ReadString('\n')
		if err != nil {
			c.Close()
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(line, "exec "))
		s.mu.Lock()
		e := s.execs[token]
		delete(s.execs, token)
		s.mu.Unlock()
		if e == nil {
			io.WriteString(c, "no such exec\n")
			c.Close()
			return
		}
		io.WriteString(c, "ok\n")
		e.stream <- &bufConn{Conn: c, r: r}
		return
	}
	s.control(&bufConn{Conn: c, r: r})
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *bufConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

type controlState struct {
	mu   sync.Mutex
	subs map[uint64]context.CancelFunc
	next uint64
	stop context.CancelFunc
}

func (s *Server) control(c net.Conn) {
	st := &controlState{subs: map[uint64]context.CancelFunc{}}
	conn := jsonrpc.NewConn(c, jsonrpc.HandlerFunc(func(ctx context.Context, conn *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
		return s.handle(ctx, conn, st, method, params)
	}))
	<-conn.Done()
	st.mu.Lock()
	for _, cancel := range st.subs {
		cancel()
	}
	if st.stop != nil {
		st.stop()
	}
	st.mu.Unlock()
}

func (st *controlState) open(parent *jsonrpc.Conn) (uint64, context.Context) {
	ctx, cancel := context.WithCancel(parent.Context())
	st.mu.Lock()
	st.next++
	id := st.next
	st.subs[id] = cancel
	st.mu.Unlock()
	return id, ctx
}

func (st *controlState) end(id uint64) bool {
	st.mu.Lock()
	cancel := st.subs[id]
	delete(st.subs, id)
	st.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return cancel != nil
}

func (s *Server) handle(ctx context.Context, conn *jsonrpc.Conn, st *controlState, method string, params json.RawMessage) (any, error) {
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	switch method {
	case MethodStart:
		cfg, err := jsonrpc.Bind[Config](params)
		if err != nil {
			return nil, err
		}
		wctx, cancel := context.WithCancel(conn.Context())
		st.mu.Lock()
		if st.stop != nil {
			st.stop()
		}
		st.stop = cancel
		st.mu.Unlock()
		if err := s.B.Start(wctx, cfg, func(r Reach) { conn.Notify(NotifyReach, r) }); err != nil {
			cancel()
			return nil, err
		}
		return api.Empty{}, nil
	case MethodLAN:
		p, err := jsonrpc.Bind[LANParams](params)
		if err != nil {
			return nil, err
		}
		s.B.SetLAN(p.LAN)
		return api.Empty{}, nil
	case api.MethodK8sWatch:
		p, err := jsonrpc.Bind[api.K8sWatchParams](params)
		if err != nil {
			return nil, err
		}
		id, wctx := st.open(conn)
		var mu sync.Mutex
		var held []api.K8sChanged
		replied := false
		objects, err := s.B.Watch(wctx, p, func(ch api.K8sChanged) {
			ch.Subscription = id
			mu.Lock()
			defer mu.Unlock()
			if !replied {
				held = append(held, ch)
				return
			}
			conn.Notify(api.NotifyK8sChanged, ch)
		})
		if err != nil {
			st.end(id)
			return nil, err
		}
		if objects == nil {
			objects = []api.K8sObject{}
		}
		jsonrpc.AfterReply(ctx, func() {
			mu.Lock()
			defer mu.Unlock()
			replied = true
			for _, ch := range held {
				conn.Notify(api.NotifyK8sChanged, ch)
			}
			held = nil
		})
		return api.K8sWatching{Subscription: id, Objects: objects}, nil
	case api.MethodK8sUnwatch:
		p, err := jsonrpc.Bind[api.Subscription](params)
		if err != nil {
			return nil, err
		}
		st.end(p.Subscription)
		return api.Empty{}, nil
	case api.MethodK8sGet:
		p, err := jsonrpc.Bind[api.K8sRef](params)
		if err != nil {
			return nil, err
		}
		return s.B.Get(ctx, p)
	case api.MethodK8sApply:
		p, err := jsonrpc.Bind[api.K8sApplyParams](params)
		if err != nil {
			return nil, err
		}
		return s.B.Apply(ctx, p)
	case api.MethodK8sUpdate:
		p, err := jsonrpc.Bind[api.K8sUpdateParams](params)
		if err != nil {
			return nil, err
		}
		return s.B.Update(ctx, p)
	case api.MethodK8sScale:
		p, err := jsonrpc.Bind[api.K8sScaleParams](params)
		if err != nil {
			return nil, err
		}
		return s.B.Scale(ctx, p)
	case api.MethodK8sRestart:
		p, err := jsonrpc.Bind[api.K8sRef](params)
		if err != nil {
			return nil, err
		}
		return s.B.Restart(ctx, p)
	case api.MethodK8sDelete:
		p, err := jsonrpc.Bind[api.K8sRef](params)
		if err != nil {
			return nil, err
		}
		return api.Empty{}, s.B.Delete(ctx, p)
	case api.MethodK8sLogs:
		p, err := jsonrpc.Bind[api.K8sLogsParams](params)
		if err != nil {
			return nil, err
		}
		id, lctx := st.open(conn)
		jsonrpc.AfterReply(ctx, func() {
			go func() {
				err := s.B.Logs(lctx, p, func(text string) {
					conn.Notify(api.NotifyK8sLog, api.K8sLog{Subscription: id, Text: text})
				})
				if st.end(id) {
					end := api.K8sLogEnd{Subscription: id}
					if err != nil {
						end.Error = oneLine(err.Error())
					}
					conn.Notify(api.NotifyK8sLogEnd, end)
				}
			}()
		})
		return api.Subscription{Subscription: id}, nil
	case api.MethodK8sExec:
		p, err := jsonrpc.Bind[api.K8sExecParams](params)
		if err != nil {
			return nil, err
		}
		container, err := s.B.ExecTarget(ctx, p)
		if err != nil {
			return nil, err
		}
		p.Container = container
		return s.openExec(conn, p), nil
	case MethodExecResize:
		p, err := jsonrpc.Bind[ExecResize](params)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		e := s.byID[p.ID]
		s.mu.Unlock()
		if e != nil {
			select {
			case e.resize <- [2]int{p.Rows, p.Cols}:
			default:
			}
		}
		return api.Empty{}, nil
	case MethodExecClose:
		p, err := jsonrpc.Bind[ExecID](params)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		e := s.byID[p.ID]
		s.mu.Unlock()
		if e != nil {
			e.cancel()
		}
		return api.Empty{}, nil
	}
	return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method %q not found", method)
}

func (s *Server) openExec(conn *jsonrpc.Conn, p api.K8sExecParams) ExecOpened {
	var b [16]byte
	rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	ctx, cancel := context.WithCancel(conn.Context())
	s.mu.Lock()
	s.nextID++
	e := &serverExec{id: s.nextID, conn: conn, p: p, resize: make(chan [2]int, 4), stream: make(chan net.Conn, 1), cancel: cancel}
	s.execs[token] = e
	s.byID[e.id] = e
	s.mu.Unlock()
	if p.Rows > 0 && p.Cols > 0 {
		e.resize <- [2]int{p.Rows, p.Cols}
	}
	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.execs, token)
			delete(s.byID, e.id)
			s.mu.Unlock()
		}()
		var stream net.Conn
		select {
		case stream = <-e.stream:
		case <-time.After(streamAttach):
			conn.Notify(NotifyExecExit, ExecExit{ID: e.id, ExitCode: -1, Error: "the exec's stream was not attached within 10s"})
			return
		case <-ctx.Done():
			conn.Notify(NotifyExecExit, ExecExit{ID: e.id, ExitCode: -1, Error: "closed before its stream was attached"})
			return
		}
		go func() { <-ctx.Done(); stream.Close() }()
		code, err := s.B.Exec(ctx, p, stream, e.resize)
		if cw, ok := stream.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		exit := ExecExit{ID: e.id, ExitCode: code}
		if err != nil && ctx.Err() == nil {
			exit.ExitCode, exit.Error = -1, oneLine(err.Error())
		}
		conn.Notify(NotifyExecExit, exit)
	}()
	return ExecOpened{ID: e.id, Token: token, Container: p.Container}
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

type Client struct {
	path string
	conn *jsonrpc.Conn

	mu      sync.Mutex
	reach   func(Reach)
	watches map[uint64]func(api.K8sChanged)
	logs    map[uint64]func(api.K8sLog, *api.K8sLogEnd)
	execs   map[uint64]chan ExecExit
}

func Dial(ctx context.Context, path string) (*Client, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	cl := &Client{path: path, watches: map[uint64]func(api.K8sChanged){},
		logs: map[uint64]func(api.K8sLog, *api.K8sLogEnd){}, execs: map[uint64]chan ExecExit{}}
	cl.conn = jsonrpc.NewConn(c, jsonrpc.HandlerFunc(cl.handle))
	return cl, nil
}

func (c *Client) Done() <-chan struct{} { return c.conn.Done() }

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) handle(_ context.Context, _ *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
	switch method {
	case NotifyReach:
		r, err := jsonrpc.Bind[Reach](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		fn := c.reach
		c.mu.Unlock()
		if fn != nil {
			fn(r)
		}
	case api.NotifyK8sChanged:
		ch, err := jsonrpc.Bind[api.K8sChanged](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		fn := c.watches[ch.Subscription]
		c.mu.Unlock()
		if fn != nil {
			fn(ch)
		}
	case api.NotifyK8sLog:
		l, err := jsonrpc.Bind[api.K8sLog](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		fn := c.logs[l.Subscription]
		c.mu.Unlock()
		if fn != nil {
			fn(l, nil)
		}
	case api.NotifyK8sLogEnd:
		e, err := jsonrpc.Bind[api.K8sLogEnd](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		fn := c.logs[e.Subscription]
		delete(c.logs, e.Subscription)
		c.mu.Unlock()
		if fn != nil {
			fn(api.K8sLog{}, &e)
		}
	case NotifyExecExit:
		e, err := jsonrpc.Bind[ExecExit](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		ch := c.execs[e.ID]
		delete(c.execs, e.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- e
		}
	}
	return nil, nil
}

func (c *Client) Start(ctx context.Context, cfg Config, reach func(Reach)) error {
	c.mu.Lock()
	c.reach = reach
	c.mu.Unlock()
	return c.conn.Call(ctx, MethodStart, cfg, nil)
}

func (c *Client) SetLAN(ctx context.Context, lan bool) error {
	return c.conn.Call(ctx, MethodLAN, LANParams{LAN: lan}, nil)
}

func (c *Client) Watch(ctx context.Context, p api.K8sWatchParams, deliver func(api.K8sChanged)) ([]api.K8sObject, func(), error) {
	var r api.K8sWatching
	err := c.conn.CallThen(ctx, api.MethodK8sWatch, p, &r, func() {
		c.mu.Lock()
		c.watches[r.Subscription] = deliver
		c.mu.Unlock()
	})
	if err != nil {
		return nil, nil, err
	}
	return r.Objects, func() {
		c.mu.Lock()
		delete(c.watches, r.Subscription)
		c.mu.Unlock()
		c.conn.Call(context.Background(), api.MethodK8sUnwatch, api.Subscription{Subscription: r.Subscription}, nil)
	}, nil
}

func (c *Client) Get(ctx context.Context, ref api.K8sRef) (*api.K8sManifest, error) {
	var r api.K8sManifest
	return &r, c.conn.Call(ctx, api.MethodK8sGet, ref, &r)
}

func (c *Client) Apply(ctx context.Context, p api.K8sApplyParams) (*api.K8sApplyResult, error) {
	var r api.K8sApplyResult
	return &r, c.conn.Call(ctx, api.MethodK8sApply, p, &r)
}

func (c *Client) Update(ctx context.Context, p api.K8sUpdateParams) (*api.K8sApplyResult, error) {
	var r api.K8sApplyResult
	return &r, c.conn.Call(ctx, api.MethodK8sUpdate, p, &r)
}

func (c *Client) Scale(ctx context.Context, p api.K8sScaleParams) (*api.K8sObject, error) {
	var r api.K8sObject
	return &r, c.conn.Call(ctx, api.MethodK8sScale, p, &r)
}

func (c *Client) Restart(ctx context.Context, ref api.K8sRef) (*api.K8sObject, error) {
	var r api.K8sObject
	return &r, c.conn.Call(ctx, api.MethodK8sRestart, ref, &r)
}

func (c *Client) Delete(ctx context.Context, ref api.K8sRef) error {
	return c.conn.Call(ctx, api.MethodK8sDelete, ref, nil)
}

func (c *Client) Logs(ctx context.Context, p api.K8sLogsParams, text func(string), end func(error)) (func(), error) {
	var sub api.Subscription
	err := c.conn.CallThen(ctx, api.MethodK8sLogs, p, &sub, func() {
		c.mu.Lock()
		c.logs[sub.Subscription] = func(l api.K8sLog, e *api.K8sLogEnd) {
			if e == nil {
				text(l.Text)
				return
			}
			var err error
			if e.Error != "" {
				err = errors.New(e.Error)
			}
			end(err)
		}
		c.mu.Unlock()
	})
	if err != nil {
		return nil, err
	}
	return func() {
		c.mu.Lock()
		_, open := c.logs[sub.Subscription]
		delete(c.logs, sub.Subscription)
		c.mu.Unlock()
		if open {
			c.conn.Call(context.Background(), api.MethodK8sUnwatch, sub, nil)
		}
	}, nil
}

type Exec struct {
	c         *Client
	ID        uint64
	Container string
	Stream    *net.UnixConn
	exit      chan ExecExit
}

func (c *Client) Exec(ctx context.Context, p api.K8sExecParams) (*Exec, error) {
	var r ExecOpened
	exit := make(chan ExecExit, 1)
	err := c.conn.CallThen(ctx, api.MethodK8sExec, p, &r, func() {
		c.mu.Lock()
		c.execs[r.ID] = exit
		c.mu.Unlock()
	})
	if err != nil {
		return nil, err
	}
	e := &Exec{c: c, ID: r.ID, Container: r.Container, exit: exit}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.path)
	if err == nil {
		conn.SetDeadline(time.Now().Add(streamAttach))
		_, err = fmt.Fprintf(conn, "exec %s\n", r.Token)
		if err == nil {
			var line []byte
			b := make([]byte, 1)
			for err == nil && (len(line) == 0 || line[len(line)-1] != '\n') {
				_, err = conn.Read(b)
				line = append(line, b[0])
			}
			if err == nil && string(line) != "ok\n" {
				err = errors.New(strings.TrimSpace(string(line)))
			}
		}
		if err != nil {
			conn.Close()
		}
	}
	if err != nil {
		e.Close()
		return nil, fmt.Errorf("the exec's stream: %w", err)
	}
	conn.SetDeadline(time.Time{})
	e.Stream = conn.(*net.UnixConn)
	return e, nil
}

func (e *Exec) Resize(rows, cols int) error {
	return e.c.conn.Call(context.Background(), MethodExecResize, ExecResize{ID: e.ID, Rows: rows, Cols: cols}, nil)
}

func (e *Exec) Close() {
	e.c.conn.Call(context.Background(), MethodExecClose, ExecID{ID: e.ID}, nil)
}

func (e *Exec) Wait() ExecExit {
	select {
	case x := <-e.exit:
		return x
	case <-e.c.conn.Done():
		return ExecExit{ID: e.ID, ExitCode: -1, Error: "the cluster's client ended"}
	}
}
