package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

const keepEnded = 64

const progressEvery = time.Second

type Operations struct {
	publish func(api.Operation)

	mu    sync.Mutex
	byID  map[string]*Operation
	ended []string
}

func NewOperations(publish func(api.Operation)) *Operations {
	return &Operations{publish: publish, byID: map[string]*Operation{}}
}

type Operation struct {
	ops    *Operations
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	op       api.Operation
	sent     time.Time
	done     chan struct{}
	standing string
}

func (op *Operation) noteOf(phase string) string {
	switch {
	case op.standing == "":
		return phase
	case phase == "":
		return op.standing
	}
	return op.standing + "; " + phase
}

func newOperationID(now time.Time) string {
	var b [6]byte
	rand.Read(b[:])
	return "op-" + now.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

func (t *Operations) Start(ctx context.Context, method, target, phase string, run func(op *Operation)) api.Operation {
	now := time.Now()
	wctx, cancel := context.WithCancel(context.Background())
	op := &Operation{ops: t, ctx: wctx, cancel: cancel, done: make(chan struct{}), sent: now,
		op: api.Operation{ID: newOperationID(now), Method: method, Target: target, State: api.OperationRunning, Phase: phase, Started: stamp(now)}}
	t.mu.Lock()
	t.byID[op.op.ID] = op
	t.mu.Unlock()
	first := op.Get()
	begin := func() {
		t.publish(first)
		go run(op)
	}
	if !jsonrpc.AfterReply(ctx, begin) {
		begin()
	}
	return first
}

func (op *Operation) Context() context.Context { return op.ctx }

func (op *Operation) Get() api.Operation {
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.op
}

func (op *Operation) Progress(phase string, done, total int64) {
	op.mu.Lock()
	if op.op.State != api.OperationRunning {
		op.mu.Unlock()
		return
	}
	if total > 0 && done > total {
		total = done
	}
	newPhase := phase != op.op.Phase
	if newPhase {
		op.op.Note = op.standing
	}
	op.op.Phase, op.op.DoneBytes, op.op.TotalBytes = phase, done, total
	now := time.Now()
	send := newPhase || now.Sub(op.sent) >= progressEvery
	if send {
		op.sent = now
	}
	cur := op.op
	op.mu.Unlock()
	if send {
		op.ops.publish(cur)
	}
}

func (op *Operation) ProgressNote(phase, note string) {
	op.mu.Lock()
	if op.op.State != api.OperationRunning {
		op.mu.Unlock()
		return
	}
	note = op.noteOf(note)
	send := phase != op.op.Phase || note != op.op.Note
	op.op.Phase, op.op.Note, op.op.DoneBytes, op.op.TotalBytes = phase, note, 0, 0
	if send {
		op.sent = time.Now()
	}
	cur := op.op
	op.mu.Unlock()
	if send {
		op.ops.publish(cur)
	}
}

func (op *Operation) Note(text string) {
	op.mu.Lock()
	if op.op.State != api.OperationRunning || op.standing == text {
		op.mu.Unlock()
		return
	}
	op.standing = text
	op.op.Note = text
	op.sent = time.Now()
	cur := op.op
	op.mu.Unlock()
	op.ops.publish(cur)
}

func (op *Operation) end(change func(*api.Operation)) {
	op.mu.Lock()
	if op.op.State != api.OperationRunning {
		op.mu.Unlock()
		return
	}
	change(&op.op)
	op.op.Ended = stamp(time.Now())
	cur := op.op
	close(op.done)
	op.mu.Unlock()
	op.cancel()
	t := op.ops
	t.mu.Lock()
	t.ended = append(t.ended, cur.ID)
	for len(t.ended) > keepEnded {
		delete(t.byID, t.ended[0])
		t.ended = t.ended[1:]
	}
	t.mu.Unlock()
	t.publish(cur)
}

func (op *Operation) Done(result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		op.Fail(err)
		return
	}
	op.end(func(o *api.Operation) { o.State, o.Result = api.OperationDone, raw })
}

func (op *Operation) Fail(err error) {
	if op.ctx.Err() != nil {
		op.Cancelled()
		return
	}
	op.end(func(o *api.Operation) { o.State, o.Error = api.OperationFailed, clip(err.Error(), 2000) })
}

func (op *Operation) Cancelled() {
	op.end(func(o *api.Operation) { o.State = api.OperationCancelled })
}

func (t *Operations) Get(id string) (*Operation, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	op, ok := t.byID[id]
	return op, ok
}

const cancelWait = 5 * time.Second

func (t *Operations) Cancel(ctx context.Context, id string) (api.Operation, error) {
	op, ok := t.Get(id)
	if !ok {
		return api.Operation{}, jsonrpc.Errorf(api.CodeFailed, "no operation %q", clip(id, 80))
	}
	op.cancel()
	wait, stop := context.WithTimeout(ctx, cancelWait)
	defer stop()
	select {
	case <-op.done:
	case <-wait.Done():
	}
	return op.Get(), nil
}

func (t *Operations) Close() {
	t.mu.Lock()
	var all []*Operation
	for _, op := range t.byID {
		all = append(all, op)
	}
	t.mu.Unlock()
	for _, op := range all {
		op.cancel()
	}
}

func (t *Operations) handle(method string) MethodFunc {
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := jsonrpc.Bind[api.OperationID](params)
		if err != nil {
			return nil, err
		}
		if method == api.MethodOperationCancel {
			return t.Cancel(ctx, p.ID)
		}
		op, ok := t.Get(p.ID)
		if !ok {
			return nil, jsonrpc.Errorf(api.CodeFailed, "no operation %q: it never was, or 64 newer ones have ended, or the daemon has restarted since", clip(p.ID, 80))
		}
		return op.Get(), nil
	}
}
