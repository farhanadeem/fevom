package daemon

import (
	"context"
	"time"

	"fev/host/power"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/vmmctl"
)

var asleepNow = power.Asleep

const NotRespondingWarning = "the VM is not responding: its VMM did not answer; `fev restart` starts it again"

func (s *Supervisor) Pause() api.PowerSleep {
	var out api.PowerSleep
	bound := time.NewTimer(s.timing.PauseBound)
	defer bound.Stop()
	late := func(b *boot, why string) api.PowerSleep {
		id := ""
		if b != nil {
			id = b.id
		}
		s.log.Warn("sleep: "+why+"; letting the Mac sleep anyway", "boot", id, "bound", s.timing.PauseBound)
		out.TimedOut = true
		return out
	}
	var b *boot
	var p *pauseCall
	var first bool
	for {
		s.mu.Lock()
		s.wakeBegun = false
		st := s.hub.Status()
		b = s.cur
		if w := s.waking; w != nil {
			s.mu.Unlock()
			select {
			case <-w:
				continue
			case <-bound.C:
				return late(b, "a wake of the VM was still under way")
			}
		}
		if st.State == api.StatePaused {
			s.mu.Unlock()
			out.Paused = true
			return out
		}
		if b == nil || b.ctl == nil || b.reason != "" || st.State != api.StateRunning {
			s.mu.Unlock()
			return out
		}
		if p = b.pause; p != nil && p.overtaken {
			s.mu.Unlock()
			select {
			case <-p.done:
				continue
			case <-bound.C:
				return late(b, "the VMM had not answered an earlier pause")
			}
		}
		if first = p == nil; first {
			p = &pauseCall{done: make(chan struct{})}
			b.pause = p
			go s.pauseVMM(b, p, b.agent)
		}
		s.mu.Unlock()
		break
	}

	select {
	case <-p.done:
	case <-bound.C:
		return late(b, "the VMM had not paused in time")
	}
	out.Paused = p.err == nil && s.hub.Status().State == api.StatePaused
	out.Changed = out.Paused && first
	if out.Changed {
		out.PauseMS = float64(p.took.Microseconds()) / 1000
	}
	return out
}

func (s *Supervisor) syncGuest(b *boot, client *agent.Client) {
	if client == nil {
		return
	}
	began := time.Now()
	ctx, cancel := context.WithTimeout(b.ctx, s.timing.SyncBound)
	r, err := client.StorageSync(ctx)
	cancel()
	switch {
	case err == nil:
		s.log.Debug("sync: the guest has written its data before the pause", "boot", b.id, "guest_ms", r.DurationMS, "ms", float64(time.Since(began).Microseconds())/1000)
	case agent.IsNotImplemented(err) || b.ctx.Err() != nil:
	default:
		s.log.Warn("sync: the guest had not written its data before the pause; it goes on when the guest runs again", "boot", b.id, "bound", s.timing.SyncBound, "err", err)
	}
}

