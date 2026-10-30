package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"

	"fev/guest/internal/logbuf"
)

type Checker interface {
	StorageCheck(follow bool) (api.StorageCheck, <-chan struct{}, error)
	StorageCheckNow() *api.StorageCheck
}

var ErrNoCheck = errors.New(api.NoCheck)

var opIDs atomic.Uint64

type checkOp struct {
	id      uint64
	done    <-chan struct{}
	waiters []checkWaiter
}

type checkWaiter struct {
	c       *jsonrpc.Conn
	replied chan struct{}
}

type checks struct {
	mu  sync.Mutex
	cur *checkOp
}

func (s *Server) storageCheck(ctx context.Context, c *jsonrpc.Conn, st *connState, follow bool) (any, error) {
	chk, ok := s.Sys.(Checker)
	if !ok {
		return nil, failed(ErrNotImplemented)
	}
	s.check.mu.Lock()
	now, done, err := chk.StorageCheck(follow)
	if err != nil {
		s.check.mu.Unlock()
		return nil, failed(err)
	}
	op := s.check.cur
	if op == nil || op.done != done {
		op = &checkOp{id: opIDs.Add(1), done: done}
		s.check.cur = op
		go s.endCheck(chk, op)
	}
	w := checkWaiter{c: c, replied: make(chan struct{})}
	op.waiters = append(op.waiters, w)
	id := op.id
	s.check.mu.Unlock()
	st.mu.Lock()
	client := st.client
	st.mu.Unlock()
	s.Log.Logf(logbuf.Info, "agent: storage check %d asked for by %q (auto: %v, follow: %v)", id, client, now.Auto, follow)
	jsonrpc.AfterReply(ctx, func() {
		defer close(w.replied)
		select {
		case <-done:
		default:
			c.Notify(api.NotifyOperationProgress, api.OperationProgress{
				ID: id, Method: api.MethodStorageCheck, State: api.OperationRunning,
				Phase: api.PhaseScrub, DoneBytes: now.ScrubbedBytes,
			})
		}
	})
	return api.Operation{ID: id}, nil
}

func (s *Server) endCheck(chk Checker, op *checkOp) {
	<-op.done
	s.check.mu.Lock()
	if s.check.cur == op {
		s.check.cur = nil
	}
	waiters := op.waiters
	op.waiters = nil
	last := chk.StorageCheckNow()
	s.check.mu.Unlock()
	if last == nil {
		return
	}
	p := api.OperationProgress{ID: op.id, Method: api.MethodStorageCheck, State: last.State, Phase: api.PhaseScrub, DoneBytes: last.ScrubbedBytes}
	switch last.State {
	case api.OperationDone:
		p.Result, _ = json.Marshal(last)
	case api.OperationFailed, api.OperationCancelled:
		p.Error = last.Error
		if p.State == api.OperationFailed && p.Error == "" {
			p.Error = "the check failed"
		}
		if p.State == api.OperationCancelled {
			p.Error = ""
		}
	}
	for _, w := range waiters {
		go func() {
			select {
			case <-w.replied:
			case <-w.c.Context().Done():
				return
			}
			w.c.Notify(api.NotifyOperationProgress, p)
		}()
	}
}
