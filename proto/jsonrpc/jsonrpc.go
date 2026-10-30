package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

const MaxMessage = 4 << 20

const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

type message struct {
	Version string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *Error           `json:"error,omitempty"`
}

type Handler interface {
	Handle(ctx context.Context, c *Conn, method string, params json.RawMessage) (any, error)
}

type HandlerFunc func(ctx context.Context, c *Conn, method string, params json.RawMessage) (any, error)

func (f HandlerFunc) Handle(ctx context.Context, c *Conn, method string, params json.RawMessage) (any, error) {
	return f(ctx, c, method, params)
}

var ErrClosed = errors.New("jsonrpc: connection closed")

type Conn struct {
	rw      io.ReadWriteCloser
	handler Handler

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]*call
	closed  bool
	err     error

	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	drained    chan struct{}
	closedHere atomic.Bool
	notes      chan *message

	values sync.Map
}

type call struct {
	ch   chan *message
	then func(*message)
}

func NewConn(rw io.ReadWriteCloser, handler Handler) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		rw:      rw,
		handler: handler,
		pending: make(map[uint64]*call),
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		drained: make(chan struct{}),
		notes:   make(chan *message, 256),
	}
	go c.read()
	go func() {
		defer close(c.drained)
		for {
			select {
			case m := <-c.notes:
				c.serve(m)
			case <-c.done:
				for !c.closedHere.Load() {
					select {
					case m := <-c.notes:
						c.serve(m)
					default:
						return
					}
				}
				return
			}
		}
	}()
	return c
}

func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) Drained() <-chan struct{} { return c.drained }

func (c *Conn) Context() context.Context { return c.ctx }

func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) Close() error {
	c.closedHere.Store(true)
	c.shutdown(nil)
	return nil
}

func (c *Conn) Set(key, value any)      { c.values.Store(key, value) }
func (c *Conn) Get(key any) (any, bool) { return c.values.Load(key) }

func (c *Conn) shutdown(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.err = err
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()

	c.cancel()
	c.rw.Close()
	for _, p := range pending {
		close(p.ch)
	}
	close(c.done)
}

func (c *Conn) write(m *message) error {
	m.Version = "2.0"
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(data) > MaxMessage {
		return fmt.Errorf("jsonrpc: message of %d bytes exceeds the %d byte limit", len(data), MaxMessage)
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.rw.Write(data); err != nil {
		c.shutdown(err)
		return ErrClosed
	}
	return nil
}

func (c *Conn) read() {
	r := bufio.NewReaderSize(c.rw, 64<<10)
	for {
		line, err := readLine(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			c.shutdown(err)
			return
		}
		if len(line) == 0 {
			continue
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil || m.Version != "2.0" {
			c.write(&message{ID: nullID(), Error: Errorf(CodeParseError, "malformed JSON-RPC message")})
			continue
		}
		switch {
		case m.Method != "" && m.ID == nil:
			select {
			case c.notes <- &m:
			case <-c.done:
				return
			}
		case m.Method != "":
			go c.serve(&m)
		case m.ID != nil:
			c.deliver(&m)
		}
	}
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > MaxMessage {
			return nil, fmt.Errorf("jsonrpc: peer sent a message longer than %d bytes", MaxMessage)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		return line[:len(line)-1], nil
	}
}

func nullID() *json.RawMessage {
	id := json.RawMessage("null")
	return &id
}

type afterReplyKey struct{}

func AfterReply(ctx context.Context, fn func()) bool {
	if hooks, ok := ctx.Value(afterReplyKey{}).(*[]func()); ok {
		*hooks = append(*hooks, fn)
		return true
	}
	return false
}

func (c *Conn) serve(m *message) {
	var result any
	var err error
	var hooks []func()
	if c.handler == nil {
		err = Errorf(CodeMethodNotFound, "method %q not found", m.Method)
	} else {
		ctx := c.ctx
		if m.ID != nil {
			ctx = context.WithValue(ctx, afterReplyKey{}, &hooks)
		}
		result, err = c.callHandler(ctx, m)
	}
	if m.ID == nil {
		return
	}
	defer func() {
		for _, fn := range hooks {
			fn()
		}
	}()
	reply := &message{ID: m.ID}
	if err != nil {
		var rpcErr *Error
		if !errors.As(err, &rpcErr) {
			rpcErr = Errorf(CodeInternalError, "%v", err)
		}
		reply.Error = rpcErr
	} else {
		data, merr := json.Marshal(result)
		if merr != nil {
			reply.Error = Errorf(CodeInternalError, "cannot encode result: %v", merr)
		} else {
			reply.Result = data
		}
	}
	c.write(reply)
}

func (c *Conn) callHandler(ctx context.Context, m *message) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = Errorf(CodeInternalError, "internal error in %s: %v", m.Method, r)
		}
	}()
	return c.handler.Handle(ctx, c, m.Method, m.Params)
}

func (c *Conn) deliver(m *message) {
	var id uint64
	if json.Unmarshal(*m.ID, &id) != nil {
		return
	}
	c.mu.Lock()
	p := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if p != nil {
		if p.then != nil {
			p.then(m)
		}
		p.ch <- m
	}
}

func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	return c.call(ctx, method, params, result, nil)
}

func (c *Conn) CallThen(ctx context.Context, method string, params, result any, then func()) error {
	return c.call(ctx, method, params, result, then)
}

func (c *Conn) call(ctx context.Context, method string, params, result any, then func()) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	p := &call{ch: make(chan *message, 1)}
	var decodeErr error
	if then != nil {
		p.then = func(m *message) {
			if m.Error != nil {
				return
			}
			if result != nil {
				decodeErr = json.Unmarshal(m.Result, result)
			}
			if decodeErr == nil {
				then()
			}
		}
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = p
	c.mu.Unlock()

	idRaw := json.RawMessage(fmt.Sprintf("%d", id))
	if err := c.write(&message{ID: &idRaw, Method: method, Params: raw}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	finish := func(m *message, ok bool) error {
		if !ok {
			return ErrClosed
		}
		if m.Error != nil {
			return m.Error
		}
		if then != nil {
			return decodeErr
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(m.Result, result)
	}
	select {
	case m, ok := <-p.ch:
		return finish(m, ok)
	case <-ctx.Done():
		c.mu.Lock()
		_, waiting := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if waiting || then == nil {
			return ctx.Err()
		}
		m, ok := <-p.ch
		return finish(m, ok)
	}
}

func (c *Conn) Notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}
	return c.write(&message{Method: method, Params: raw})
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	return json.Marshal(params)
}

func Bind[T any](params json.RawMessage) (T, error) {
	var v T
	if len(params) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(params, &v); err != nil {
		return v, Errorf(CodeInvalidParams, "invalid parameters: %v", err)
	}
	return v, nil
}
