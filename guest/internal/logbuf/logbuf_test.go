package logbuf

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func collect(t *testing.T, ch <-chan Entry, n int) []string {
	t.Helper()
	var out []string
	for len(out) < n {
		select {
		case e := <-ch:
			out = append(out, e.Text)
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d of %d entries: %v", len(out), n, out)
		}
	}
	return out
}

func TestBacklogThenLiveInOrder(t *testing.T) {
	var console bytes.Buffer
	b := New(4, func() float64 { return 1 }, &console)
	for i := range 6 {
		b.Logf(Info, "old %d", i)
	}
	ch := make(chan Entry, 64)
	cancel := b.Subscribe(true, func(e Entry) { ch <- e })
	b.Logf(Info, "new 0")
	b.Logf(Warning, "new 1")
	got := collect(t, ch, 6)
	want := []string{"old 2", "old 3", "old 4", "old 5", "new 0", "new 1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v", got)
	}
	cancel()
	cancel()
	if b.Subscribers() != 0 {
		t.Fatal("subscription not removed")
	}
	b.Logf(Info, "after")
	select {
	case e := <-ch:
		t.Fatalf("delivered %q after cancel", e.Text)
	case <-time.After(20 * time.Millisecond):
	}
	if !strings.Contains(console.String(), "fev-init: old 0\n") {
		t.Fatalf("console got %q", console.String())
	}
	b.Logf(Debug, "detail")
	if strings.Contains(console.String(), "detail") {
		t.Fatal("debug entries belong to subscribers, not the console")
	}
}

func TestWithoutBacklogOnlyNewEntries(t *testing.T) {
	b := New(8, func() float64 { return 1 }, nil)
	b.Logf(Info, "before")
	ch := make(chan Entry, 8)
	defer b.Subscribe(false, func(e Entry) { ch <- e })()
	b.Logf(Info, "after")
	if got := collect(t, ch, 1); got[0] != "after" {
		t.Fatalf("got %v", got)
	}
}

func TestSlowSubscriberNeverBlocksLoggingAndIsToldWhatItMissed(t *testing.T) {
	b := New(8, func() float64 { return 1 }, nil)
	release := make(chan struct{})
	ch := make(chan Entry, 4*subQueueLimit)
	defer b.Subscribe(false, func(e Entry) { <-release; ch <- e })()

	done := make(chan struct{})
	go func() {
		for i := range 3 * subQueueLimit {
			b.Logf(Info, "%d", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging blocked on a stuck subscriber")
	}
	close(release)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-ch:
			if strings.Contains(e.Text, "log entries dropped") {
				var n int
				fmt.Sscanf(e.Text, "%d", &n)
				if n <= 0 {
					t.Fatalf("bad drop notice %q", e.Text)
				}
				return
			}
		case <-deadline:
			t.Fatal("no drop notice")
		}
	}
}

func TestTheBacklogsEndIsMarkedAfterItsLastEntry(t *testing.T) {
	b := New(4, func() float64 { return 0 }, nil)
	for i := range 6 {
		b.Logf(Info, "old %d", i)
	}
	got := make(chan string, 16)
	hold := make(chan struct{})
	cancel := b.SubscribeMarked(true, func(e Entry) {
		<-hold
		got <- e.Text
	}, func() { got <- "END" })
	defer cancel()
	b.Logf(Info, "new")
	close(hold)
	var seen []string
	for len(seen) < 6 {
		seen = append(seen, <-got)
	}
	want := "old 2,old 3,old 4,old 5,END,new"
	if s := strings.Join(seen, ","); s != want {
		t.Fatalf("got %s, want %s", s, want)
	}

	for _, backlog := range []bool{false, true} {
		empty := New(4, func() float64 { return 0 }, nil)
		if !backlog {
			empty.Logf(Info, "not asked for")
		}
		marked := make(chan struct{})
		c := empty.SubscribeMarked(backlog, func(Entry) {}, func() { close(marked) })
		select {
		case <-marked:
		case <-time.After(5 * time.Second):
			t.Fatalf("backlog %v: no mark", backlog)
		}
		c()
	}
}
