package supervise

import (
	"errors"
	"fmt"
	"sync"
	"time"

	api "fev/proto/agent"
)

type Run struct {
	PID    int
	Ready  <-chan error
	Exited <-chan struct{}
	How    func() string
}

type Hooks struct {
	Start func() (*Run, error)
	Stop  func(run *Run, grace time.Duration)
	NowMS func() float64
	After func(d time.Duration, f func()) (cancel func())
	Logf  func(format string, args ...any)
}

const (
	FirstDelay    = 100 * time.Millisecond
	MaxDelay      = 5 * time.Second
	HealthyAfter  = 10 * time.Second
	MaxFailures   = 5
	notReadyGrace = 2 * time.Second
)

var ErrClosed = errors.New("the guest is shutting down")

type Supervisor struct {
	h Hooks

	op sync.Mutex

	mu       sync.Mutex
	st       api.EngineStatus
	run      *Run
	stopping bool
	failures int
	delay    time.Duration
	retry    func()
	retrySeq uint64
	closed   bool
	absent   string
	subs     map[uint64]func(api.EngineStatus)
	nextSub  uint64
}

func New(h Hooks) *Supervisor {
	return &Supervisor{h: h, st: api.EngineStatus{State: api.EngineStopped}, delay: FirstDelay, subs: map[uint64]func(api.EngineStatus){}}
}

func (s *Supervisor) set(change func(st *api.EngineStatus)) {
	change(&s.st)
	for _, emit := range s.subs {
		emit(s.st)
	}
}

func (s *Supervisor) Status() api.EngineStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

func (s *Supervisor) Subscribe(emit func(api.EngineStatus)) (stop func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSub++
	id := s.nextSub
	s.subs[id] = emit
	emit(s.st)
	return func() {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
}

func (s *Supervisor) Absent(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.absent = reason
	s.set(func(st *api.EngineStatus) { st.State, st.Error = api.EngineFailed, reason })
}

func (s *Supervisor) Start() (api.EngineStatus, error) {
	s.op.Lock()
	defer s.op.Unlock()
	if err := s.startable(); err != nil {
		return s.Status(), err
	}
	s.mu.Lock()
	running := s.run != nil
	s.mu.Unlock()
	if running {
		return s.Status(), nil
	}
	s.fresh()
	s.start()
	return s.Status(), nil
}

func (s *Supervisor) startable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return ErrClosed
	case s.absent != "":
		return errors.New(s.absent)
	}
	return nil
}

func (s *Supervisor) fresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retry != nil {
		s.retry()
		s.retry = nil
	}
	s.failures, s.delay = 0, FirstDelay
}

func (s *Supervisor) start() {
	s.mu.Lock()
	s.st.State, s.st.PID, s.st.ReadyMS, s.st.Error = api.EngineStarting, 0, 0, ""
	s.st.StartedMS = s.h.NowMS()
	s.mu.Unlock()

	run, err := s.h.Start()
	if err != nil {
		s.h.Logf("engine: could not start: %v", err)
		s.mu.Lock()
		s.failed("could not start: " + err.Error())
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.run, s.stopping = run, false
	s.set(func(st *api.EngineStatus) { st.PID = run.PID; st.Starts++ })
	s.mu.Unlock()
	go s.watch(run)
}

func (s *Supervisor) watch(run *Run) {
	select {
	case err := <-run.Ready:
		if err != nil {
			s.h.Logf("engine: not ready: %v", err)
			s.op.Lock()
			s.mu.Lock()
			current := s.run == run && !s.stopping
			s.mu.Unlock()
			if current {
				s.h.Stop(run, notReadyGrace)
			}
			s.op.Unlock()
			break
		}
		s.mu.Lock()
		if s.run == run && !s.stopping {
			s.set(func(st *api.EngineStatus) { st.State, st.ReadyMS = api.EngineReady, s.h.NowMS() })
		}
		s.mu.Unlock()
	case <-run.Exited:
	}
	<-run.Exited

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run != run || s.stopping {
		return
	}
	s.run = nil
	how := run.How()
	wasReady := s.st.ReadyMS
	if wasReady > 0 && s.h.NowMS()-wasReady >= float64(HealthyAfter.Milliseconds()) {
		s.failures, s.delay = 0, FirstDelay
	}
	s.h.Logf("engine: exited unexpectedly (%s)", how)
	s.st.LastExit = how
	s.failed(how)
}

func (s *Supervisor) failed(why string) {
	s.failures++
	if s.closed || s.failures >= MaxFailures {
		s.set(func(st *api.EngineStatus) {
			st.State, st.PID = api.EngineFailed, 0
			st.Error = fmt.Sprintf("failed %d times in a row; last: %s", s.failures, why)
		})
		return
	}
	delay := s.delay
	s.delay = min(s.delay*2, MaxDelay)
	s.retrySeq++
	seq := s.retrySeq
	s.retry = s.h.After(delay, func() {
		s.op.Lock()
		defer s.op.Unlock()
		s.mu.Lock()
		mine := s.retry != nil && s.retrySeq == seq
		if mine {
			s.retry = nil
		}
		due := mine && s.run == nil && !s.closed && s.st.State == api.EngineStarting
		s.mu.Unlock()
		if due {
			s.start()
		}
	})
	s.set(func(st *api.EngineStatus) { st.State, st.PID, st.ReadyMS = api.EngineStarting, 0, 0 })
}

func (s *Supervisor) stop(grace time.Duration) {
	s.mu.Lock()
	if s.retry != nil {
		s.retry()
		s.retry = nil
	}
	run := s.run
	if run == nil {
		if s.absent == "" {
			s.set(func(st *api.EngineStatus) { st.State, st.PID, st.ReadyMS, st.Error = api.EngineStopped, 0, 0, "" })
		}
		s.mu.Unlock()
		return
	}
	s.stopping = true
	s.set(func(st *api.EngineStatus) { st.State = api.EngineStopping })
	s.mu.Unlock()

	s.h.Stop(run, grace)
	<-run.Exited

	s.mu.Lock()
	s.run, s.stopping = nil, false
	s.set(func(st *api.EngineStatus) {
		st.State, st.PID, st.ReadyMS, st.Error = api.EngineStopped, 0, 0, ""
		st.LastExit = run.How()
	})
	s.mu.Unlock()
}

func (s *Supervisor) Stop(grace time.Duration) (api.EngineStatus, error) {
	s.op.Lock()
	defer s.op.Unlock()
	s.stop(grace)
	return s.Status(), nil
}

func (s *Supervisor) Restart(grace time.Duration) (api.EngineStatus, error) {
	s.op.Lock()
	defer s.op.Unlock()
	if err := s.startable(); err != nil {
		return s.Status(), err
	}
	s.stop(grace)
	s.fresh()
	s.start()
	return s.Status(), nil
}

func (s *Supervisor) Close(grace time.Duration) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.op.Lock()
	defer s.op.Unlock()
	s.stop(grace)
}
