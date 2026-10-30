package daemon

import (
	"testing"

	api "fev/proto/daemon"
)

func TestAMachinesIPv6AddressIsShownWhileItRuns(t *testing.T) {
	f := machinesDaemon(t)
	dev := f.create("dev", "ubuntu")
	const want = "fd5e:1c2b:9a40:1::2"
	if dev.Address != "198.19.0.2" || dev.Address6 != want {
		t.Fatalf("the created machine: address %q address6 %q, not 198.19.0.2 and %s", dev.Address, dev.Address6, want)
	}
	address6 := func() (listed, info string) {
		t.Helper()
		l, err := f.c.Machines(bg)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range l.Machines {
			if m.Name == "dev" {
				listed = m.Address6
			}
		}
		i, err := f.c.Machine(bg, "dev")
		if err != nil {
			t.Fatal(err)
		}
		return listed, i.Address6
	}
	if l, i := address6(); l != want || i != want {
		t.Fatalf("while it runs: machines.list %q, machines.info %q, not %s", l, i, want)
	}
	if _, err := f.c.StopMachine(bg, api.MachinesStopParams{Name: "dev"}); err != nil {
		t.Fatal(err)
	}
	f.machineIs("dev", api.MachineStopped)
	if l, i := address6(); l != "" || i != "" {
		t.Errorf("stopped: machines.list %q, machines.info %q, not none", l, i)
	}
	m, err := f.c.StartMachine(bg, "dev")
	if err != nil {
		t.Fatal(err)
	}
	f.machineIs("dev", api.MachineRunning)
	if l, i := address6(); l != want || i != want {
		t.Errorf("started again (start said %q): machines.list %q, machines.info %q, not %s", m.Address6, l, i, want)
	}
	if _, err := f.c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	if l, i := address6(); l != "" || i != "" {
		t.Errorf("with the VM stopped: machines.list %q, machines.info %q, not none", l, i)
	}
}
