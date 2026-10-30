package cli

import (
	"bytes"
	"strings"
	"testing"

	api "fev/proto/daemon"
)

func TestEmulationSelectionsAreShownInStatusAndInfo(t *testing.T) {
	var w bytes.Buffer
	statusText(&w, &api.Status{State: api.StateRunning, Emulation: &api.EmulationStatus{
		State: api.EmulationOn, Mode: "auto", Translator: "Rosetta", Reason: "installed Rosetta lacks x86 seccomp",
		HardwareOrdering: true, EnginePending: true,
		Workloads: []api.EmulationWorkload{{ID: "c1", Kind: "container", Name: "web", Engine: "fex", Reason: "nix: installed Rosetta cannot enforce x86 seccomp filters"}},
	}})
	for _, want := range []string{
		"x86 emulation: on: automatic Rosetta/FEX selection per workload; i386 through FEX-Emu; installed Rosetta lacks x86 seccomp, with the CPU's memory ordering; emulation.engine's new value takes effect when the VM next starts (\"fev restart\")\n",
		"  container web: fex: nix: installed Rosetta cannot enforce x86 seccomp filters\n",
	} {
		if !strings.Contains(w.String(), want) {
			t.Errorf("status has no %q:\n%s", want, w.String())
		}
	}
	for _, tc := range []struct{ engine, reason, want string }{
		{"rosetta", "automatic selection: no known Rosetta incompatibility", "emulation: rosetta: automatic selection: no known Rosetta incompatibility\n"},
		{"fex", "", "emulation: fex\n"},
	} {
		w.Reset()
		machineText(&w, &api.Machine{Name: "x", State: api.MachineRunning, Image: api.Image{Arch: "amd64"}, Emulation: tc.engine, EmulationReason: tc.reason})
		if !strings.Contains(w.String(), tc.want) {
			t.Errorf("info has no %q:\n%s", tc.want, w.String())
		}
	}
	w.Reset()
	machineText(&w, &api.Machine{Name: "a", State: api.MachineRunning, Image: api.Image{Arch: "arm64"}})
	if strings.Contains(w.String(), "emulation") {
		t.Errorf("an arm64 machine with an emulation line:\n%s", w.String())
	}
}
