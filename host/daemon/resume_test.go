package daemon

import (
	"testing"

	"fev/proto/agent"
	api "fev/proto/daemon"
)

func TestAResumeLeavesAMachineBeingMadeToItsCreate(t *testing.T) {
	release := make(chan struct{})
	f := machinesDaemon(t, []string{"starting-ms=1500"}, func(m *Machines) {
		m.BeforeResume = func(string) { <-release }
	})
	op, err := f.c.CreateMachine(bg, api.MachinesCreateParams{Name: "iso", Distro: "ubuntu", Isolated: true})
	if err != nil {
		t.Fatal(err)
	}
	f.machineIs("iso", api.MachineStarting)
	close(release)
	if end := f.ended(op.ID); end.State != api.OperationDone {
		t.Fatalf("the isolated machine's create: %+v", end)
	}
	l, err := f.c.Machines(bg)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range l.Machines {
		if m.Name == "iso" && !m.Isolated {
			t.Errorf("iso is shown as a trusted machine after its create: %+v", m)
		}
	}
	if _, exit, err := f.run(f.c, api.SessionStartParams{Machine: "iso", Copy: agent.CopyOut, Argv: []string{"./made"}}); err != nil || exit.ExitCode != 0 {
		t.Errorf("a copy out of iso: %v %+v", err, exit)
	}
}
