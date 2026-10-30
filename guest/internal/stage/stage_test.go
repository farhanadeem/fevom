package stage

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func clock() func() float64 {
	var t atomic.Int64
	return func() float64 { return float64(t.Add(1)) }
}

func byName(rs []Result) map[string]Result {
	m := map[string]Result{}
	for _, r := range rs {
		m[r.Name] = r
	}
	return m
}

func TestDependenciesFinishBeforeDependentsStart(t *testing.T) {
	var mu sync.Mutex
	var order []string
	mark := func(name string) func() error {
		return func() error {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return nil
		}
	}
	rs, err := Run([]Stage{
		{Name: "c", After: []string{"a", "b"}, Run: mark("c")},
		{Name: "a", Run: mark("a")},
		{Name: "b", After: []string{"a"}, Run: mark("b")},
	}, clock())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ""); got != "abc" {
		t.Fatalf("ran in order %q", got)
	}
	if rs[0].Name != "c" || rs[1].Name != "a" || rs[2].Name != "b" {
		t.Fatalf("results out of order: %+v", rs)
	}
	r := byName(rs)
	if !(r["a"].EndMS <= r["b"].StartMS && r["b"].EndMS <= r["c"].StartMS) {
		t.Fatalf("timestamps do not nest: %+v", rs)
	}
	for _, x := range rs {
		if x.EndMS < x.StartMS {
			t.Fatalf("%s ends before it starts", x.Name)
		}
	}
}

func TestIndependentStagesRunConcurrently(t *testing.T) {
	var started sync.WaitGroup
	started.Add(2)
	both := func() error {
		started.Done()
		done := make(chan struct{})
		go func() { started.Wait(); close(done) }()
		select {
		case <-done:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("the other stage never started")
		}
	}
	rs, err := Run([]Stage{{Name: "x", Run: both}, {Name: "y", Run: both}}, clock())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.Name, r.Err)
		}
	}
}

func TestFailureOfARequiredStageStopsItsDependentsOnly(t *testing.T) {
	ran := map[string]*atomic.Bool{"dependent": {}, "transitive": {}, "unrelated": {}, "tolerant": {}}
	set := func(n string) func() error { return func() error { ran[n].Store(true); return nil } }
	rs, err := Run([]Stage{
		{Name: "required", Required: true, Run: func() error { return errors.New("no") }},
		{Name: "dependent", Required: true, After: []string{"required"}, Run: set("dependent")},
		{Name: "transitive", After: []string{"dependent"}, Run: set("transitive")},
		{Name: "unrelated", Run: set("unrelated")},
		{Name: "optional", Run: func() error { return errors.New("meh") }},
		{Name: "tolerant", After: []string{"optional"}, Run: set("tolerant")},
	}, clock())
	if err != nil {
		t.Fatal(err)
	}
	r := byName(rs)
	if r["required"].Err == nil || !r["dependent"].NotRun || !r["transitive"].NotRun {
		t.Fatalf("required failure did not propagate: %+v", rs)
	}
	if ran["dependent"].Load() || ran["transitive"].Load() {
		t.Fatal("a stage ran after its required dependency failed")
	}
	if !ran["unrelated"].Load() || !ran["tolerant"].Load() {
		t.Fatal("an unaffected stage did not run")
	}
	if r["optional"].Err == nil || r["tolerant"].NotRun {
		t.Fatalf("optional failure handled wrongly: %+v", rs)
	}
}

func TestSkipsAndPanicsAreRecorded(t *testing.T) {
	rs, err := Run([]Stage{
		{Name: "skip", Required: true, Run: func() error { return Skip("no %s", "disk") }},
		{Name: "after-skip", After: []string{"skip"}, Run: func() error { return nil }},
		{Name: "panics", Run: func() error { panic("bug") }},
	}, clock())
	if err != nil {
		t.Fatal(err)
	}
	r := byName(rs)
	if r["skip"].Skipped != "no disk" || r["skip"].Err != nil {
		t.Fatalf("skip: %+v", r["skip"])
	}
	if r["after-skip"].NotRun {
		t.Fatal("a skipped stage blocked its dependent")
	}
	if r["panics"].Err == nil || !strings.Contains(r["panics"].Err.Error(), "bug") {
		t.Fatalf("panic: %+v", r["panics"])
	}
}

func TestMalformedGraphsAreRefused(t *testing.T) {
	nop := func() error { return nil }
	cases := map[string][]Stage{
		"twice":   {{Name: "a", Run: nop}, {Name: "a", Run: nop}},
		"unknown": {{Name: "a", After: []string{"ghost"}, Run: nop}},
		"cycle":   {{Name: "a", After: []string{"b"}, Run: nop}, {Name: "b", After: []string{"c"}, Run: nop}, {Name: "c", After: []string{"a"}, Run: nop}},
	}
	for want, stages := range cases {
		if _, err := Run(stages, clock()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
}
