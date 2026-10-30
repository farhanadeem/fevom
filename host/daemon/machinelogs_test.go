package daemon

import (
	"errors"
	"io"
	"testing"
	"time"

	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func TestAClientThatGoesAwayEndsItsSessionWithNoTerminal(t *testing.T) {
	f := machinesDaemon(t)
	f.create("dev", "ubuntu")
	c := f.client()
	started, err := c.SessionStart(bg, api.SessionStartParams{Argv: []string{"sleep", "30000"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := api.AttachSession(bg, f.layout.Home, started.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	c.Close()
	ended := make(chan error, 1)
	go func() {
		_, _, err := readSession(stream)
		ended <- err
	}()
	select {
	case err := <-ended:
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("the stream ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the session of a connection that ended still runs 10 s later")
	}
	f.m.sessions.mu.Lock()
	left := len(f.m.sessions.byID)
	f.m.sessions.mu.Unlock()
	if left != 0 {
		t.Errorf("the daemon still holds %d sessions", left)
	}
}

func TestAMachinesLogIsTheGuestsWithItsBacklogMarked(t *testing.T) {
	f := machinesDaemon(t)
	f.create("dev", "ubuntu")
	if _, err := f.c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	c := f.client()
	if _, err := c.SubscribeLogs(bg, api.LogSubscribeParams{Sources: []string{api.MachineConsoleLog("nosuch")}, Backlog: 10}, func(api.LogEntry) {}); !api.IsNoMachine(err) {
		t.Fatalf("a machine there is not: %v", err)
	}
	if st, _ := c.Status(bg); st.State != api.StateStopped {
		t.Fatalf("the VM was started for a machine there is not: %s", st.State)
	}
	var e *jsonrpc.Error
	if _, err := c.SubscribeLogs(bg, api.LogSubscribeParams{Sources: []string{api.MachineConsoleLog("dev"), api.LogDaemon}}, func(api.LogEntry) {}); !errors.As(err, &e) || e.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("a machine's source and the daemon's: %v", err)
	}
	for _, c2 := range []struct {
		source string
		want   []string
	}{
		{api.MachineConsoleLog("dev"), []string{"fev-standin: the console of dev", "dev login:"}},
		{api.MachineRuntimeLog("dev"), []string{"machine dev: made by the stand-in", "machine dev: started"}},
	} {
		lines := make(chan api.LogEntry, 16)
		end := make(chan struct{})
		stop, err := c.SubscribeLogsMarked(bg, api.LogSubscribeParams{Sources: []string{c2.source}, Backlog: 1000},
			func(e api.LogEntry) { lines <- e }, func() { close(end) })
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-end:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: no end of the backlog", c2.source)
		}
		close(lines)
		var got []string
		for l := range lines {
			if l.Source != c2.source || l.Time == "" {
				t.Errorf("%s: a line %+v", c2.source, l)
			}
			got = append(got, l.Text)
		}
		if len(got) != len(c2.want) || got[0] != c2.want[0] || got[1] != c2.want[1] {
			t.Errorf("%s: %q, want %q", c2.source, got, c2.want)
		}
		stop()
	}
	if st, _ := c.Status(bg); st.State != api.StateRunning && st.State != api.StateStarting {
		t.Errorf("the VM is %s after a machine's log was asked for", st.State)
	}
	end := make(chan struct{})
	if _, err := c.SubscribeLogsMarked(bg, api.LogSubscribeParams{Sources: []string{api.LogDaemon}, Backlog: 5}, func(api.LogEntry) {}, func() { close(end) }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-end:
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon's own tail: no end of the backlog")
	}
}

func TestAMachinesConfigurationCarriesTheShare(t *testing.T) {
	f := machinesDaemon(t)
	f.create("dev", "ubuntu")
	if !f.m.configFor("dev").Share {
		t.Error("with files.share on, a machine is not given the share")
	}
	if _, err := f.c.ConfigSet(bg, map[string]any{api.KeyFilesShare: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.VMRestart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	if in, _ := f.m.cfg.InEffect(api.KeyFilesShare); in != false {
		t.Fatalf("files.share after the restart: %v", in)
	}
	if f.m.configFor("dev").Share {
		t.Error("with files.share off, a machine is given the share")
	}
}
