package logs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	api "fev/proto/daemon"
)

func open(t *testing.T, max int64) (*Hub, string) {
	t.Helper()
	dir := t.TempDir()
	h, err := Open(Options{Dir: dir, MaxBytes: max})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, dir
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestEverySourceHasItsFileAndTheGuestSourcesShareOne(t *testing.T) {
	h, dir := open(t, 0)
	at := time.Date(2026, 10, 8, 12, 0, 0, 123456000, time.UTC)
	for _, s := range Sources {
		h.Write(Entry{Source: s, Time: at, Level: 6, Text: "line from " + s})
	}
	h.Write(Entry{Source: "nonsense", Text: "dropped"})
	for file, sources := range map[string][]string{
		"fevd.log": {api.LogDaemon}, "vmm.log": {api.LogVMM}, "console.log": {api.LogConsole},
		"guest.log": {api.LogKernel, api.LogInit, api.LogEngine},
	} {
		got := read(t, filepath.Join(dir, file))
		var want string
		for _, s := range sources {
			want += fmt.Sprintf("2026-10-08T12:00:00.123456Z %s 6 line from %s\n", s, s)
		}
		if got != want {
			t.Errorf("%s:\n%s\nwant:\n%s", file, got, want)
		}
	}
}

func TestAFileIsRotatedAtItsLimitAndThreeGenerationsAreKept(t *testing.T) {
	const max = 1000
	h, dir := open(t, max)
	path := filepath.Join(dir, "vmm.log")
	const lines = 200
	for i := 0; i < lines; i++ {
		h.Write(Entry{Source: api.LogVMM, Level: 6, Text: fmt.Sprintf("line %04d", i)})
	}
	var all []string
	for _, name := range []string{path + ".3", path + ".2", path + ".1", path} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Size() > max || info.Size() == 0 {
			t.Errorf("%s is %d bytes; the limit is %d", name, info.Size(), max)
		}
		for _, l := range strings.Split(strings.TrimRight(read(t, name), "\n"), "\n") {
			all = append(all, l[strings.LastIndex(l, "line "):])
		}
	}
	if _, err := os.Stat(path + ".4"); err == nil {
		t.Error("a fourth generation was kept")
	}
	for i, l := range all {
		if want := fmt.Sprintf("line %04d", lines-len(all)+i); l != want {
			t.Fatalf("line %d across the generations is %q, want %q", i, l, want)
		}
	}
	if len(all) < 60 {
		t.Errorf("only %d lines survive in four files of %d bytes", len(all), max)
	}
	if _, err := os.Stat(filepath.Join(dir, "console.log.1")); err == nil {
		t.Error("console.log was rotated though nothing was written to it")
	}
}

func TestAFileLeftOverTheLimitIsRotatedAtOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fevd.log")
	os.WriteFile(path, []byte(strings.Repeat("x", 600)+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "vmm.log"), []byte("short\n"), 0o644)
	h, err := Open(Options{Dir: dir, MaxBytes: 500})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.Write(Entry{Source: api.LogDaemon, Text: "new"})
	h.Write(Entry{Source: api.LogVMM, Text: "appended"})
	if got := read(t, path+".1"); len(got) != 601 {
		t.Errorf("the old file was not moved aside whole: %d bytes", len(got))
	}
	if got := read(t, path); !strings.HasSuffix(got, " daemon 0 new\n") || strings.Contains(got, "xxx") {
		t.Errorf("the new file holds %q", got)
	}
	if got := read(t, filepath.Join(dir, "vmm.log")); !strings.HasPrefix(got, "short\n") || !strings.HasSuffix(got, "appended\n") {
		t.Errorf("vmm.log: %q", got)
	}
}

func next(t *testing.T, s *Sub) (Entry, int) {
	t.Helper()
	type got struct {
		e       Entry
		dropped int
		ok      bool
	}
	ch := make(chan got, 1)
	stop := make(chan struct{})
	go func() {
		e, d, ok := s.Next(stop)
		ch <- got{e, d, ok}
	}()
	select {
	case g := <-ch:
		if !g.ok {
			t.Fatal("the subscription ended")
		}
		return g.e, g.dropped
	case <-time.After(5 * time.Second):
		close(stop)
		t.Fatal("no line arrived")
		return Entry{}, 0
	}
}

