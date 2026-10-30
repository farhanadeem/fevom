package machines

import (
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/internal/streams"
)

func (rt *Runtime) Dial(name string, target netip.AddrPort, timeout time.Duration) (net.Conn, error) {
	if err := rt.available(); err != nil {
		return nil, err
	}
	rt.mu.Lock()
	m := rt.byName[name]
	rt.mu.Unlock()
	if m == nil {
		return nil, fmt.Errorf("%s: %w", name, streams.ErrNoMachine)
	}
	m.mu.Lock()
	r, state := m.cur, m.state
	m.mu.Unlock()
	if r == nil || state != api.MachineRunning {
		return nil, fmt.Errorf("%s is %s: %w", name, state, streams.ErrNoMachine)
	}
	b := r.boot()
	if b == nil {
		return nil, fmt.Errorf("%s is between boots: %w", name, streams.ErrNoMachine)
	}
	b.nsMu.RLock()
	if b.ns == nil {
		b.nsMu.RUnlock()
		return nil, fmt.Errorf("%s has stopped: %w", name, streams.ErrNoMachine)
	}
	netns, err := unix.FcntlInt(b.ns["net"].Fd(), unix.F_DUPFD_CLOEXEC, 0)
	b.nsMu.RUnlock()
	if err != nil {
		return nil, err
	}
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		defer unix.Close(netns)
		runtime.LockOSThread()
		if err := unix.Setns(netns, unix.CLONE_NEWNET); err != nil {
			done <- result{err: fmt.Errorf("enter %s's network namespace: %w", name, err)}
			return
		}
		d := net.Dialer{Timeout: timeout}
		c, err := d.Dial("tcp", target.String())
		done <- result{c, err}
	}()
	res := <-done
	return res.conn, res.err
}
