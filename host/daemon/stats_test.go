package daemon

import (
	"strings"
	"testing"
	"time"

	"fev/host/power"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func take(t *testing.T, samples chan api.StatsSample, what string) api.StatsSample {
	t.Helper()
	select {
	case s := <-samples:
		return s
	case <-time.After(5 * time.Second):
		t.Fatalf("no sample: %s", what)
		return api.StatsSample{}
	}
}

func TestTheStatsStreamCarriesTheVMMsAccountAndTheGuests(t *testing.T) {
	f := started(t)
	c := f.client()
	samples := make(chan api.StatsSample, 64)
	began := time.Now()
	stop, err := c.SubscribeStats(bg, 250, func(s api.StatsSample) { samples <- s })
	if err != nil {
		t.Fatal(err)
	}
	first := take(t, samples, "the first, with no VM")
	if first.State != api.StateStopped || first.VMM != nil || first.Guest != nil || first.Time == "" || time.Since(began) > 200*time.Millisecond {
		t.Fatalf("the first sample of a stopped VM, %v after the call: %+v", time.Since(began), first)
	}
	if second := take(t, samples, "the second, with no VM"); second.State != api.StateStopped || time.Since(began) < 200*time.Millisecond {
		t.Fatalf("the second sample %v after the call: %+v", time.Since(began), second)
	}

	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	f.running()
	var prev *api.StatsSample
	for n := 0; n < 3; {
		s := take(t, samples, "of a running VM")
		if s.State != api.StateRunning || s.Guest == nil {
			continue
		}
		if s.VMM == nil || s.VMM.GuestBytes != 8192<<20 {
			t.Fatalf("the VMM's account: %+v", s.VMM)
		}
		g := s.Guest
		if len(g.Groups) == 0 || g.Groups[0].Kind != api.StatsSystem || g.Groups[0].Name != "guest" || g.MemTotalKB <= 0 || g.MemAvailableKB > g.MemTotalKB {
			t.Fatalf("the guest's account: %+v", g)
		}
		if prev != nil {
			if g.Groups[0].CPUUsec < prev.Guest.Groups[0].CPUUsec || s.Time <= prev.Time {
				t.Errorf("a counter went back: %+v after %+v", g.Groups[0], prev.Guest.Groups[0])
			}
		}
		prev = &s
		n++
	}
	stop()
	time.Sleep(100 * time.Millisecond)
	for len(samples) > 0 {
		<-samples
	}
	time.Sleep(600 * time.Millisecond)
	if n := len(samples); n != 0 {
		t.Fatalf("%d samples after the stream was ended", n)
	}
}

func TestARunningVMsFirstSampleWaitsForTheGuestsFirst(t *testing.T) {
	f := started(t, []string{"stats-first-ms=300"})
	c := f.client()
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	f.running()
	samples := make(chan api.StatsSample, 64)
	if _, err := c.SubscribeStats(bg, 250, func(s api.StatsSample) { samples <- s }); err != nil {
		t.Fatal(err)
	}
	if s := take(t, samples, "the first of a running VM"); s.State != api.StateRunning || s.Guest == nil || s.VMM == nil {
		t.Fatalf("the first sample of a running VM whose guest is slow to begin: %+v", s)
	}
}

func TestAPausedVMsSampleHasTheVMMsAccountAlone(t *testing.T) {
	f, src := powered(t, nil)
	f.running()
	samples := make(chan api.StatsSample, 64)
	if _, err := f.client().SubscribeStats(bg, 250, func(s api.StatsSample) { samples <- s }); err != nil {
		t.Fatal(err)
	}
	if s := take(t, samples, "running"); s.Guest == nil || s.VMM == nil {
		t.Fatalf("the first sample of a running VM: %+v", s)
	}
	send[api.PowerSleep](t, src, power.WillSleep)
	seen := 0
	for seen < 2 {
		s := take(t, samples, "paused")
		if s.State != api.StatePaused {
			continue
		}
		if s.Guest != nil || s.VMM == nil {
			t.Fatalf("a paused VM's sample: %+v", s)
		}
		seen++
	}
	late := make(chan api.StatsSample, 64)
	began := time.Now()
	if _, err := f.client().SubscribeStats(bg, 250, func(s api.StatsSample) { late <- s }); err != nil {
		t.Fatal(err)
	}
	if s := take(t, late, "subscribed while paused"); s.State != api.StatePaused || s.Guest != nil || s.VMM == nil || time.Since(began) > 200*time.Millisecond {
		t.Fatalf("subscribed while paused, %v after: %+v", time.Since(began), s)
	}
	send[api.PowerWake](t, src, power.HasPoweredOn)
	for _, ch := range []chan api.StatsSample{samples, late} {
		for {
			if s := take(t, ch, "after the wake"); s.State == api.StateRunning && s.Guest != nil {
				break
			}
		}
	}
}

func TestTheGuestsStreamLastsAsLongAsAClientsDoes(t *testing.T) {
	f := started(t)
	c := f.client()
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	f.running()
	guestStreams := func() int {
		return count(f.logFile("guest.log"), "stats: stream ") - count(f.logFile("guest.log"), "stats: stream ended")*2
	}
	until := func(want int, when string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for guestStreams() != want {
			if time.Now().After(deadline) {
				t.Fatalf("%s: the guest feeds %d streams, want %d\n%s", when, guestStreams(), want, f.logFile("guest.log"))
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	until(0, "nobody subscribed")
	samples := make(chan api.StatsSample, 64)
	stop, err := c.SubscribeStats(bg, 250, func(s api.StatsSample) { samples <- s })
	if err != nil {
		t.Fatal(err)
	}
	take(t, samples, "running")
	until(1, "one subscriber")
	other := f.client()
	if _, err := other.SubscribeStats(bg, 500, func(api.StatsSample) {}); err != nil {
		t.Fatal(err)
	}
	until(2, "two subscribers")
	other.Close()
	until(1, "one connection gone")

	if _, err := c.VMRestart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	boot := f.running().BootID
	for {
		if s := take(t, samples, "after the restart"); s.State == api.StateRunning && s.Guest != nil {
			break
		}
	}
	if st := f.d.hub.Status(); st.BootID != boot {
		t.Fatalf("%+v", st)
	}
	stop()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(lastLine(f.logFile("guest.log"), "stats: stream"), "stream ended") {
		if time.Now().After(deadline) {
			t.Fatalf("the guest still feeds a stream nobody holds:\n%s", f.logFile("guest.log"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func lastLine(text, containing string) string {
	last := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, containing) {
			last = line
		}
	}
	return last
}

func TestAStatsSubscriptionsIntervalIsChecked(t *testing.T) {
	f := started(t)
	c := f.raw()
	for _, ms := range []int{1, api.MinStatsIntervalMS - 1, -5, 3600*1000 + 1} {
		if err := c.Call(bg, api.MethodStatsSubscribe, api.StatsSubscribeParams{IntervalMS: ms}, nil); code(err) != jsonrpc.CodeInvalidParams {
			t.Errorf("an interval of %d ms: %v", ms, err)
		}
	}
	var sub api.Subscription
	if err := c.Call(bg, api.MethodStatsSubscribe, api.StatsSubscribeParams{}, &sub); err != nil || sub.Subscription == 0 {
		t.Fatalf("the default interval: %+v, %v", sub, err)
	}
	if err := f.raw().Call(bg, api.MethodStatsUnsubscribe, sub, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(bg, api.MethodStatsUnsubscribe, sub, nil); err != nil {
		t.Fatal(err)
	}
}
