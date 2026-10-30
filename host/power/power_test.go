package power

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTheShimRegistersOnceAndIsSilentWhileTheMacIsAwake(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	src, err := Listen()
	if err == ErrUnsupported {
		t.Skip("built without cgo")
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(); err == nil {
		t.Error("a second registration was accepted while the first was open")
	}
	select {
	case e := <-src.Events():
		t.Errorf("an event with the Mac awake: %v", e.Kind)
	case <-time.After(50 * time.Millisecond):
	}
	src.Close()
	again, err := Listen()
	if err != nil {
		t.Fatalf("registering again after a close: %v", err)
	}
	again.Close()
}

func TestTheCallbackDeliversOrAnswersItself(t *testing.T) {
	src, err := Listen()
	if err != nil {
		t.Skip(err)
	}
	defer src.Close()
	deliver(1, 0)
	deliver(2, 0)
	sleep, wake := <-src.Events(), <-src.Events()
	if sleep.Kind != WillSleep || sleep.Allow == nil {
		t.Errorf("the first event is %+v", sleep)
	}
	if wake.Kind != HasPoweredOn || wake.Allow != nil {
		t.Errorf("the second event is %+v", wake)
	}
	sleep.Ack()
	sleep.Ack()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			deliver(1, 0)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the callback waited for a listener that was not reading")
	}
}

func TestClosingAnswersAWillSleepNobodyTook(t *testing.T) {
	src, err := Listen()
	if err != nil {
		t.Skip(err)
	}
	acked := 0
	old := allow
	allow = func(int64) { acked++ }
	defer func() { allow = old }()
	deliver(1, 41)
	deliver(1, 42)
	src.Close()
	if acked != 2 {
		t.Errorf("%d of 2 queued will-sleeps were answered at close", acked)
	}
	select {
	case e := <-src.Events():
		t.Errorf("an event was left in the queue: %v", e.Kind)
	default:
	}
}

func TestAsleepStandsStillWhileTheMacIsAwake(t *testing.T) {
	a := Asleep()
	time.Sleep(20 * time.Millisecond)
	b := Asleep()
	if a < 0 || b-a > 5*time.Millisecond || a-b > 5*time.Millisecond {
		t.Errorf("asleep for %v, then %v 20 ms later with the Mac awake", a, b)
	}
}

func TestAnInjectedEventReturnsItsOutcome(t *testing.T) {
	src := NewInjected()
	go func() {
		e := <-src.Events()
		e.Ack()
		e.Handled(e.Kind.String())
	}()
	if got := src.Send(WillSleep); got != "will sleep" {
		t.Errorf("the sender was told %v", got)
	}
}

func TestAnAssertionIsHeldUntilReleased(t *testing.T) {
	const reason = "fev test: an assertion is held until released"
	release, err := Assert(reason, false)
	if err != nil {
		t.Skip(err)
	}
	listed := func() bool {
		out, err := exec.Command("pmset", "-g", "assertions").Output()
		if err != nil {
			t.Skip(err)
		}
		return strings.Contains(string(out), reason)
	}
	if !listed() {
		t.Error("the assertion is not in `pmset -g assertions` while held")
	}
	release()
	release()
	if listed() {
		t.Error("the assertion is still listed after its release")
	}
}
