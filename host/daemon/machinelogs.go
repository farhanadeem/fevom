package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"fev/host/daemon/logs"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/relay"
)

func machineSources(sources []string) bool {
	for _, s := range sources {
		if strings.HasPrefix(s, api.LogMachinePrefix) {
			return true
		}
	}
	return false
}

func machineOfSource(source string) string {
	name, ok := strings.CutPrefix(source, api.LogMachinePrefix)
	if !ok {
		return ""
	}
	name = strings.TrimSuffix(name, ":runtime")
	if relay.CheckMachineName(name) != nil {
		return ""
	}
	return name
}

const machineLogQueueLines = 2 * logs.QueueLines

type machineLogQueue struct {
	mu      sync.Mutex
	items   []api.LogEntry
	dropped int
	marked  bool
	closed  bool
	wake    chan struct{}
}

func (q *machineLogQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *machineLogQueue) push(e api.LogEntry) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if len(q.items) >= machineLogQueueLines {
		q.dropped++
		return
	}
	e.Dropped, q.dropped = q.dropped, 0
	q.items = append(q.items, e)
	q.signal()
}

func (q *machineLogQueue) mark() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.marked {
		return
	}
	q.marked = true
	q.items = append(q.items, api.LogEntry{})
	q.signal()
}

func (q *machineLogQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *machineLogQueue) next(stop <-chan struct{}) (api.LogEntry, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			e := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return e, true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return api.LogEntry{}, false
		}
		select {
		case <-q.wake:
		case <-stop:
			return api.LogEntry{}, false
		}
	}
}

func (d *Daemon) subscribeMachineLogs(ctx context.Context, c *jsonrpc.Conn, cl *client, p api.LogSubscribeParams) (any, error) {
	for _, s := range p.Sources {
		if machineOfSource(s) == "" {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "no log source %q: a machine's sources (%sNAME, %sNAME:runtime) are not mixed with others", clip(s, 60), api.LogMachinePrefix, api.LogMachinePrefix)
		}
	}
	info := d.methods[api.MethodMachinesInfo]
	if info == nil {
		return nil, jsonrpc.Errorf(api.CodeNoMachine, "this daemon has no machines")
	}
	for _, s := range p.Sources {
		params, _ := json.Marshal(api.MachineName{Name: machineOfSource(s)})
		if _, err := info(ctx, params); err != nil {
			return nil, err
		}
	}
	h := moduleHost{d}
	if err := h.StartAsked(); err != nil {
		return nil, jsonrpc.Errorf(api.CodeShuttingDown, "%v", err)
	}
	usable := func(st api.Status) bool {
		return st.State == api.StateRunning || (st.State == api.StateStarting && st.Phase == api.PhaseEngine)
	}
	st, err := h.WaitStatus(ctx, func(st api.Status) bool { return usable(st) || st.State == api.StateStopped })
	if err != nil {
		return nil, err
	}
	vm, ok := d.sup.VM()
	if !usable(st) || !ok {
		why := st.Error
		if why == "" {
			why = "the VM was stopped before it was running"
		}
		return nil, jsonrpc.Errorf(api.CodeFailed, "the machine's log is in the VM, which did not start: %s", why)
	}
	guest := vm.Agent()
	q := &machineLogQueue{wake: make(chan struct{}, 1)}
	deliver := func(e agent.LogEntry) {
		if e.TimeMS < 0 || e.TimeMS > 3e12 {
			e.TimeMS = 0
		}
		q.push(api.LogEntry{Source: e.Source, Time: stamp(time.Now()), GuestMS: e.TimeMS, Level: min(max(e.Level, 0), 7), Text: e.Text})
	}
	var end func()
	if p.Backlog > 0 {
		end = q.mark
	}
	unsubscribe, err := guest.SubscribeLogsMarked(ctx, agent.LogSubscribeParams{Sources: p.Sources, Backlog: p.Backlog > 0}, deliver, end)
	if err != nil {
		return nil, guestError(err)
	}
	id := d.nextSub.Add(1)
	stopped := make(chan struct{})
	cl.add(id, func() {
		q.close()
		close(stopped)
		go unsubscribe()
	})
	go func() {
		select {
		case <-guest.Done():
			q.push(api.LogEntry{Source: api.LogDaemon, Time: stamp(time.Now()), Level: 4, Text: "the VM stopped: the machine's log ends here"})
			if end != nil {
				q.mark()
			}
		case <-stopped:
		}
	}()
	jsonrpc.AfterReply(ctx, func() { go pumpMachineLogs(c, id, q) })
	return api.Subscription{Subscription: id}, nil
}

func pumpMachineLogs(c *jsonrpc.Conn, id uint64, q *machineLogQueue) {
	for {
		e, ok := q.next(c.Done())
		if !ok {
			return
		}
		var err error
		if e.Source == "" {
			err = c.Notify(api.NotifyLogBacklogEnd, api.Subscription{Subscription: id})
		} else {
			e.Subscription = id
			err = c.Notify(api.NotifyLogEntry, e)
		}
		if err != nil {
			return
		}
	}
}
