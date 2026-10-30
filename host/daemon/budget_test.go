package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fev/host/power"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/vmmctl"
)

type mac struct{ available, image atomic.Uint64 }

func (m *mac) set(available, image uint64) {
	m.available.Store(available)
	m.image.Store(image)
}

func budgeted(t *testing.T, script ...string) (*fixture, *DataImage, *mac, *power.Injected) {
	t.Helper()
	f := newFixture(t, script)
	figures := &mac{}
	figures.set(10<<30, 2<<30)
	m := &DataImage{Path: f.layout.DataImage(), Margin: 1 << 30}
	m.free = func(string) (uint64, error) { return figures.available.Load(), nil }
	m.blocks = func(string) (uint64, error) { return figures.image.Load(), nil }
	f.launch.Process.DataImage = m.Path
	f.opts.Modules = []Module{m}
	src := power.NewInjected()
	f.opts.Power = src
	f.start()
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	f.running()
	return f, m, figures, src
}

var (
	baselineLine = regexp.MustCompile(`control: budget: baseline (\d+) bytes available, image (\d+); (\d+) held`)
	guestLine    = regexp.MustCompile(`storage: budget (\d+) bytes: limit (\d+)`)
)

func (f *fixture) baselines() [][2]uint64 {
	var out [][2]uint64
	for _, m := range baselineLine.FindAllStringSubmatch(f.logFile("vmm.log"), -1) {
		a, _ := strconv.ParseUint(m[1], 10, 64)
		b, _ := strconv.ParseUint(m[2], 10, 64)
		out = append(out, [2]uint64{a, b})
	}
	return out
}

func (f *fixture) budgets() []int64 {
	var out []int64
	for _, m := range guestLine.FindAllStringSubmatch(f.logFile("guest.log"), -1) {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		out = append(out, n)
	}
	return out
}

