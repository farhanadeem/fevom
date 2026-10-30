package main

import (
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNoLockedGoroutineRunsOnTheLeadingThread(t *testing.T) {
	leader := os.Getpid()
	var onLeader atomic.Int32
	for batch := 0; batch < 40; batch++ {
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				runtime.LockOSThread()
				if unix.Gettid() == leader {
					onLeader.Add(1)
				}
			}()
		}
		wg.Wait()
	}
	if n := onLeader.Load(); n != 0 {
		t.Fatalf("%d of 2000 locked goroutines ran on the leading thread, which the runtime parks and does not end: whatever namespace such a goroutine entered is then pid 1's", n)
	}
	if state := leaderState(t, leader); state == "Z" || state == "X" {
		t.Fatalf("the leading thread has ended (state %s)", state)
	}
}

func leaderState(t *testing.T, pid int) string {
	t.Helper()
	b, err := os.ReadFile("/proc/self/task/" + itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == ')' && i+2 < len(b) {
			return string(b[i+2 : i+3])
		}
	}
	t.Fatalf("unreadable stat: %q", b)
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}
