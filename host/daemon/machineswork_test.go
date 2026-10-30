package daemon

import (
	"sync/atomic"
	"testing"
	"time"

	api "fev/proto/daemon"
)

type workEnds struct{ n atomic.Int32 }

func (w *workEnds) Name() string                   { return "work-ends" }
func (w *workEnds) Marks() []string                { return nil }
func (w *workEnds) Start(Host) error               { return nil }
func (w *workEnds) StateChanged(api.Status)        {}
func (w *workEnds) ConfigChanged(api.ConfigChange) {}
func (w *workEnds) Close()                         {}
func (w *workEnds) WorkEnded()                     { w.n.Add(1) }

func (w *workEnds) past(t *testing.T, n int32, what string) int32 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for w.n.Load() <= n {
		if time.Now().After(deadline) {
			t.Fatalf("%s: work was not said to have ended", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return w.n.Load()
}

func TestAMachinesCreateStopAndDeleteEndWork(t *testing.T) {
	w := &workEnds{}
	f := machinesDaemon(t, func(f *fixture) { f.opts.Modules = append(f.opts.Modules, w) })
	f.create("dev", "ubuntu")
	n := w.past(t, 0, "the create")
	f.machineIs("dev", api.MachineRunning)
	if _, err := f.c.StopMachine(bg, api.MachinesStopParams{Name: "dev"}); err != nil {
		t.Fatal(err)
	}
	n = w.past(t, n, "the stop")
	if _, err := f.c.StartMachine(bg, "dev"); err != nil {
		t.Fatal(err)
	}
	f.machineIs("dev", api.MachineRunning)
	time.Sleep(200 * time.Millisecond)
	n = w.n.Load()
	if err := f.c.DeleteMachine(bg, api.MachinesStopParams{Name: "dev"}); err != nil {
		t.Fatal(err)
	}
	w.past(t, n, "the delete")
}
