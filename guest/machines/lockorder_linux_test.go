package machines

import (
	"testing"
	"time"
)

func TestTheAgentPathDoesNotWaitOnAStartingMachine(t *testing.T) {
	rt := &Runtime{byName: map[string]*machine{}}
	m := &machine{rt: rt, name: "m"}
	rt.byName["m"] = m

	held := make(chan struct{})
	startDone := make(chan struct{})
	go func() {
		m.mu.Lock()
		close(held)
		time.Sleep(100 * time.Millisecond)
		rt.mu.Lock()
		rt.mu.Unlock()
		m.mu.Unlock()
		close(startDone)
	}()
	<-held
	setDone := make(chan struct{})
	go func() {
		rt.SetAgentPath("/tmp/agent.sock")
		close(setDone)
	}()
	for _, c := range []chan struct{}{startDone, setDone} {
		select {
		case <-c:
		case <-time.After(5 * time.Second):
			t.Fatal("deadlock: SetAgentPath and a starting machine wait on each other")
		}
	}
}

func TestRunningDoesNotWaitOnABusyMachine(t *testing.T) {
	rt := &Runtime{byName: map[string]*machine{}}
	m := &machine{rt: rt, name: "m"}
	rt.byName["m"] = m
	m.mu.Lock()
	defer m.mu.Unlock()
	done := make(chan []RunningMachine)
	go func() { done <- rt.Running() }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Fatalf("a busy machine listed: %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Running waited on a machine whose lock is held")
	}
}
