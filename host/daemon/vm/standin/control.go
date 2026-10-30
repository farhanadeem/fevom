package standin

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"fev/host/daemon/boottrace"
	"fev/proto/jsonrpc"
	"fev/proto/version"
	"fev/proto/vmmctl"
)

type control struct {
	s *standin

	order sync.Mutex

	mu         sync.Mutex
	paused     bool
	pausedAt   time.Time
	pausedAtNS uint64
	resumed    chan struct{}
	pauses     uint64
	total      time.Duration
	conn       *jsonrpc.Conn
	resets     uint64
	held       uint64
}

func (c *control) wait() {
	for {
		c.mu.Lock()
		paused, resumed := c.paused, c.resumed
		c.mu.Unlock()
		if !paused {
			return
		}
		<-resumed
	}
}

func (c *control) parked() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused {
		return c.total + time.Since(c.pausedAt)
	}
	return c.total
}

func (c *control) hung() {
	if path := c.s.o.set["hang-file"]; path != "" {
		if _, err := os.Stat(path); err == nil {
			select {}
		}
	}
}

func (c *control) serve(f *os.File) {
	conn := jsonrpc.NewConn(f, jsonrpc.HandlerFunc(c.handle))
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
}

func (c *control) reset() {
	c.mu.Lock()
	conn := c.conn
	c.resets++
	n := c.resets
	c.mu.Unlock()
	if conn != nil {
		conn.Notify(vmmctl.NotifyReset, vmmctl.Reset{Count: n, AtNS: boottrace.Now()})
	}
}

func (c *control) space(available, image, held uint64) {
	c.mu.Lock()
	conn := c.conn
	c.held = held
	c.mu.Unlock()
	if conn != nil {
		c.s.logf("control: space: %d bytes available, image %d, %d held", available, image, held)
		conn.Notify(vmmctl.NotifySpace, vmmctl.Space{AvailableBytes: available, ImageBytes: image, HeldRequests: held, AtNS: boottrace.Now()})
	}
}

func (c *control) handle(ctx context.Context, conn *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
	o := c.s.o
	c.hung()
	switch method {
	case vmmctl.MethodBudget:
		b, err := jsonrpc.Bind[vmmctl.Baseline](params)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		held := c.held
		c.held = 0
		c.mu.Unlock()
		c.s.logf("control: budget: baseline %d bytes available, image %d; %d held", b.AvailableBytes, b.ImageBytes, held)
		if held != 0 {
			jsonrpc.AfterReply(ctx, func() {
				c.s.logf("control: space: held writes released")
				conn.Notify(vmmctl.NotifySpace, vmmctl.Space{AvailableBytes: b.AvailableBytes, ImageBytes: b.ImageBytes, AtNS: boottrace.Now()})
			})
		}
		return vmmctl.BudgetResult{HeldRequests: held}, nil

	case vmmctl.MethodStats:
		c.mu.Lock()
		defer c.mu.Unlock()
		st := vmmctl.Stats{
			APIVersion: vmmctl.APIVersion, Build: version.Build, State: vmmctl.StateRunning, AtNS: boottrace.Now(),
			Pauses: c.pauses, TotalOffsetNS: uint64(c.total), Resets: c.resets, VCPUs: make([]vmmctl.VCPU, max(o.cpus, 1)),
			Memory: vmmctl.Memory{GuestBytes: uint64(o.mem) << 20},
		}
		if c.paused {
			st.State, st.PausedAtNS = vmmctl.StatePaused, c.pausedAtNS
		}
		if state, reason, _ := strings.Cut(o.set["window"], ":"); state != "" {
			w := &vmmctl.Window{State: state, Reason: reason, Faults: 3}
			if state == vmmctl.WindowOn {
				w.SizeBytes, w.GuestAddr, w.Reads = 256<<30, 1<<39, 41
			}
			st.Windows = &vmmctl.Windows{DataDisk: w}
		}
		return st, nil

	case vmmctl.MethodPause:
		c.order.Lock()
		defer c.order.Unlock()
		if o.set["pause-never"] != "" {
			c.s.logf("control: pause asked for; never answering, as told")
			select {}
		}
		if text := o.set["pause-error"]; text != "" {
			return nil, jsonrpc.Errorf(vmmctl.CodeFailed, "%s", text)
		}
		c.mu.Lock()
		if c.paused {
			at := c.pausedAtNS
			c.mu.Unlock()
			c.s.logf("control: pause asked for; already paused")
			return vmmctl.PauseResult{State: vmmctl.StatePaused, VCPUs: max(o.cpus, 1), PausedAtNS: at}, nil
		}
		c.mu.Unlock()
		began := time.Now()
		time.Sleep(o.ms("pause-ms", 0))
		c.mu.Lock()
		c.paused, c.pausedAt, c.pausedAtNS, c.resumed = true, time.Now(), boottrace.Now(), make(chan struct{})
		c.pauses++
		at := c.pausedAtNS
		c.mu.Unlock()
		c.s.logf("control: paused")
		return vmmctl.PauseResult{State: vmmctl.StatePaused, Changed: true, VCPUs: max(o.cpus, 1), TookNS: uint64(time.Since(began)), PausedAtNS: at}, nil

	case vmmctl.MethodResume:
		c.order.Lock()
		defer c.order.Unlock()
		if o.set["resume-never"] != "" {
			c.s.logf("control: resume asked for; never answering, as told")
			select {}
		}
		c.mu.Lock()
		if !c.paused {
			total := c.total
			c.mu.Unlock()
			c.s.logf("control: resume asked for; already running")
			return vmmctl.ResumeResult{State: vmmctl.StateRunning, VCPUs: max(o.cpus, 1), TotalOffsetNS: uint64(total)}, nil
		}
		c.mu.Unlock()
		if slow := o.ms("resume-ms", 0); slow > 0 {
			c.s.logf("control: resume asked for; taking %d ms over it, as told", slow.Milliseconds())
			time.Sleep(slow)
		}
		c.mu.Lock()
		began := time.Now()
		offset := began.Sub(c.pausedAt)
		c.total += offset
		total := c.total
		c.paused = false
		close(c.resumed)
		c.mu.Unlock()
		if c.s.agent != nil {
			c.s.agent.FallBehind(offset)
		}
		c.s.logf("control: resumed after %d ms", offset.Milliseconds())
		return vmmctl.ResumeResult{State: vmmctl.StateRunning, Changed: true, VCPUs: max(o.cpus, 1), TookNS: uint64(time.Since(began)), OffsetNS: uint64(offset), TotalOffsetNS: uint64(total)}, nil

	case vmmctl.MethodStop:
		c.order.Lock()
		jsonrpc.AfterReply(ctx, func() {
			c.s.logf("control: stop asked for; stopping")
			c.s.exit("stopped", 0)
		})
		return vmmctl.Empty{}, nil

	case vmmctl.MethodKill:
		jsonrpc.AfterReply(ctx, func() {
			syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		})
		return vmmctl.Empty{}, nil
	}
	return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method %q not found", method)
}
