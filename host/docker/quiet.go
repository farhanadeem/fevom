package docker

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"fev/host/daemon"
	"fev/host/docker/engine"
	"fev/proto/agent"
)

var ReclaimDelay = 2 * time.Second

var LongLived = time.Second

var (
	SweepEvery    = 30 * time.Second
	SweepMaxEvery = 5 * time.Minute
)

func ends(e engine.Event) bool {
	if e.Type == engine.TypeContainer && e.Action == "die" {
		return true
	}
	return frees(e)
}

type quiet struct {
	log   *slog.Logger
	call  func(ctx context.Context, vm daemon.VM, p agent.ReclaimParams) (*agent.Reclaimed, error)
	after func()

	mu     sync.Mutex
	vm     daemon.VM
	timer  *time.Timer
	arming uint64
	paused bool
	owed   bool

	sweep       *time.Timer
	sweepArming uint64
	sweepEvery  time.Duration
	sweeping    bool
}

func newQuiet() *quiet {
	return &quiet{call: func(ctx context.Context, vm daemon.VM, p agent.ReclaimParams) (*agent.Reclaimed, error) {
		a := vm.Agent()
		if a == nil {
			return nil, errNoAgent
		}
		return a.MemoryReclaim(ctx, p)
	}}
}

func (q *quiet) up(vm daemon.VM) {
	q.mu.Lock()
	q.vm = vm
	q.mu.Unlock()
}

func (q *quiet) down() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.timer != nil {
		q.timer.Stop()
		q.timer = nil
	}
	q.stopSweep()
	q.vm, q.owed, q.paused = nil, false, false
}

func (q *quiet) started() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.vm == nil {
		return
	}
	q.armSweep(SweepEvery)
}

func (q *quiet) armSweep(wait time.Duration) {
	if q.sweep != nil {
		q.sweep.Stop()
		q.sweep = nil
	}
	q.sweeping, q.sweepEvery = true, wait
	if q.paused {
		return
	}
	vm := q.vm
	q.sweepArming++
	arming := q.sweepArming
	q.sweep = time.AfterFunc(wait, func() { q.sweepNow(vm, arming) })
}

func (q *quiet) stopSweep() {
	if q.sweep != nil {
		q.sweep.Stop()
		q.sweep = nil
	}
	q.sweeping = false
}

func (q *quiet) sweepNow(vm daemon.VM, arming uint64) {
	q.mu.Lock()
	if q.sweepArming != arming || q.vm != vm || q.paused {
		q.mu.Unlock()
		return
	}
	q.sweep = nil
	every := q.sweepEvery
	q.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), guestCallTimeout)
	defer cancel()
	r, err := q.call(ctx, vm, agent.ReclaimParams{Running: true})
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.vm != vm || q.sweepArming != arming {
		return
	}
	switch {
	case err != nil && (agent.IsNotImplemented(err) || err == errNoAgent):
		q.stopSweep()
		return
	case err != nil:
		q.log.Warn("docker: the sweep of idle running containers failed; it is asked again later", "err", err)
		q.armSweep(min(2*every, SweepMaxEvery))
		return
	case r.Running == 0:
		q.stopSweep()
		return
	}
	if r.TrimmedBytes > 0 || r.ReclaimedBytes > 0 {
		q.log.Info("docker: the guest gave back cache of idle running containers", "trimmed_bytes", r.TrimmedBytes,
			"reclaimed_bytes", r.ReclaimedBytes, "running", r.Running, "cold_bytes", r.ColdBytes)
	}
	if r.TrimmedBytes > 0 || r.ColdBytes > agent.ReclaimSlackMiB<<20 {
		q.armSweep(SweepEvery)
	} else {
		q.armSweep(min(2*every, SweepMaxEvery))
	}
}

func (q *quiet) ended() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.vm == nil {
		return
	}
	if q.timer != nil {
		q.timer.Stop()
		q.timer = nil
	}
	if q.paused {
		q.owed = true
		return
	}
	q.arm()
	if q.sweeping {
		q.armSweep(SweepEvery)
	}
}

func (q *quiet) arm() {
	vm := q.vm
	q.arming++
	arming := q.arming
	q.timer = time.AfterFunc(ReclaimDelay, func() { q.now(vm, arming) })
}

func (q *quiet) pause(paused bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.paused = paused
	switch {
	case paused && q.timer != nil:
		q.timer.Stop()
		q.timer, q.owed = nil, true
	case !paused && q.owed && q.vm != nil:
		q.owed = false
		q.arm()
	}
	switch {
	case paused && q.sweep != nil:
		q.sweep.Stop()
		q.sweep = nil
	case !paused && q.sweeping && q.vm != nil:
		q.armSweep(q.sweepEvery)
	}
}

func (q *quiet) now(vm daemon.VM, arming uint64) {
	q.mu.Lock()
	current := q.vm == vm
	if q.arming == arming {
		q.timer = nil
	}
	q.mu.Unlock()
	if !current {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), guestCallTimeout)
	defer cancel()
	r, err := q.call(ctx, vm, agent.ReclaimParams{})
	if q.after != nil {
		q.after()
	}
	switch {
	case err != nil:
		q.mu.Lock()
		owed := q.paused && q.vm == vm
		if owed {
			q.owed = true
		}
		q.mu.Unlock()
		if owed {
			q.log.Info("docker: the memory reclaim did not finish before the VM was paused; it is made again at the wake", "err", err)
		} else if !agent.IsNotImplemented(err) && err != errNoAgent {
			q.log.Warn("docker: the memory reclaim after work ended failed; the cache stays until the next one", "err", err)
		}
	case r.Skipped != "":
		q.log.Debug("docker: nothing to reclaim after work ended", "why", r.Skipped, "file_bytes", r.FileBeforeBytes)
		q.sweepIfRunning(vm)
	default:
		q.log.Info("docker: the guest gave back file cache after work ended", "ms", math.Round(r.DurationMS),
			"reclaimed_bytes", r.ReclaimedBytes, "left_bytes", r.FileAfterBytes)
		q.sweepIfRunning(vm)
	}
}

func (q *quiet) sweepIfRunning(vm daemon.VM) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.vm == vm && !q.sweeping {
		q.armSweep(SweepEvery)
	}
}
