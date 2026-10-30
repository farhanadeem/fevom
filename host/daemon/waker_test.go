package daemon

import (
	"testing"
	"time"

	api "fev/proto/daemon"
)

func TestEachWakeOfTheMacReadsItsNetworkAgain(t *testing.T) {
	f, _, _, mac, _ := direct(t, "")
	refreshes := func() int {
		mac.mu.Lock()
		defer mac.mu.Unlock()
		return mac.refreshes
	}
	waitFor := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for refreshes() < want && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := refreshes(); got < want {
			t.Fatalf("%d refreshes, want %d", got, want)
		}
	}
	before := refreshes()
	f.d.wake("")
	waitFor(before + 1)
	f.d.wake(api.WakeNotSleep)
	waitFor(before + 2)
	f.d.wake(api.WakeCommand)
	time.Sleep(50 * time.Millisecond)
	if got := refreshes(); got != before+2 {
		t.Errorf("a command's resume read the Mac's network again: %d refreshes, want %d", got, before+2)
	}
}