func (f *fixture) settled(m *DataImage, n int) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if len(f.budgets()) >= n && len(f.baselines()) >= n && m.budgetIdle() {
			break
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("%d budgets and %d baselines after 10 s, want %d each\n%s", len(f.budgets()), len(f.baselines()), n, f.logFile("fevd.log"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got, told := len(f.budgets()), len(f.baselines()); got != n || told != n {
		f.t.Fatalf("%d budgets and %d baselines, want %d each\n%s", got, told, n, f.logFile("fevd.log"))
	}
}

const (
	guestUsed     = 6 << 20
	guestReserved = 256 << 20
)

func TestTheBudgetIsSetAtBootAndOnEachEventFromTheMacsFigures(t *testing.T) {
	f, m, figures, _ := budgeted(t)
	f.settled(m, 1)
	want := func(i int, available, image uint64) {
		t.Helper()
		if got := f.baselines()[i]; got != [2]uint64{available, image} {
			t.Errorf("baseline %d: the VMM was sent %v, want %d and %d", i, got, available, image)
		}
		budget := agent.BudgetBytes(int64(available), 1<<30, int64(image), guestUsed, guestReserved)
		if got := f.budgets()[i]; got != budget {
			t.Errorf("budget %d: the guest set %d, want %d", i, got, budget)
		}
	}
	want(0, 10<<30, 2<<30)
	guest := f.logFile("guest.log")
	if b, e := strings.Index(guest, "storage: budget"), strings.Index(guest, "API listen on"); b < 0 || (e >= 0 && e < b) {
		t.Errorf("the boot's budget came after the engine was ready:\n%s", guest)
	}
	if !strings.Contains(f.logFile("fevd.log"), "why=boot") {
		t.Errorf("no budget named for the boot:\n%s", f.logFile("fevd.log"))
	}

	for i, event := range []struct {
		why   string
		cause func()
	}{
		{"trim", m.Trimmed},
		{"quiet", m.Quiet},
		{"low-disk", m.lowDisk},
		{"vmm", func() { m.Space(vmmctl.Space{AvailableBytes: 1, ImageBytes: 1}) }},
	} {
		available, image := uint64(9-i)<<30, uint64(2<<30)+uint64(i)*(100<<20)
		figures.set(available, image)
		event.cause()
		f.settled(m, i+2)
		want(i+1, available, image)
		if !strings.Contains(f.logFile("fevd.log"), "why="+event.why) {
			t.Errorf("no budget named for %s:\n%s", event.why, f.logFile("fevd.log"))
		}
	}
	time.Sleep(300 * time.Millisecond)
	f.settled(m, 5)
	if st := f.d.hub.Status(); len(st.Warnings) != 0 {
		t.Errorf("warnings with 6 GiB free and a margin of 1: %q", st.Warnings)
	}
}

func TestABurstOfEventsIsOneBudgetMoreNotOneEach(t *testing.T) {
	f, m, figures, _ := budgeted(t)
	f.settled(m, 1)
	for i := 0; i < 50; i++ {
		figures.set(uint64(9<<30)-uint64(i)<<20, 2<<30)
		m.Trimmed()
	}
	deadline := time.Now().Add(10 * time.Second)
	for !m.budgetIdle() || len(f.baselines()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the burst's budgets never ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	got := f.baselines()
	if len(got) > 12 {
		t.Errorf("50 events caused %d budgets", len(got)-1)
	}
	if last := got[len(got)-1]; last != [2]uint64{9<<30 - 49<<20, 2 << 30} {
		t.Errorf("the last budget was made from %v, not from the last figures", last)
	}
}

func TestABudgetDoesNotResumeAPausedVMAndTheWakeSetsOne(t *testing.T) {
	f, m, figures, src := budgeted(t)
	f.settled(m, 1)
	if slept := send[api.PowerSleep](t, src, power.WillSleep); !slept.Paused {
		t.Fatalf("the VM was not paused: %+v", slept)
	}
	figures.set(4<<30, 2<<30)
	m.lowDisk()
	m.Space(vmmctl.Space{AvailableBytes: 4 << 30, ImageBytes: 2 << 30})
	m.Trimmed()
	time.Sleep(300 * time.Millisecond)
	if st := f.d.hub.Status(); st.State != api.StatePaused {
		t.Fatalf("a budget resumed the VM: %+v", st)
	}
	if n := len(f.budgets()); n != 1 {
		t.Fatalf("%d budgets reached a paused guest", n-1)
	}
	if n := len(f.baselines()); n != 1 {
		t.Fatalf("%d baselines reached a paused VMM", n-1)
	}
	figures.set(3<<30, 2<<30)
	if woke := send[api.PowerWake](t, src, power.HasPoweredOn); !woke.Resumed {
		t.Fatalf("the VM was not resumed: %+v", woke)
	}
	f.settled(m, 2)
	if got := f.baselines()[1]; got != [2]uint64{3 << 30, 2 << 30} {
		t.Errorf("the wake's budget was made from %v", got)
	}
	if !strings.Contains(f.logFile("fevd.log"), "why=wake") {
		t.Errorf("no budget named for the wake:\n%s", f.logFile("fevd.log"))
	}
}

func TestTheLowDiskWarningSaysWhatContainersAreTold(t *testing.T) {
	f, m, figures, _ := budgeted(t)
	f.settled(m, 1)
	low := func() string {
		for _, w := range f.d.hub.Status().Warnings {
			if strings.Contains(w, "nearly full") {
				return w
			}
		}
		return ""
	}
	figures.set(512<<20, guestUsed)
	m.lowDisk()
	f.settled(m, 2)
	got := low()
	for _, want := range []string{"0.5 GiB free", filepath.Dir(m.Path), "leaves 1.0 GiB", "told their disk is full", `"No space left on device"`, "nothing goes read-only"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not say %q: %q", want, got)
		}
	}
	if strings.Contains(got, "the data disk goes read-only") {
		t.Errorf("the warning still says what no longer happens: %q", got)
	}
	for _, want := range []string{"docker volume rm", "docker rmi"} {
		if !strings.Contains(got, want) {
			t.Errorf("with nothing left the warning does not name %q, which works there: %q", want, got)
		}
	}
	if strings.Contains(got, "(docker system prune)") {
		t.Errorf("with nothing left the warning advises docker system prune, which fails there: %q", got)
	}
	if b := f.budgets()[1]; b != 0 {
		t.Errorf("the guest was given %d bytes with the Mac under its margin", b)
	}
	figures.set(900<<20, guestUsed+1<<30)
	m.lowDisk()
	f.settled(m, 3)
	if b := f.budgets()[2]; b != 900<<20+guestReserved-1<<30 {
		t.Errorf("under the margin with 1 GiB more in the image than in use and 256 MiB reserved: a budget of %d", b)
	}
	if got := low(); !strings.Contains(got, "Containers can write 0.1 GiB more") || !strings.Contains(got, "(docker system prune)") {
		t.Errorf("a low disk with a budget left: %q", got)
	}
	figures.set(8<<30, 2<<30)
	m.Trimmed()
	f.settled(m, 4)
	if got := low(); got != "" {
		t.Errorf("the warning outlived the low disk: %q", got)
	}
}

func TestHeldWritesAreAWarningUntilTheyAreDone(t *testing.T) {
	f, m, figures, _ := budgeted(t, "space-ms=400", "space-held=3", "space-available=33554432")
	figures.set(32<<20, 2<<30)
	held := func() string {
		for _, w := range f.d.hub.Status().Warnings {
			if strings.Contains(w, "waiting for room") {
				return w
			}
		}
		return ""
	}
	seen := ""
	deadline := time.Now().Add(10 * time.Second)
	for len(f.baselines()) < 2 || !m.budgetIdle() || held() != "" {
		if w := held(); w != "" {
			seen = w
		}
		if time.Now().After(deadline) {
			t.Fatalf("held writes never came and went: baselines %v, warning %q\n%s", f.baselines(), held(), f.logFile("vmm.log"))
		}
		time.Sleep(time.Millisecond)
	}
	if seen != "" {
		for _, want := range []string{"32 MiB free", "stalled, not failed", "free some on the Mac"} {
			if !strings.Contains(seen, want) {
				t.Errorf("the warning does not say %q: %q", want, seen)
			}
		}
	}
	log := f.logFile("fevd.log")
	if !strings.Contains(log, "the VMM holds the guest's writes") || !strings.Contains(log, "waited for room on the Mac are done") {
		t.Errorf("the daemon's log does not tell of the hold and its end:\n%s", log)
	}
	if !strings.Contains(f.logFile("vmm.log"), "; 3 held") {
		t.Errorf("the budget that followed did not find the writes held:\n%s", f.logFile("vmm.log"))
	}

	m.Space(vmmctl.Space{AvailableBytes: 1 << 20, ImageBytes: 1 << 30, HeldRequests: 7})
	if got := held(); !strings.Contains(got, "1 MiB free") {
		t.Fatalf("no warning for writes held: %q", f.d.hub.Status().Warnings)
	}
	m.noteHeld(7, 1<<20)
	if _, err := f.client().VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	if got := held(); got != "" {
		t.Errorf("the warning outlived the VM: %q", got)
	}
}

func TestFiguresThatCannotBeReadSendNoBudget(t *testing.T) {
	f, m, _, _ := budgeted(t)
	f.settled(m, 1)
	bad := func(string) (uint64, error) { return 0, fmt.Errorf("no such volume") }
	m.free = bad
	m.Trimmed()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(f.logFile("fevd.log"), "the Mac's free space cannot be read") {
		if time.Now().After(deadline) {
			t.Fatalf("a volume that cannot be read was not reported:\n%s", f.logFile("fevd.log"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(f.budgets()); n != 1 {
		t.Errorf("%d budgets were sent from figures that could not be read", n-1)
	}
}

func TestAnImageHoldingMoreThanIsUsedIsTrimmedBeforeTheBudget(t *testing.T) {
	f, m, figures, _ := budgeted(t)
	f.settled(m, 1)
	count := func(what string) int { return strings.Count(f.logFile("guest.log"), what) }
	if d, tr := count("storage: no budget set"), count("storage: trimmed"); d != 1 || tr != 1 {
		t.Fatalf("at the boot: %d budgets deferred and %d trims, want one of each\n%s", d, tr, f.logFile("guest.log"))
	}
	if !strings.Contains(f.logFile("fevd.log"), "trimmed before the budget") {
		t.Errorf("the daemon did not say it trimmed first:\n%s", f.logFile("fevd.log"))
	}
	m.Trimmed()
	f.settled(m, 2)
	if d, tr := count("storage: no budget set"), count("storage: trimmed"); d != 1 || tr != 1 {
		t.Errorf("after a trim's budget: %d deferred and %d trims, want still one of each", d, tr)
	}
	var readings atomic.Int32
	m.blocks = func(string) (uint64, error) {
		if readings.Add(1) >= 2 {
			return guestUsed + 8<<20, nil
		}
		return figures.image.Load(), nil
	}
	m.Quiet()
	f.settled(m, 3)
	if n := readings.Load(); n != 2 {
		t.Errorf("the Mac was read %d times for a budget made after a trim, want before it and after it", n)
	}
	if got := f.baselines()[2]; got != [2]uint64{10 << 30, guestUsed + 8<<20} {
		t.Errorf("the budget after the trim was made from %v, want the figures read after it", got)
	}
	m.Quiet()
	f.settled(m, 4)
	if d, tr := count("storage: no budget set"), count("storage: trimmed"); d != 2 || tr != 2 {
		t.Errorf("with 8 MiB more in the image than in use: %d deferred and %d trims in all, want two of each", d, tr)
	}
}

func TestAGuestThatRefusesTheBudgetIsAWarningUntilItTakesOne(t *testing.T) {
	f, m, _, _ := budgeted(t, "budget-fail=2-3")
	f.settled(m, 1)
	refused := func() string {
		for _, w := range f.d.hub.Status().Warnings {
			if strings.Contains(w, "free space") && strings.Contains(w, "not") {
				return w
			}
		}
		return ""
	}
	if got := refused(); got != "" {
		t.Fatalf("a warning before any budget was refused: %q", got)
	}
	wait := func(want bool, what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for (refused() != "") != want || !m.budgetIdle() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: warnings %q\n%s", what, f.d.hub.Status().Warnings, f.logFile("fevd.log"))
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	m.Trimmed()
	wait(true, "no warning after the guest refused a budget")
	got := refused()
	for _, want := range []string{"quota group", "no limit", "whole size", "no such file"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not say %q: %q", want, got)
		}
	}
	m.Trimmed()
	time.Sleep(100 * time.Millisecond)
	wait(true, "the warning went while budgets were still refused")
	if n := len(f.baselines()); n != 1 {
		t.Errorf("%d baselines; the VMM is not to measure from a budget the guest did not take", n)
	}
	m.Trimmed()
	wait(false, "the warning outlived a budget the guest took")
	if n := len(f.budgets()); n != 2 {
		t.Errorf("%d budgets set by the guest, want the boot's and the fourth", n)
	}
}

func TestTheStandInsFiguresAreReadFromAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "space")
	if err := os.WriteFile(path, []byte("123456 789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	free, blocks := fakeFigures(path)
	if a, err := free(""); a != 123456 || err != nil {
		t.Errorf("available: %d, %v", a, err)
	}
	if b, err := blocks(""); b != 789 || err != nil {
		t.Errorf("image: %d, %v", b, err)
	}
	os.WriteFile(path, []byte("x"), 0o600)
	if _, err := free(""); err == nil {
		t.Error("a file that is not two figures was read")
	}
}

func TestTheLowDiskWatchBlocksAndStops(t *testing.T) {
	var calls atomic.Int32
	w, err := watchLowDisk(func() { calls.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	done := make(chan struct{})
	go func() { w.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not stop")
	}
	if n := calls.Load(); n > 3 {
		t.Errorf("%d low-disk events in 200 ms", n)
	}
}

func TestAStopWithWritesHeldDoesNotWaitTheGuestsWholeDeadline(t *testing.T) {
	hold := func(t *testing.T) *fixture {
		f := newFixture(t, []string{"ignore-shutdown=1"})
		f.opts.Timing.HeldStopBound = 250 * time.Millisecond
		m := &DataImage{Path: f.layout.DataImage(), Margin: 1 << 30}
		m.free = func(string) (uint64, error) { return 10 << 30, nil }
		m.blocks = func(string) (uint64, error) { return 2 << 30, nil }
		f.launch.Process.DataImage = m.Path
		f.opts.Modules = []Module{m}
		f.start()
		if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
			t.Fatal(err)
		}
		f.running()
		deadline := time.Now().Add(10 * time.Second)
		for len(f.baselines()) < 1 || !m.budgetIdle() {
			if time.Now().After(deadline) {
				t.Fatalf("the boot's budget never settled")
			}
			time.Sleep(time.Millisecond)
		}
		m.noteHeld(7, 1<<20)
		return f
	}
	t.Run("no timeout named", func(t *testing.T) {
		f := hold(t)
		begun := time.Now()
		st, err := f.client().VMStop(bg, api.StopParams{})
		took := time.Since(begun)
		if err != nil || st.State != api.StateStopped {
			t.Fatalf("stop: %+v %v", st, err)
		}
		if took < 250*time.Millisecond || took > 2*time.Second {
			t.Errorf("the stop took %v with writes held and a bound of 250 ms", took)
		}
		if !strings.Contains(f.logFile("fevd.log"), "writes are held") {
			t.Errorf("the daemon's log does not say why the stop was short:\n%s", f.logFile("fevd.log"))
		}
	})
	t.Run("a timeout named is kept", func(t *testing.T) {
		f := hold(t)
		begun := time.Now()
		st, err := f.client().VMStop(bg, api.StopParams{TimeoutMS: 900})
		took := time.Since(begun)
		if err != nil || st.State != api.StateStopped {
			t.Fatalf("stop: %+v %v", st, err)
		}
		if took < 900*time.Millisecond {
			t.Errorf("the stop took %v with a timeout of 900 ms named", took)
		}
	})
}

func TestTheLastBudgetIsInTheStatus(t *testing.T) {
	f, m, figures, _ := budgeted(t)
	f.settled(m, 1)
	budget := func() *api.DataBudget {
		t.Helper()
		st, err := f.client().Status(bg)
		if err != nil {
			t.Fatal(err)
		}
		if st.Data == nil {
			return nil
		}
		return st.Data.Budget
	}
	check := func(b *api.DataBudget, set int64) {
		t.Helper()
		if b == nil || !b.Limited || b.BudgetBytes != set || b.LimitBytes != b.ReferencedBytes+b.BudgetBytes || b.ReferencedBytes <= 0 {
			t.Fatalf("the status's budget is %+v, the guest set %d", b, set)
		}
		if _, err := time.Parse(time.RFC3339, b.At); err != nil {
			t.Errorf("at: %v", err)
		}
	}
	check(budget(), f.budgets()[0])
	seq := f.d.hub.Status().Seq
	figures.set(8<<30, 2<<30)
	m.Quiet()
	f.settled(m, 2)
	check(budget(), f.budgets()[1])
	if f.d.hub.Status().Seq <= seq {
		t.Error("a new budget was no transition")
	}
	if _, err := f.client().VMStop(bg, api.StopParams{TimeoutMS: 5000}); err != nil {
		t.Fatal(err)
	}
	if b := budget(); b != nil {
		t.Errorf("a stopped VM's status has the budget %+v", b)
	}
	figures.set(7<<30, 2<<30)
	sub, _ := f.d.hub.Subscribe(func() {})
	defer sub.Close()
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	f.settled(m, 3)
	check(budget(), f.budgets()[2])
	if f.budgets()[2] == f.budgets()[1] {
		t.Fatal("the two boots' budgets are the same; the test tells them apart by their sizes")
	}
	stop := make(chan struct{})
	close(stop)
	for {
		event, ok := sub.Next(stop)
		if !ok {
			break
		}
		if st, isStatus := event.(api.Status); isStatus && st.State != api.StateStopped && st.State != api.StateStopping && st.Data != nil && st.Data.Budget.BudgetBytes == f.budgets()[1] {
			t.Errorf("the last boot's budget was shown in boot %s: %+v", st.BootID, st.Data.Budget)
		}
	}
}
