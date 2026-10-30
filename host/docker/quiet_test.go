package docker

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"fev/host/daemon"
	"fev/host/docker/engine"
	"fev/proto/agent"
	api "fev/proto/daemon"
)

type quietWorld struct {
	*world
	reclaims    chan time.Time
	seen        chan engine.Event
	sweeps      chan time.Time
	sweepAnswer chan *agent.Reclaimed
}

func newQuietWorld(t *testing.T) *quietWorld {
	t.Helper()
	oldDelay, oldLong := ReclaimDelay, LongLived
	ReclaimDelay, LongLived = 150*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { ReclaimDelay, LongLived = oldDelay, oldLong })
	q := &quietWorld{world: newWorld(t, Options{Autostart: true}), reclaims: make(chan time.Time, 16), seen: make(chan engine.Event, 64),
		sweeps: make(chan time.Time, 64), sweepAnswer: make(chan *agent.Reclaimed, 64)}
	q.m.quiet.call = func(_ context.Context, _ daemon.VM, p agent.ReclaimParams) (*agent.Reclaimed, error) {
		if p.Running {
			q.sweeps <- time.Now()
			select {
			case r := <-q.sweepAnswer:
				return r, nil
			default:
				return &agent.Reclaimed{}, nil
			}
		}
		q.reclaims <- time.Now()
		return &agent.Reclaimed{ReclaimedBytes: 1 << 30, FloorMiB: agent.DefaultReclaimFloorMiB}, nil
	}
	q.m.trimCall = func(context.Context, daemon.VM) (*agent.TrimResult, error) { return &agent.TrimResult{}, nil }
	q.m.Subscribe(func(e engine.Event) { q.seen <- e })
	q.boot()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(strings.Join(q.engine.Stats().Paths, " "), "GET /containers/json") {
		if time.Now().After(deadline) {
			t.Fatal("the feed never listed the containers")
		}
		time.Sleep(time.Millisecond)
	}
	return q
}

func (q *quietWorld) emit(kind, action string) {
	q.t.Helper()
	q.engine.Emit(kind, action, "x", nil)
	select {
	case <-q.seen:
	case <-time.After(5 * time.Second):
		q.t.Fatalf("%s %s never arrived", kind, action)
	}
}

func (q *quietWorld) none(why string) {
	q.t.Helper()
	select {
	case <-q.reclaims:
		q.t.Fatalf("a reclaim %s", why)
	case <-time.After(3 * ReclaimDelay):
	}
}

func (q *quietWorld) one(since time.Time, why string) {
	q.t.Helper()
	select {
	case at := <-q.reclaims:
		if at.Sub(since) < ReclaimDelay*3/5 {
			q.t.Errorf("the reclaim %s came %v after it, before the delay of %v", why, at.Sub(since), ReclaimDelay)
		}
	case <-time.After(5 * time.Second):
		q.t.Fatalf("no reclaim %s", why)
	}
}

func TestWorkEndingIsFollowedByOneReclaim(t *testing.T) {
	q := newQuietWorld(t)
	for _, e := range [][2]string{{"container", "start"}, {"container", "exec_die"}, {"container", "health_status: healthy"}, {"image", "pull"}, {"network", "destroy"}, {"volume", "create"}} {
		q.emit(e[0], e[1])
	}
	q.none("with no work ended")
	var last time.Time
	for _, e := range [][2]string{{"container", "die"}, {"container", "destroy"}, {"container", "die"}, {"volume", "destroy"}, {"image", "delete"}} {
		q.emit(e[0], e[1])
		last = time.Now()
		time.Sleep(ReclaimDelay / 5)
	}
	q.one(last, "after the burst")
	q.none("a second time for one burst")
	q.emit("container", "die")
	q.to(api.StateStopping, api.EngineStopping, "")
	q.none("after the engine had gone")
}

func TestWorkOutsideTheEngineEndingIsFollowedByOneReclaim(t *testing.T) {
	q := newQuietWorld(t)
	q.none("with no work ended")
	q.m.WorkEnded()
	q.one(time.Now(), "after a debug session ended")
	q.none("a second time for one session")
}