func TestTheUnifiedTailCarriesEverySourceBacklogFirst(t *testing.T) {
	h, _ := open(t, 0)
	base := time.Now().Add(-time.Minute)
	h.Write(Entry{Source: api.LogDaemon, Time: base.Add(1 * time.Second), Text: "d1"})
	h.Write(Entry{Source: api.LogConsole, Time: base.Add(3 * time.Second), Text: "c3"})
	h.Write(Entry{Source: api.LogKernel, Time: base.Add(2 * time.Second), GuestMS: 7.5, Level: 4, Text: "k2"})
	h.Write(Entry{Source: api.LogDaemon, Time: base.Add(4 * time.Second), Text: "d4"})

	all := h.Subscribe(nil, 10)
	defer all.Close()
	for _, want := range []string{"d1", "k2", "c3", "d4"} {
		if e, dropped := next(t, all); e.Text != want || dropped != 0 || e.End {
			t.Fatalf("backlog: got %q (dropped %d), want %q", e.Text, dropped, want)
		}
	}
	if e, _ := next(t, all); !e.End {
		t.Fatalf("after the backlog: %+v, want its end", e)
	}
	some := h.Subscribe([]string{api.LogDaemon, api.LogKernel}, 1)
	defer some.Close()
	for _, want := range []string{"k2", "d4", ""} {
		if e, _ := next(t, some); e.Text != want || e.End != (want == "") {
			t.Fatalf("filtered backlog: got %+v, want %q", e, want)
		}
	}
	empty := h.Subscribe([]string{api.LogEngine}, 5)
	defer empty.Close()
	if e, _ := next(t, empty); !e.End {
		t.Fatalf("an empty backlog: %+v, want its end", e)
	}
	none := h.Subscribe([]string{api.LogVMM}, 0)
	defer none.Close()

	h.Write(Entry{Source: api.LogVMM, Text: "v5"})
	h.Write(Entry{Source: api.LogKernel, GuestMS: 9, Level: 3, Text: "k6"})
	if e, _ := next(t, all); e.Text != "v5" || e.Source != api.LogVMM || e.Time.IsZero() {
		t.Errorf("live: %+v", e)
	}
	if e, _ := next(t, all); e.Text != "k6" || e.GuestMS != 9 || e.Level != 3 {
		t.Errorf("live: %+v", e)
	}
	if e, _ := next(t, some); e.Text != "k6" {
		t.Errorf("a subscriber to two sources got %q", e.Text)
	}
	if e, _ := next(t, none); e.Text != "v5" {
		t.Errorf("a subscriber with no backlog got %q", e.Text)
	}
	all.Close()
	if _, _, ok := all.Next(nil); ok {
		t.Error("Next succeeded on a closed subscription")
	}
}

func TestASlowSubscriberLosesLinesAndIsToldHowMany(t *testing.T) {
	h, _ := open(t, 0)
	s := h.Subscribe([]string{api.LogConsole}, 0)
	defer s.Close()
	const extra = 500
	for i := 0; i < QueueLines+extra; i++ {
		h.Write(Entry{Source: api.LogConsole, Text: fmt.Sprintf("%d", i)})
	}
	h.mu.Lock()
	queued := len(s.queue)
	h.mu.Unlock()
	if queued != QueueLines {
		t.Fatalf("%d lines are queued for a subscriber that reads nothing; the bound is %d", queued, QueueLines)
	}
	for i := 0; i < QueueLines; i++ {
		if e, dropped := next(t, s); e.Text != fmt.Sprintf("%d", i) || dropped != 0 {
			t.Fatalf("line %d: %q, dropped %d", i, e.Text, dropped)
		}
	}
	h.Write(Entry{Source: api.LogConsole, Text: "after"})
	if e, dropped := next(t, s); e.Text != "after" || dropped != extra {
		t.Errorf("the line after the gap: %q, reporting %d lost, want %d", e.Text, dropped, extra)
	}
	h.Write(Entry{Source: api.LogConsole, Text: "again"})
	if _, dropped := next(t, s); dropped != 0 {
		t.Errorf("the loss was reported twice: %d", dropped)
	}
}

