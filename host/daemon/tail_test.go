package daemon

import (
	"strings"
	"sync"
	"testing"
	"time"

	api "fev/proto/daemon"
)

type collected struct {
	mu      sync.Mutex
	entries []api.LogEntry
}

func (f *fixture) tail(p api.LogSubscribeParams) *collected {
	f.t.Helper()
	c := &collected{}
	_, err := f.client().SubscribeLogs(bg, p, func(e api.LogEntry) {
		c.mu.Lock()
		c.entries = append(c.entries, e)
		c.mu.Unlock()
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (c *collected) until(t *testing.T, text string) []api.LogEntry {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.mu.Lock()
		for _, e := range c.entries {
			if strings.Contains(e.Text, text) {
				out := append([]api.LogEntry(nil), c.entries...)
				c.mu.Unlock()
				return out
			}
		}
		c.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no log line containing %q arrived", text)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTheUnifiedTailCarriesEverySourceAndInitsLinesOnce(t *testing.T) {
	f := started(t, []string{"chatty=3"})
	live := f.tail(api.LogSubscribeParams{})
	c := f.client()
	before := time.Now()
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	entries := live.until(t, "the VM is running")
	live.until(t, "API listen on")
	live.mu.Lock()
	entries = append([]api.LogEntry(nil), live.entries...)
	live.mu.Unlock()

	bySource := map[string][]api.LogEntry{}
	for _, e := range entries {
		bySource[e.Source] = append(bySource[e.Source], e)
		if e.Level < 0 || e.Level > 7 || e.Dropped != 0 {
			t.Errorf("entry %+v", e)
		}
		at, err := time.Parse(time.RFC3339Nano, e.Time)
		if err != nil || at.Before(before.Add(-time.Second)) || at.After(time.Now().Add(time.Second)) {
			t.Errorf("entry time %q (%v)", e.Time, err)
		}
		if guest := e.Source == api.LogKernel || e.Source == api.LogInit || e.Source == api.LogEngine; guest != (e.GuestMS != 0) && e.Text != "Linux version 7.2.0-fev-standin" {
			t.Errorf("guest_ms on %+v", e)
		}
	}
	for source, want := range map[string]string{
		api.LogDaemon: "starting the VM", api.LogVMM: "stand-in:", api.LogConsole: "chatty console line 2",
		api.LogKernel: "Run /init as init process", api.LogInit: "init: ready at", api.LogEngine: "Starting up",
	} {
		found := false
		for _, e := range bySource[source] {
			found = found || strings.Contains(e.Text, want)
		}
		if !found {
			t.Errorf("the tail has no %s line containing %q: %+v", source, want, bySource[source])
		}
	}
	for _, text := range []string{"init: chatty line 0", "init: chatty line 2", "init: ready at"} {
		n := 0
		for _, e := range entries {
			if strings.Contains(e.Text, text) {
				n++
				if e.Source != api.LogInit {
					t.Errorf("%q arrived from %s", text, e.Source)
				}
			}
		}
		if n != 1 {
			t.Errorf("%q is in the tail %d times", text, n)
		}
	}
	console := f.logFile("console.log")
	if strings.Count(console, "fev-init: init: chatty line") != 3 || !strings.Contains(console, "chatty console line 0") {
		t.Errorf("console.log:\n%s", console)
	}
	guest := f.logFile("guest.log")
	for _, want := range []string{" kernel 6 Run /init as init process", " init 7 init: chatty line 1", " engine 6 Starting up"} {
		if !strings.Contains(guest, want) {
			t.Errorf("guest.log lacks %q:\n%s", want, guest)
		}
	}
	if !strings.Contains(f.logFile("vmm.log"), " vmm 6 fevvm: stand-in:") || !strings.Contains(f.logFile("fevd.log"), " daemon 6 fevd is ready") {
		t.Error("vmm.log or fevd.log lacks its lines")
	}

	late := f.tail(api.LogSubscribeParams{Sources: []string{api.LogInit, api.LogConsole}, Backlog: 2})
	backlog := late.until(t, "init: ready at")
	texts := []string{}
	for _, e := range backlog {
		texts = append(texts, e.Source+": "+e.Text)
	}
	if len(texts) != 4 || !strings.Contains(strings.Join(texts, "\n"), "console: [") || strings.Contains(strings.Join(texts, "\n"), "console: [") && strings.Contains(strings.Join(texts, "\n"), "] fev-init:") {
		t.Errorf("backlog of two per source: %q", texts)
	}
	for i := 1; i < len(backlog); i++ {
		if backlog[i].Time < backlog[i-1].Time {
			t.Errorf("backlog out of order: %q", texts)
		}
	}
}

func TestInitsConsoleLinesAreNotLostWhenTheAgentNeverConnects(t *testing.T) {
	f := newFixture(t, []string{"no-agent=1", "chatty=2"})
	f.opts.Timing.StartDeadline = 150 * time.Millisecond
	f.opts.Timing.Backoff = []time.Duration{time.Hour}
	f.opts.Timing.MaxBackoff = time.Hour
	f.start()
	live := f.tail(api.LogSubscribeParams{Sources: []string{api.LogConsole, api.LogInit}})
	c := f.client()
	c.VMStart(bg, api.StartParams{NoWait: true})
	b := f.until("backoff", func(st api.Status) bool { return st.Phase == api.PhaseBackoff })
	if !contains(b.LastExit.ConsoleTail, "fev-init: init: chatty line 1") {
		t.Errorf("the crash's console tail: %q", b.LastExit.ConsoleTail)
	}
	entries := live.until(t, "fev-init: init: ready at")
	n := 0
	for _, e := range entries {
		if e.Source != api.LogConsole {
			t.Errorf("a line from %s with no agent connected: %+v", e.Source, e)
		}
		if strings.Contains(e.Text, "fev-init: init: chatty line") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("init's two console lines are in the tail %d times", n)
	}
	c.VMStop(bg, api.StopParams{})
}

func TestTheDaemonRotatesItsLogsAsItWrites(t *testing.T) {
	f := newFixture(t, []string{"chatty=400"})
	f.opts.LogMaxBytes = 4096
	f.start()
	live := f.tail(api.LogSubscribeParams{Sources: []string{api.LogConsole}})
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	entries := live.until(t, "chatty console line 399")
	n := 0
	for _, e := range entries {
		if strings.Contains(e.Text, "chatty console line") {
			n++
		}
	}
	if n != 400 {
		t.Errorf("the tail carried %d of 400 console lines across the rotations", n)
	}
	for _, name := range []string{"console.log", "console.log.1", "console.log.2", "console.log.3", "guest.log", "guest.log.1"} {
		data := f.logFile(name)
		if len(data) == 0 || len(data) > 4096 {
			t.Errorf("%s is %d bytes; the limit is 4096", name, len(data))
		}
	}
	if f.logFile("console.log.4") != "" {
		t.Error("a fourth generation was kept")
	}
	if cur := f.logFile("console.log"); !strings.Contains(cur, "chatty console line 399\n") || !strings.HasSuffix(cur, "\n") {
		t.Errorf("console.log ends: %q", cur[max(0, len(cur)-200):])
	}
}
