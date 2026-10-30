package boottrace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	api "fev/proto/daemon"
)

const ms = 1_000_000

func record(stages ...float64) *Recorder {
	origin := uint64(5_000_000 * ms)
	r := New("b00701", "build", "client", origin, 2, 4096)
	at := origin
	at += uint64(stages[0] * ms)
	r.MarkAt(VMMSpawn, at)
	at += uint64(stages[1] * ms)
	r.MarkAt(VMMGuestEntry, at)
	r.SetGuestClock(origin+499*ms, origin+501*ms, 100)
	guest := float64(at-origin)/ms - 400
	guest += stages[2]
	r.MarkGuest(InitMain, guest, false)
	guest += stages[3]
	r.MarkGuest(InitReady, guest, false)
	if len(stages) > 4 {
		guest += stages[4]
		r.MarkGuest(EngineReady, guest, false)
	}
	if len(stages) > 5 {
		r.MarkAt(DockerReleased, origin+uint64((guest+400+stages[5])*ms))
	}
	return r
}

func TestStagesAreMeasuredBetweenMarksOnOneClock(t *testing.T) {
	tr := record(9, 7, 24, 2, 858, 5).Trace()
	if !tr.Complete || !tr.OK || len(tr.Stages) != 6 {
		t.Fatalf("trace: complete=%v ok=%v stages=%d", tr.Complete, tr.OK, len(tr.Stages))
	}
	for i, want := range []float64{9, 7, 24, 2, 858, 5} {
		s := tr.Stages[i]
		if s.Stage != i || s.MS != want || s.Over || s.BudgetMS != Budget[i].BudgetMS || s.From != Budget[i].From || s.To != Budget[i].To {
			t.Errorf("stage %d: %+v, want %v ms", i, s, want)
		}
	}
	if tr.TotalMS != 905 || tr.BudgetMS != 2000 || tr.GuestOffsetMS != 400 || tr.GuestErrorMS != 1 {
		t.Errorf("total %v, budget %v, guest offset %v, guest error %v", tr.TotalMS, tr.BudgetMS, tr.GuestOffsetMS, tr.GuestErrorMS)
	}
	for i := 1; i < len(tr.Marks); i++ {
		if tr.Marks[i].MS < tr.Marks[i-1].MS {
			t.Errorf("marks out of order: %+v", tr.Marks)
		}
	}
	if tr.V != Version || tr.Clock != "uptime_raw" || tr.Origin != "client" || tr.CPUs != 2 || tr.MemMiB != 4096 {
		t.Errorf("head: %+v", tr)
	}
	if _, err := time.Parse(time.RFC3339Nano, tr.Wall); err != nil {
		t.Errorf("wall %q: %v", tr.Wall, err)
	}
}

func TestAStageOverItsBudgetIsNamed(t *testing.T) {
	tr := record(9, 7, 351, 2, 858, 5).Trace()
	over := Over(tr)
	if tr.OK || len(over) != 1 || over[0].Stage != 2 || over[0].MS != 351 || over[0].BudgetMS != 350 {
		t.Errorf("ok=%v over=%+v", tr.OK, over)
	}
	tr = record(50, 100, 350, 200, 1100, 200.5).Trace()
	if tr.OK || len(Over(tr)) != 1 {
		t.Errorf("a stage half a millisecond over: ok=%v over=%+v", tr.OK, Over(tr))
	}
	tr = record(50, 100, 350, 200, 1100, 200).Trace()
	if !tr.OK || tr.TotalMS != 2000 {
		t.Errorf("exactly on budget: ok=%v total=%v", tr.OK, tr.TotalMS)
	}
}

func TestABootThatDidNotFinishIsNotWithinBudget(t *testing.T) {
	r := record(9, 7, 24, 2)
	tr := r.Trace()
	if tr.Complete || tr.OK || len(tr.Stages) != 4 || tr.TotalMS != 42 {
		t.Errorf("complete=%v ok=%v stages=%d total=%v", tr.Complete, tr.OK, len(tr.Stages), tr.TotalMS)
	}
	r.End()
	r.Mark(DockerReleased)
	r.MarkGuest(EngineReady, 900, false)
	if len(r.Trace().Stages) != 4 {
		t.Error("a mark placed after the end of the boot was recorded")
	}
}

