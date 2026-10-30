package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	"fev/host/daemon"
	"fev/host/docker/engine"
	"fev/proto/agent"
	api "fev/proto/daemon"
)

func TestATrimInFlightWhenTheVMIsPausedIsMadeAgainAfterTheWake(t *testing.T) {
	oldDelay, oldBound := TrimDelay, guestCallTimeout
	TrimDelay, guestCallTimeout = 50*time.Millisecond, 200*time.Millisecond
	defer func() { TrimDelay, guestCallTimeout = oldDelay, oldBound }()
	w := newWorld(t, Options{Autostart: true})
	entered := make(chan struct{}, 16)
	parked := make(chan struct{})
	w.m.trimCall = func(ctx context.Context, _ daemon.VM) (*agent.TrimResult, error) {
		entered <- struct{}{}
		select {
		case <-parked:
			return &agent.TrimResult{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	seen := make(chan engine.Event, 16)
	w.m.Subscribe(func(e engine.Event) { seen <- e })
	w.boot()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(strings.Join(w.engine.Stats().Paths, " "), "GET /containers/json") {
		if time.Now().After(deadline) {
			t.Fatal("the feed never listed the containers")
		}
		time.Sleep(time.Millisecond)
	}
	w.engine.Emit("image", "delete", "x", nil)
	<-seen
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no trim after the removal")
	}
	w.to(api.StatePaused, api.EngineReady, "")
	time.Sleep(2 * guestCallTimeout)
	select {
	case <-entered:
		t.Fatal("the guest was asked to trim again while paused")
	default:
	}
	close(parked)
	w.to(api.StateRunning, api.EngineReady, "")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the trim that timed out against the parked guest was never made again")
	}
	select {
	case <-entered:
		t.Fatal("a third trim")
	case <-time.After(4 * TrimDelay):
	}
}

func TestATrimThatFailsWithTheVMRunningIsNotRepeated(t *testing.T) {
	oldDelay, oldBound := TrimDelay, guestCallTimeout
	TrimDelay, guestCallTimeout = 30*time.Millisecond, 60*time.Millisecond
	defer func() { TrimDelay, guestCallTimeout = oldDelay, oldBound }()
	w := newWorld(t, Options{Autostart: true})
	entered := make(chan struct{}, 16)
	w.m.trimCall = func(ctx context.Context, _ daemon.VM) (*agent.TrimResult, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	seen := make(chan engine.Event, 16)
	w.m.Subscribe(func(e engine.Event) { seen <- e })
	w.boot()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(strings.Join(w.engine.Stats().Paths, " "), "GET /containers/json") {
		if time.Now().After(deadline) {
			t.Fatal("the feed never listed the containers")
		}
		time.Sleep(time.Millisecond)
	}
	w.engine.Emit("image", "delete", "x", nil)
	<-seen
	<-entered
	select {
	case <-entered:
		t.Fatal("a failed trim was made again with no pause and no removal")
	case <-time.After(10 * guestCallTimeout):
	}
}

func TestAWakeOwesNoTrimWhenTheLastOneWasMade(t *testing.T) {
	old := TrimDelay
	TrimDelay = 30 * time.Millisecond
	defer func() { TrimDelay = old }()
	w := newWorld(t, Options{Autostart: true})
	trims := make(chan struct{}, 16)
	w.m.trimCall = func(context.Context, daemon.VM) (*agent.TrimResult, error) {
		trims <- struct{}{}
		return &agent.TrimResult{}, nil
	}
	seen := make(chan engine.Event, 16)
	w.m.Subscribe(func(e engine.Event) { seen <- e })
	w.boot()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(strings.Join(w.engine.Stats().Paths, " "), "GET /containers/json") {
		if time.Now().After(deadline) {
			t.Fatal("the feed never listed the containers")
		}
		time.Sleep(time.Millisecond)
	}
	w.engine.Emit("image", "delete", "x", nil)
	<-seen
	select {
	case <-trims:
	case <-time.After(5 * time.Second):
		t.Fatal("no trim after the removal")
	}
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 2; i++ {
		w.to(api.StatePaused, api.EngineReady, "")
		w.to(api.StateRunning, api.EngineReady, "")
		select {
		case <-trims:
			t.Fatalf("a trim after wake %d with nothing removed since the last one", i+1)
		case <-time.After(6 * TrimDelay):
		}
	}
}

func TestARestartOfTheEngineResumesAPausedVMAndAHeldConnectionOnlyNudges(t *testing.T) {
	w := newWorld(t, Options{Autostart: true})
	w.boot()
	w.to(api.StatePaused, api.EngineReady, "")
	counts := func() (rouses, nudges int) {
		w.fakeHost.mu.Lock()
		defer w.fakeHost.mu.Unlock()
		return w.fakeHost.rouses, w.fakeHost.nudges
	}
	if r, n := counts(); r != 0 || n != 0 {
		t.Fatalf("the pause itself: %d rouses, %d nudges", r, n)
	}
	w.later("/_ping")
	w.held(1)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if r, n := counts(); r == 0 && n == 1 {
			break
		} else if r != 0 || n > 1 || time.Now().After(deadline) {
			t.Fatalf("one held connection: %d rouses, %d nudges; want a nudge and no more", r, n)
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.fakeHost.methods[api.MethodDockerRestart](ctx, nil)
	if r, _ := counts(); r != 1 {
		t.Fatalf("docker.restart on a paused VM asked for the VM %d times, want 1", r)
	}
	w.to(api.StateRunning, api.EngineReady, "")
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	w.fakeHost.methods[api.MethodDockerRestart](ctx2, nil)
	if r, _ := counts(); r != 1 {
		t.Errorf("docker.restart on a running VM asked for a resume (%d)", r)
	}
}