func TestSubscribingDuringWritesNeitherLosesNorRepeatsALine(t *testing.T) {
	h, _ := open(t, 0)
	const lines = 150
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < lines; i++ {
			h.Write(Entry{Source: api.LogInit, Text: fmt.Sprintf("%d", i)})
		}
	}()
	s := h.Subscribe([]string{api.LogInit}, RingLines)
	defer s.Close()
	wg.Wait()
	marks := 0
	for i := 0; i < lines; i++ {
		e, _ := next(t, s)
		if e.End {
			marks++
			i--
			continue
		}
		if e.Text != fmt.Sprintf("%d", i) {
			t.Fatalf("line %d arrived as %q", i, e.Text)
		}
	}
	if marks > 1 {
		t.Errorf("the backlog's end was marked %d times", marks)
	}
}

func TestAHeldLineStaysOutOfTheTailUntilReleased(t *testing.T) {
	h, dir := open(t, 0)
	live := h.Subscribe([]string{api.LogConsole}, 0)
	defer live.Close()
	h.WriteHeld(Entry{Source: api.LogConsole, Text: "fev-init: early, boot 1"}, 1)
	h.WriteHeld(Entry{Source: api.LogConsole, Text: "fev-init: early, boot 2"}, 2)
	h.Write(Entry{Source: api.LogConsole, Text: "kernel panic"})

	if e, _ := next(t, live); e.Text != "kernel panic" {
		t.Fatalf("a held line reached the tail: %q", e.Text)
	}
	if got := read(t, filepath.Join(dir, "console.log")); strings.Count(got, "fev-init: early") != 2 {
		t.Errorf("held lines are missing from the file:\n%s", got)
	}
	if got := h.Tail(api.LogConsole, 10); len(got) != 3 || got[0] != "fev-init: early, boot 1" {
		t.Errorf("the crash tail lacks held lines: %q", got)
	}
	if got := h.Tail(api.LogConsole, 1); len(got) != 1 || got[0] != "kernel panic" {
		t.Errorf("a tail of one: %q", got)
	}
	backlog := h.Subscribe([]string{api.LogConsole}, 10)
	if e, _ := next(t, backlog); e.Text != "kernel panic" {
		t.Errorf("a held line is in the backlog: %q", e.Text)
	}
	backlog.Close()

	h.Release(api.LogConsole, 2)
	if e, _ := next(t, live); e.Text != "fev-init: early, boot 2" {
		t.Errorf("released: %q", e.Text)
	}
	backlog = h.Subscribe([]string{api.LogConsole}, 10)
	defer backlog.Close()
	for _, want := range []string{"fev-init: early, boot 2", "kernel panic"} {
		if e, _ := next(t, backlog); e.Text != want {
			t.Errorf("backlog after release: %q, want %q", e.Text, want)
		}
	}
}

func TestMemoryPerSourceIsBounded(t *testing.T) {
	h, _ := open(t, 0)
	for i := 0; i < RingLines*3; i++ {
		h.Write(Entry{Source: api.LogConsole, Text: fmt.Sprintf("%d", i)})
	}
	tail := h.Tail(api.LogConsole, RingLines*3)
	if len(tail) != RingLines || tail[0] != fmt.Sprintf("%d", RingLines*2) {
		t.Errorf("the ring holds %d lines starting at %q", len(tail), tail[0])
	}
	h.Write(Entry{Source: api.LogConsole, Text: strings.Repeat("x", MaxLine*4)})
	if got := h.Tail(api.LogConsole, 1); len(got[0]) != MaxLine {
		t.Errorf("a line of %d bytes was kept at %d", MaxLine*4, len(got[0]))
	}
}