func TestGuestMarksWaitForTheGuestClockAndAreLabelledWhenApproximate(t *testing.T) {
	origin := Now()
	r := New("b1", "build", "daemon", origin, 1, 1024)
	r.MarkGuest(InitMain, 8, false)
	r.MarkGuest(KernelRunInit, 7.5, true)
	if n := len(r.Trace().Marks); n != 0 {
		t.Fatalf("%d guest marks were placed before the guest's clock was", n)
	}
	if _, ok := r.GuestToWall(8); ok {
		t.Error("a guest time was converted before the guest's clock was placed")
	}
	r.SetGuestClock(origin+30*ms, origin+30*ms, 10)
	r.SetGuestClock(origin+90*ms, origin+90*ms, 10)
	r.MarkGuest(InitMain, 99, false)
	marks := map[string]api.Mark{}
	for _, m := range r.Trace().Marks {
		marks[m.Name] = m
	}
	if m := marks[InitMain]; m.MS != 28 || m.Approx {
		t.Errorf("init.main: %+v", m)
	}
	if m := marks[KernelRunInit]; m.MS != 27.5 || !m.Approx {
		t.Errorf("kernel.run_init: %+v", m)
	}
	wall, ok := r.GuestToWall(8)
	head, _ := time.Parse(time.RFC3339Nano, r.Trace().Wall)
	if !ok || wall.Sub(head) != 28*time.Millisecond {
		t.Errorf("guest 8 ms is %v after the origin's wall time", wall.Sub(head))
	}
}

func TestEachBootIsOneLineOfTheLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot-trace.jsonl")
	for _, r := range []*Recorder{record(9, 7, 24, 2, 858, 5), record(9, 7, 24, 2)} {
		if err := Append(path, r.Trace()); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []api.Trace
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var tr api.Trace
		if err := api.Conforms(sc.Bytes(), api.Trace{}); err != nil {
			t.Errorf("a line does not fit the schema: %v", err)
		}
		json.Unmarshal(sc.Bytes(), &tr)
		lines = append(lines, tr)
	}
	if len(lines) != 2 || !lines[0].Complete || lines[1].Complete {
		t.Errorf("lines: %+v", lines)
	}
}

func TestTheClockIsMonotonicInNanoseconds(t *testing.T) {
	a := Now()
	time.Sleep(2 * time.Millisecond)
	if d := Now() - a; d < 2*ms || d > 2000*ms {
		t.Errorf("2 ms of sleep measured as %d ns", d)
	}
}

func TestTheTraceLogIsRotatedAtItsLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot-trace.jsonl")
	r := New("b00001", "dev", "daemon", Now(), 2, 1024)
	r.Mark(VMMSpawn)
	one := r.Trace()
	line, _ := json.Marshal(one)
	max := int64(2*len(line) + 2 + len(line)/2)
	for i := 0; i < 11; i++ {
		if err := AppendRotating(path, one, max, 3); err != nil {
			t.Fatal(err)
		}
	}
	count := func(p string) int {
		data, err := os.ReadFile(p)
		if err != nil {
			return -1
		}
		return strings.Count(string(data), "\n")
	}
	if got := []int{count(path), count(path + ".1"), count(path + ".2"), count(path + ".3"), count(path + ".4")}; !reflect.DeepEqual(got, []int{1, 2, 2, 2, -1}) {
		t.Fatalf("records in the file and its generations: %v", got)
	}
	big := filepath.Join(t.TempDir(), "boot-trace.jsonl")
	os.WriteFile(big, []byte(strings.Repeat(string(line)+"\n", 9)), 0o600)
	if err := AppendRotating(big, one, max, 3); err != nil {
		t.Fatal(err)
	}
	if count(big) != 1 || count(big+".1") != 9 {
		t.Errorf("an oversized file: %d records left, %d moved aside", count(big), count(big+".1"))
	}
}