func (q *quietWorld) connect() net.Conn {
	q.t.Helper()
	before := q.engine.Stats().Connections
	c, err := net.Dial("unix", q.m.Socket())
	if err != nil {
		q.t.Fatal(err)
	}
	q.t.Cleanup(func() { c.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for q.engine.Stats().Connections == before {
		if time.Now().After(deadline) {
			q.t.Fatal("the connection never reached the engine")
		}
		time.Sleep(time.Millisecond)
	}
	return c
}

func TestALongConnectionEndingIsWorkEndingAndAQuestionIsNot(t *testing.T) {
	q := newQuietWorld(t)
	q.connect()
	for i := 0; i < 8; i++ {
		if got := q.get("/_ping"); got.status != 200 {
			t.Fatalf("ping: %+v", got)
		}
		time.Sleep(ReclaimDelay / 4)
	}
	q.none("for questions")

	long := q.connect()
	time.Sleep(LongLived + LongLived/2)
	long.Close()
	q.one(time.Now().Add(-ReclaimDelay/5), "after a long connection ended")
	q.none("a second time")

	stop := make(chan struct{})
	asked := make(chan struct{})
	go func() {
		defer close(asked)
		for {
			select {
			case <-stop:
				return
			default:
				q.get("/_ping")
				time.Sleep(ReclaimDelay / 6)
			}
		}
	}()
	q.emit("container", "die")
	died := time.Now()
	select {
	case at := <-q.reclaims:
		if at.Sub(died) > 3*ReclaimDelay {
			t.Errorf("the reclaim came %v after the container died, held off by a client that only asks", at.Sub(died))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a client that asks every little while held the reclaim off for ever")
	}
	close(stop)
	<-asked
}

func TestAReclaimOwedWhenTheVMIsPausedWaitsForTheWake(t *testing.T) {
	q := newQuietWorld(t)
	q.emit("container", "die")
	q.to(api.StatePaused, api.EngineReady, "")
	q.none("while paused")
	woke := time.Now()
	q.to(api.StateRunning, api.EngineReady, "")
	q.one(woke, "after the wake")
	q.none("a second time after the wake")
	q.to(api.StatePaused, api.EngineReady, "")
	q.to(api.StateRunning, api.EngineReady, "")
	q.none("at a wake that owed none")
}

func TestTheSweepRunsWhileContainersRun(t *testing.T) {
	oldEvery, oldMax := SweepEvery, SweepMaxEvery
	SweepEvery, SweepMaxEvery = 100*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { SweepEvery, SweepMaxEvery = oldEvery, oldMax })
	q := newQuietWorld(t)
	gap := func(why string, want time.Duration, since time.Time) time.Time {
		t.Helper()
		select {
		case at := <-q.sweeps:
			if d := at.Sub(since); d < want*3/5 || d > want*3 {
				t.Errorf("the sweep %s came after %v; want about %v", why, d, want)
			}
			return at
		case <-time.After(5 * time.Second):
			t.Fatalf("no sweep %s", why)
		}
		return time.Time{}
	}
	none := func(why string, wait time.Duration) {
		t.Helper()
		select {
		case <-q.sweeps:
			t.Fatalf("a sweep %s", why)
		case <-time.After(wait):
		}
	}
	none("before any container started", 3*SweepEvery)
	q.emit("container", "start")
	start := time.Now()
	q.sweepAnswer <- &agent.Reclaimed{Running: 1, TrimmedBytes: 300 << 20}
	at := gap("after the start", SweepEvery, start)
	for _, want := range []time.Duration{SweepEvery, 2 * SweepEvery, 4 * SweepEvery, 4 * SweepEvery} {
		q.sweepAnswer <- &agent.Reclaimed{Running: 1}
		at = gap("while one runs", want, at)
	}
	q.to(api.StatePaused, api.EngineReady, "")
	none("while paused", 3*SweepMaxEvery)
	woke := time.Now()
	q.sweepAnswer <- &agent.Reclaimed{Running: 0}
	q.to(api.StateRunning, api.EngineReady, "")
	gap("after the wake", SweepMaxEvery, woke)
	none("once none ran", 3*SweepMaxEvery)
}
