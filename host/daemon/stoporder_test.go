package daemon

import (
	"os"
	"testing"
	"time"

	"fev/host/daemon/boottrace"
	api "fev/proto/daemon"
)

func stopBegins(t *testing.T, f *fixture) uint64 {
	t.Helper()
	began := boottrace.Now()
	if err := os.WriteFile(api.StopMarkPath(f.layout.Home), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return began
}

func login(t *testing.T, f *fixture) {
	t.Helper()
	if err := (moduleHost{f.d}).StartAsked(); err != nil {
		t.Fatalf("the login's start: %v", err)
	}
}

func endsRunning(t *testing.T, f *fixture) {
	t.Helper()
	if st := f.running(); st.StopRequested {
		t.Errorf("running with stop_requested: %+v", st)
	}
	if _, err := os.Stat(api.StopMarkPath(f.layout.Home)); err == nil {
		t.Error("the stop mark is there although the last request was a start")
	}
	select {
	case <-f.d.Done():
		t.Fatal("the daemon shut down")
	default:
	}
}

func endsStopped(t *testing.T, f *fixture, r *api.ShutdownResult, err error) {
	t.Helper()
	if err != nil || r.Superseded {
		t.Fatalf("the shutdown of a stop that stands: %+v %v", r, err)
	}
	<-f.d.Done()
	if _, err := os.Stat(api.StopMarkPath(f.layout.Home)); err != nil {
		t.Errorf("no stop mark after the stop: %v", err)
	}
	if st := f.d.hub.Status(); st.State != api.StateStopped || !st.StopRequested {
		t.Errorf("the last status: %+v", st)
	}
}

func TestAStartThatComesAfterAStopBeganStands(t *testing.T) {
	slow := []string{"stop-ms=400"}
	for _, at := range []string{"before", "during", "between"} {
		t.Run(at, func(t *testing.T) {
			f := started(t, slow, slow, slow)
			c := f.client()
			if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
				t.Fatal(err)
			}
			began := stopBegins(t, f)
			stop := func() *api.Status {
				t.Helper()
				st, err := c.VMStop(bg, api.StopParams{StopBeganNS: began})
				if err != nil {
					t.Fatal(err)
				}
				return st
			}
			switch at {
			case "before":
				login(t, f)
				if st := stop(); st.State != api.StateRunning {
					t.Errorf("vm.stop after a login's start stopped the VM: %+v", st)
				}
			case "during":
				done := make(chan *api.Status, 1)
				go func() {
					st, _ := c.VMStop(bg, api.StopParams{StopBeganNS: began})
					done <- st
				}()
				f.until("stopping", func(st api.Status) bool { return st.State == api.StateStopping })
				login(t, f)
				<-done
			case "between":
				if st := stop(); st.State != api.StateStopped {
					t.Fatalf("vm.stop: %+v", st)
				}
				login(t, f)
			}
			r, err := c.Shutdown(bg, api.ShutdownParams{StopRequested: true, StopBeganNS: began})
			if err != nil {
				t.Fatal(err)
			}
			if !r.Superseded {
				t.Fatalf("the stop's shutdown was made although a login's start came after the stop began")
			}
			endsRunning(t, f)
		})
	}

	t.Run("alone", func(t *testing.T) {
		f := started(t, slow)
		c := f.client()
		if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
			t.Fatal(err)
		}
		began := stopBegins(t, f)
		if st, err := c.VMStop(bg, api.StopParams{StopBeganNS: began}); err != nil || st.State != api.StateStopped || !st.StopRequested {
			t.Fatalf("vm.stop: %+v %v", st, err)
		}
		r, err := c.Shutdown(bg, api.ShutdownParams{StopRequested: true, StopBeganNS: began})
		if err == nil && !r.Superseded {
			if err := (moduleHost{f.d}).StartAsked(); err != ErrShuttingDown {
				t.Errorf("a start after the shutdown was taken: %v", err)
			}
		}
		endsStopped(t, f, r, err)
	})

	for _, at := range []string{"before", "during", "between"} {
		t.Run("a command that starts nothing "+at, func(t *testing.T) {
			f := started(t, slow)
			c := f.client()
			if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
				t.Fatal(err)
			}
			began := stopBegins(t, f)
			list := func() { os.Remove(api.StopMarkPath(f.layout.Home)) }
			switch at {
			case "before":
				list()
				if st, err := c.VMStop(bg, api.StopParams{StopBeganNS: began}); err != nil || st.State != api.StateStopped {
					t.Fatalf("vm.stop after a command that starts nothing: %+v %v", st, err)
				}
			case "during":
				done := make(chan struct{})
				go func() {
					c.VMStop(bg, api.StopParams{StopBeganNS: began})
					close(done)
				}()
				f.until("stopping", func(st api.Status) bool { return st.State == api.StateStopping })
				list()
				<-done
			case "between":
				if st, err := c.VMStop(bg, api.StopParams{StopBeganNS: began}); err != nil || st.State != api.StateStopped {
					t.Fatalf("vm.stop: %+v %v", st, err)
				}
				list()
			}
			r, err := c.Shutdown(bg, api.ShutdownParams{StopRequested: true, StopBeganNS: began})
			endsStopped(t, f, r, err)
		})
	}

	t.Run("two stops then a start", func(t *testing.T) {
		f := started(t, slow, slow, slow)
		c, c2 := f.client(), f.client()
		if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
			t.Fatal(err)
		}
		first := stopBegins(t, f)
		second := stopBegins(t, f)
		done := make(chan struct{})
		go func() {
			c.VMStop(bg, api.StopParams{StopBeganNS: first})
			close(done)
		}()
		f.until("stopping", func(st api.Status) bool { return st.State == api.StateStopping })
		c2.VMStop(bg, api.StopParams{StopBeganNS: second})
		<-done
		login(t, f)
		for i, began := range []uint64{first, second} {
			r, err := []*api.Client{c, c2}[i].Shutdown(bg, api.ShutdownParams{StopRequested: true, StopBeganNS: began})
			if err != nil || !r.Superseded {
				t.Fatalf("stop %d's shutdown, a start having come after both: %+v %v", i+1, r, err)
			}
		}
		endsRunning(t, f)
	})

	t.Run("a stop, a start, a stop", func(t *testing.T) {
		f := started(t, slow, slow, slow)
		c, c2 := f.client(), f.client()
		if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
			t.Fatal(err)
		}
		first := stopBegins(t, f)
		if st, err := c.VMStop(bg, api.StopParams{StopBeganNS: first}); err != nil || st.State != api.StateStopped {
			t.Fatalf("the first stop: %+v %v", st, err)
		}
		login(t, f)
		f.running()
		second := stopBegins(t, f)
		if st, err := c2.VMStop(bg, api.StopParams{StopBeganNS: second}); err != nil || st.State != api.StateStopped {
			t.Fatalf("the second stop: %+v %v", st, err)
		}
		if r, err := c.Shutdown(bg, api.ShutdownParams{StopRequested: true, StopBeganNS: first}); err != nil || !r.Superseded {
			t.Fatalf("the first stop's shutdown: %+v %v", r, err)
		}
		r, err := c2.Shutdown(bg, api.ShutdownParams{StopRequested: true, StopBeganNS: second})
		endsStopped(t, f, r, err)
	})
}

func TestAStopsShutdownSentAsANotificationIsFinished(t *testing.T) {
	f := started(t)
	began := stopBegins(t, f)
	c := f.raw()
	if err := c.Notify(api.MethodShutdown, api.ShutdownParams{StopRequested: true, StopBeganNS: began}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.d.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon did not finish a shutdown sent as a notification")
	}
}
