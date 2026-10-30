package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
	"fev/proto/relay"

	"fev/guest/internal/logbuf"
)

var streamWait = 10 * time.Second

const progressEvery = time.Second

type operations struct {
	mu      sync.Mutex
	byID    map[uint64]*operation
	byToken map[string]*operation
}

type operation struct {
	s       *Server
	id      uint64
	method  string
	c       *jsonrpc.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	replied chan struct{}

	token        string
	attach       chan net.Conn
	releaseOnce  sync.Once
	released     chan struct{}
	mu           sync.Mutex
	phase        string
	lastProgress time.Time
}

func newToken() (string, error) {
	var b [relay.TokenLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Server) startOperation(ctx context.Context, c *jsonrpc.Conn, method string, stream bool, run func(ctx context.Context, op *operation) (any, error)) (api.Operation, error) {
	op := &operation{s: s, id: opIDs.Add(1), method: method, c: c, replied: make(chan struct{}), released: make(chan struct{})}
	op.ctx, op.cancel = context.WithCancel(context.Background())
	if stream {
		token, err := newToken()
		if err != nil {
			op.cancel()
			return api.Operation{}, err
		}
		op.token, op.attach = token, make(chan net.Conn, 1)
	}
	s.ops.mu.Lock()
	if s.ops.byID == nil {
		s.ops.byID, s.ops.byToken = map[uint64]*operation{}, map[string]*operation{}
	}
	s.ops.byID[op.id] = op
	if op.token != "" {
		s.ops.byToken[op.token] = op
	}
	s.ops.mu.Unlock()
	jsonrpc.AfterReply(ctx, func() { close(op.replied) })
	go op.run(run)
	return api.Operation{ID: op.id, Token: op.token}, nil
}

func (op *operation) run(run func(ctx context.Context, op *operation) (any, error)) {
	s := op.s
	stop := context.AfterFunc(op.c.Context(), op.cancel)
	defer func() {
		stop()
		op.cancel()
		op.release()
		s.ops.mu.Lock()
		delete(s.ops.byID, op.id)
		if op.token != "" {
			delete(s.ops.byToken, op.token)
		}
		s.ops.mu.Unlock()
	}()
	select {
	case <-op.replied:
	case <-op.ctx.Done():
	}
	result, err := run(op.ctx, op)
	op.mu.Lock()
	end := api.OperationProgress{ID: op.id, Method: op.method, State: api.OperationDone, Phase: op.phase}
	op.mu.Unlock()
	switch {
	case err != nil && op.ctx.Err() != nil:
		end.State = api.OperationCancelled
		s.Log.Logf(logbuf.Info, "agent: %s %d cancelled: %v", op.method, op.id, err)
	case err != nil:
		end.State, end.Error = api.OperationFailed, err.Error()
		s.Log.Logf(logbuf.Warning, "agent: %s %d failed: %v", op.method, op.id, err)
		if result != nil {
			end.Result, _ = json.Marshal(result)
		}
	default:
		end.Result, _ = json.Marshal(result)
		s.Log.Logf(logbuf.Info, "agent: %s %d done", op.method, op.id)
	}
	select {
	case <-op.replied:
		op.c.Notify(api.NotifyOperationProgress, end)
	case <-op.c.Context().Done():
	}
}

func (op *operation) progress(phase string, done, total int64) {
	op.mu.Lock()
	now := time.Now()
	if phase == op.phase && now.Sub(op.lastProgress) < progressEvery {
		op.mu.Unlock()
		return
	}
	op.phase, op.lastProgress = phase, now
	op.mu.Unlock()
	select {
	case <-op.replied:
	default:
		return
	}
	op.c.Notify(api.NotifyOperationProgress, api.OperationProgress{
		ID: op.id, Method: op.method, State: api.OperationRunning, Phase: phase, DoneBytes: done, TotalBytes: total,
	})
}

func (op *operation) stream() (net.Conn, error) {
	t := time.NewTimer(streamWait)
	defer t.Stop()
	select {
	case conn := <-op.attach:
		context.AfterFunc(op.ctx, func() { conn.Close() })
		return conn, nil
	case <-t.C:
		return nil, fmt.Errorf("nobody attached the operation's stream within %v", streamWait)
	case <-op.ctx.Done():
		return nil, op.ctx.Err()
	}
}

func (op *operation) release() {
	op.releaseOnce.Do(func() { close(op.released) })
}

func (s *Server) AttachStream(token string, conn net.Conn) bool {
	s.ops.mu.Lock()
	op := s.ops.byToken[token]
	delete(s.ops.byToken, token)
	s.ops.mu.Unlock()
	if op == nil {
		return false
	}
	if err := relay.WriteOK(conn); err != nil {
		op.cancel()
		return true
	}
	op.attach <- conn
	<-op.released
	return true
}

func (s *Server) cancelOperation(id uint64) {
	s.ops.mu.Lock()
	op := s.ops.byID[id]
	s.ops.mu.Unlock()
	if op != nil {
		s.Log.Logf(logbuf.Info, "agent: %s %d: cancel asked for", op.method, op.id)
		op.cancel()
	}
}
