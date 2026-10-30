package daemon

import (
	"strings"
	"testing"
	"time"

	api "fev/proto/daemon"
)

func TestADebugResetEndsTheOpenSessionsWhenForced(t *testing.T) {
	f := machinesDaemon(t)
	c := f.client()
	exits := make(chan api.SessionExit, 2)
	for _, target := range []string{"web", "db"} {
		if _, err := c.DebugStart(bg, api.DebugStartParams{Target: target, Argv: []string{"sleep", "30000"}}, func(e api.SessionExit) { exits <- e }); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.m.sessions.openDebug(); n != 2 {
		t.Fatalf("%d debug sessions open", n)
	}
	if _, err := c.DebugReset(bg, false); err == nil || !strings.Contains(err.Error(), "open") {
		t.Fatalf("a reset with sessions open: %v", err)
	}
	if _, err := c.DebugReset(bg, true); err != nil {
		t.Fatalf("a forced reset: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case e := <-exits:
			if !strings.Contains(e.Error, "reset") {
				t.Errorf("a session ended by the reset says %+v", e)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a forced reset left a session open")
		}
	}
	if n := f.m.sessions.openDebug(); n != 0 {
		t.Fatalf("%d debug sessions open after the reset", n)
	}
}

func TestTheDebugLineCountsSessionsDown(t *testing.T) {
	f := machinesDaemon(t)
	c := f.client()
	exits := make(chan api.SessionExit, 1)
	started, err := c.DebugStart(bg, api.DebugStartParams{Target: "web", Argv: []string{"sleep", "30000"}}, func(e api.SessionExit) { exits <- e })
	if err != nil {
		t.Fatal(err)
	}
	line := func() *api.DebugLine {
		st, err := f.c.Status(bg)
		if err != nil {
			t.Fatal(err)
		}
		return st.Debug
	}
	if l := line(); l == nil || l.Sessions != 1 {
		t.Fatalf("the line with a session open: %+v", l)
	}
	if err := c.SessionClose(bg, started.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		l := line()
		if l != nil && l.Sessions == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the line after the session closed: %+v", l)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