func (b *boot) takeTurn(ctx context.Context) error {
	select {
	case b.ctlTurn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *boot) giveTurn() { <-b.ctlTurn }

const quietBound = 250 * time.Millisecond

func (s *Supervisor) pauseVMM(b *boot, p *pauseCall, client *agent.Client) {
	began := time.Now()
	for _, q := range s.quiet {
		q.Quiet()
	}
	landed := false
	defer func() {
		if !landed {
			for _, q := range s.quiet {
				q.Unquiet()
			}
		}
	}()
	s.syncGuest(b, client)
	if len(s.quiet) > 0 {
		deadline := time.Now().Add(min(quietBound, s.timing.SyncBound))
		for _, q := range s.quiet {
			if left := q.QuietWait(deadline); left > 0 {
				s.log.Debug("sleep: calls of the Mac's still out to the guest at the pause; the guest answers them after the resume", "boot", b.id, "out", left)
			}
		}
	}
	err := b.takeTurn(b.ctx)
	sent := false
	if err == nil {
		s.mu.Lock()
		sent = !p.overtaken
		s.mu.Unlock()
		if sent {
			_, err = b.ctl.Pause(b.ctx)
		}
		b.giveTurn()
	}
	s.mu.Lock()
	p.err, p.took = err, time.Since(began)
	if b.pause == p {
		b.pause = nil
	}
	switch {
	case err != nil:
		if b.ctx.Err() == nil {
			s.log.Warn("sleep: the VMM did not pause", "boot", b.id, "err", err)
		}
	case !sent || s.cur != b || p.overtaken || s.hub.Status().State != api.StateRunning:
	default:
		b.disarmEngine()
		s.hub.Update(func(st *api.Status) { st.State, st.PauseReason = api.StatePaused, api.PauseSleep })
		landed = true
		s.log.Debug("the VMM has paused the VM", "boot", b.id, "ms", float64(p.took.Microseconds())/1000)
	}
	s.mu.Unlock()
	close(p.done)
}

func (s *Supervisor) Wake(cause string, held func() int) (out api.PowerWake, ended bool) {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	out.Cause = cause
	announced := cause == "" || cause == api.WakeNotSleep
	s.mu.Lock()
	begun := s.wakeBegun
	if announced || cause == api.WakeCommand {
		s.wakeBegun = false
	}
	b := s.cur
	if b == nil {
		s.mu.Unlock()
		if announced {
			s.asleep.Store(int64(asleepNow()))
		}
		return out, false
	}
	st := s.hub.Status()
	pausedNow := b.ctl != nil && (st.State == api.StatePaused || b.pause != nil)
	if !announced && (!pausedNow || st.State == api.StateStopping || (cause == api.WakeUnfinished && !begun)) {
		s.mu.Unlock()
		return out, false
	}
	s.wakeBegun = false
	if b.pause != nil {
		b.pause.overtaken = true
	}
	client := b.agent
	waking := make(chan struct{})
	s.waking = waking
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.waking = nil
		s.mu.Unlock()
		close(waking)
	}()

	if pausedNow {
		ctx, cancel := context.WithTimeout(b.ctx, s.timing.ResumeBound)
		var r *vmmctl.ResumeResult
		err := b.takeTurn(ctx)
		if err == nil {
			r, err = b.ctl.Resume(ctx)
			b.giveTurn()
		}
		cancel()
		if err != nil {
			s.mu.Lock()
			if s.cur == b && b.ctx.Err() == nil {
				b.fail("the VMM did not resume after the Mac's sleep: " + err.Error())
			}
			s.mu.Unlock()
			return out, false
		}
		out.Resumed = r.Changed
		if r.Changed {
			out.PausedMS = float64(r.OffsetNS) / 1e6
			s.log.Debug("the VMM has resumed the VM", "boot", b.id, "paused_ms", int64(out.PausedMS), "cause", cause)
		}
	}
	if client != nil && st.State != api.StateStopping {
		out.ClockStepped, out.ClockOffsetMS = b.setClock(client, "wake")
		if !out.ClockStepped {
			out.ClockOffsetMS = 0
		}
	}
	s.mu.Lock()
	if s.cur == b && s.hub.Status().State == api.StatePaused {
		if held != nil {
			out.Released = held()
		}
		if e := s.hub.Status().Engine; e != api.EngineReady && e != api.EngineFailed && e != api.EngineOff {
			b.armEngine()
		}
		s.hub.Update(func(st *api.Status) { st.State = api.StateRunning })
		ended = true
	}
	s.mu.Unlock()
	return out, ended || out.Resumed
}

func (s *Supervisor) WakeBegun() (paused, inHand bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wakeBegun = true
	if s.cur == nil {
		return false, false
	}
	return s.hub.Status().State == api.StatePaused, s.cur.pause != nil && !s.cur.pause.overtaken
}

func (s *Supervisor) CatchUp() {
	asleep := int64(asleepNow())
	last := s.asleep.Load()
	if time.Duration(asleep-last) < clockSlack || !s.asleep.CompareAndSwap(last, asleep) {
		return
	}
	s.mu.Lock()
	b, state := s.cur, s.hub.Status().State
	var client *agent.Client
	if b != nil {
		client = b.agent
	}
	s.mu.Unlock()
	if client == nil || (state != api.StateRunning && state != api.StateStarting) {
		return
	}
	s.log.Info("clock: the Mac slept with no notification; setting the guest's clock", "boot", b.id, "asleep_ms", (asleep-last)/1e6)
	go b.setClock(client, "unannounced sleep")
}

const clockSlack = 100 * time.Millisecond

func (s *Supervisor) CheckAlive() {
	s.mu.Lock()
	b, st := s.cur, s.hub.Status()
	if b == nil || b.ctl == nil || b.reason != "" || (st.State != api.StateRunning && st.State != api.StatePaused) {
		s.mu.Unlock()
		return
	}
	if b.alive != nil {
		wait := b.alive
		s.mu.Unlock()
		<-wait
		return
	}
	done := make(chan struct{})
	b.alive = done
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(b.ctx, s.timing.AliveBound)
	_, err := b.ctl.Stats(ctx)
	cancel()

	s.mu.Lock()
	b.alive = nil
	if s.cur == b && b.ctx.Err() == nil {
		was := false
		for _, w := range s.hub.Status().Warnings {
			was = was || w == NotRespondingWarning
		}
		switch {
		case err != nil && !was:
			s.log.Error("the VMM did not answer on its control socket; the VM is not responding", "boot", b.id, "bound", s.timing.AliveBound, "err", err)
			s.hub.Update(func(st *api.Status) {
				st.Warnings = append(append([]string{}, st.Warnings...), NotRespondingWarning)
			})
		case err == nil && was:
			s.log.Info("the VMM answers again", "boot", b.id)
			s.hub.Update(func(st *api.Status) {
				var keep []string
				for _, w := range st.Warnings {
					if w != NotRespondingWarning {
						keep = append(keep, w)
					}
				}
				st.Warnings = keep
			})
		}
	}
	s.mu.Unlock()
	close(done)
}
