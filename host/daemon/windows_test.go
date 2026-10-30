package daemon

import (
	"testing"
	"time"

	api "fev/proto/daemon"
)

func TestTheStatusCarriesTheDataDisksWindowWhileTheVMRuns(t *testing.T) {
	for name, tc := range map[string]struct {
		script string
		want   api.Window
	}{
		"on":     {"window=on", api.Window{State: api.WindowOn, SizeBytes: 256 << 30}},
		"off":    {"window=off:FEV_VMM_WINDOW=off", api.Window{State: api.WindowOff, Reason: "FEV_VMM_WINDOW=off"}},
		"failed": {"window=failed:mmap: Cannot allocate memory", api.Window{State: api.WindowFailed, Reason: "mmap: Cannot allocate memory"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := started(t, []string{tc.script})
			c := f.client()
			h := f.watch(c)
			if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
				t.Fatal(err)
			}
			st := f.until("running with its window", func(st api.Status) bool {
				return st.State == api.StateRunning && st.Windows != nil
			})
			if st.Windows.DataDisk == nil || *st.Windows.DataDisk != tc.want || st.Windows.FileShare != nil {
				t.Errorf("windows %+v (data disk %+v), want the data disk's %+v and no other", st.Windows, st.Windows.DataDisk, tc.want)
			}
			if got, err := c.Status(bg); err != nil || got.Windows == nil || *got.Windows.DataDisk != tc.want {
				t.Errorf("daemon.status: %+v %v", got, err)
			}
			if _, err := c.VMStop(bg, api.StopParams{}); err != nil {
				t.Fatal(err)
			}
			end := f.stopped()
			if end.Windows != nil {
				t.Errorf("a stopped VM still has windows: %+v", end.Windows.DataDisk)
			}
			h.states(t, end.Seq)
			h.mu.Lock()
			defer h.mu.Unlock()
			carried := 0
			for _, st := range h.seen {
				if st.Windows == nil {
					continue
				}
				carried++
				if st.State != api.StateRunning && st.State != api.StatePaused {
					t.Errorf("a status in state %q carries windows", st.State)
				}
			}
			if carried == 0 {
				t.Errorf("no subscriber was sent a status with the windows")
			}
		})
	}
}

func TestAVMWithoutADataDiskHasNoWindowInItsStatus(t *testing.T) {
	f := started(t)
	c := f.client()
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	st := f.running()
	time.Sleep(50 * time.Millisecond)
	if got := f.d.hub.Status(); got.Windows != nil || got.Seq != st.Seq {
		t.Errorf("status %+v after %+v", got, st)
	}
}
