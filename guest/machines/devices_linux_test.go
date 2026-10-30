package machines

import (
	"os"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

func TestTheDeviceProgramLoadsUnderSignals(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("loading a BPF program needs root (tests/run unit guest runs this privileged)")
	}
	load := func(n int) (lost int, first error) {
		var stop atomic.Bool
		done := make(chan struct{})
		go func() {
			defer close(done)
			for !stop.Load() {
				unix.Kill(os.Getpid(), unix.SIGURG)
			}
		}()
		defer func() { stop.Store(true); <-done }()
		for i := 0; i < n; i++ {
			fd, err := loadDeviceProgram([]uint32{254, 259})
			if err != nil {
				lost++
				if first == nil {
					first = err
				}
				continue
			}
			unix.Close(fd)
		}
		return lost, first
	}
	saved := progLoadTries
	progLoadTries = 1
	once, first := load(2000)
	progLoadTries = saved
	if once != 0 {
		t.Errorf("one try each, the thread's signals blocked: %d of 2000 loads failed; the first: %v", once, first)
	}
	if lost, first := load(2000); lost != 0 {
		t.Errorf("%d of 2000 loads failed with %d tries each; the first: %v", lost, progLoadTries, first)
	}
}