func TestALineBreakInALineDoesNotMakeAnotherLine(t *testing.T) {
	h, dir := open(t, 0)
	h.Write(Entry{Source: api.LogInit, Level: 6, Text: "one\n2026-01-01T00:00:00.000000Z kernel 0 forged\r\nend"})
	got := read(t, filepath.Join(dir, "guest.log"))
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, " init 6 one\\n2026-01-01T00:00:00.000000Z kernel 0 forged\\r\\nend\n") {
		t.Errorf("guest.log: %q", got)
	}
	if tail := h.Tail(api.LogInit, 10); len(tail) != 1 || strings.ContainsAny(tail[0], "\r\n") {
		t.Errorf("tail: %q", tail)
	}
}

func TestLogFilesAreForThisUserAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "log")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "fevd.log"), []byte("an older line\n"), 0o644)
	h, err := Open(Options{Dir: dir, MaxBytes: 200})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for i := 0; i < 3; i++ {
		for _, s := range Sources {
			h.Write(Entry{Source: s, Level: 6, Text: "a line long enough to rotate the file after a few of them"})
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) < 5 {
		t.Fatalf("%d files; the test wants rotated ones too", len(entries))
	}
	if old, err := os.ReadFile(filepath.Join(dir, "fevd.log.1")); err != nil || !strings.HasPrefix(string(old), "an older line") {
		t.Fatalf("the file an earlier build left is not among them: %q, %v", old, err)
	}
	for _, e := range entries {
		if info, _ := e.Info(); info.Mode().Perm() != 0o600 {
			t.Errorf("%s is mode %v", e.Name(), info.Mode().Perm())
		}
	}
}

func TestTheFilesReadBackInOrderAcrossRotationsAndSources(t *testing.T) {
	h, dir := open(t, 300)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	const each = 40
	for i := 0; i < each; i++ {
		h.Write(Entry{Source: api.LogDaemon, Time: at.Add(time.Duration(2*i) * time.Millisecond), Level: 6, Text: fmt.Sprintf("daemon %02d", i)})
		h.Write(Entry{Source: api.LogKernel, Time: at.Add(time.Duration(2*i+1) * time.Millisecond), Level: 4, GuestMS: float64(i), Text: fmt.Sprintf("kernel %02d with spaces", i)})
	}
	h.Close()
	if _, err := os.Stat(filepath.Join(dir, "fevd.log.3")); err != nil {
		t.Fatalf("the test did not rotate three times: %v", err)
	}
	var got []Entry
	if err := ReadAll(dir, nil, func(e Entry) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	if len(got) < 20 || len(got) > 2*each {
		t.Fatalf("%d lines read back", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time.Before(got[i-1].Time) {
			t.Fatalf("line %d (%s %q) is before the one above it (%s)", i, got[i].Time, got[i].Text, got[i-1].Time)
		}
	}
	last := got[len(got)-1]
	if last.Source != api.LogKernel || last.Text != fmt.Sprintf("kernel %02d with spaces", each-1) || last.Level != 4 {
		t.Errorf("the last line read back is %+v", last)
	}
	daemonLines, prev := 0, ""
	for _, e := range got {
		if e.Source != api.LogDaemon {
			continue
		}
		if e.Text <= prev {
			t.Fatalf("daemon lines out of order: %q after %q", e.Text, prev)
		}
		prev = e.Text
		daemonLines++
	}
	var only []Entry
	if err := ReadAll(dir, []string{api.LogDaemon}, func(e Entry) { only = append(only, e) }); err != nil {
		t.Fatal(err)
	}
	if len(only) != daemonLines || len(only) == 0 {
		t.Errorf("%d daemon lines alone, %d among all", len(only), daemonLines)
	}
	os.WriteFile(filepath.Join(dir, "vmm.log"), []byte("garbage\n2026-10-08T12:00:09.000000Z vmm 6 after the garbage\n"), 0o600)
	var vmm []Entry
	if err := ReadAll(dir, []string{api.LogVMM}, func(e Entry) { vmm = append(vmm, e) }); err != nil || len(vmm) != 1 || vmm[0].Text != "after the garbage" {
		t.Errorf("around a broken line: %+v, %v", vmm, err)
	}
	if err := ReadAll(filepath.Join(dir, "none"), nil, func(Entry) { t.Error("a line from nowhere") }); err != nil {
		t.Errorf("a home that has never run: %v", err)
	}
}
