package config

import (
	"testing"

	api "fev/proto/daemon"
)

func TestTheEmulationKeyWaitsForTheEnginesNextStart(t *testing.T) {
	s, _ := fresh(t)
	if !s.Bool(api.KeyEmulationX86) {
		t.Fatal("emulation.x86 is off by default")
	}
	changed, err := s.Set(map[string]any{api.KeyEmulationX86: false})
	if err != nil || len(changed) != 1 || changed[0].Pending || changed[0].Apply != api.ApplyEngineRestart {
		t.Fatalf("with no VM: %+v %v", changed, err)
	}
	s.VMStarted()
	changed, _ = s.Set(map[string]any{api.KeyEmulationX86: true})
	if len(changed) != 1 || !changed[0].Pending {
		t.Fatalf("while a VM runs with false: %+v", changed)
	}
	if got := s.PendingRestart(); len(got) != 0 {
		t.Errorf("listed as waiting for the VM's restart: %v", got)
	}
	if v, _ := s.Get(api.KeyEmulationX86); !v[api.KeyEmulationX86].Pending {
		t.Errorf("get: %+v", v)
	}
	s.SetInEffect(api.KeyEmulationX86, true)
	if v, _ := s.Get(api.KeyEmulationX86); v[api.KeyEmulationX86].Pending {
		t.Errorf("pending after the engine started with it: %+v", v)
	}
	s.SetInEffect(api.KeyMemoryMiB, int64(1))
	if v, ok := s.InEffect(api.KeyMemoryMiB); !ok || v == int64(1) {
		t.Errorf("a VM's key set in effect: %v", v)
	}
	s.VMStopped()
	s.SetInEffect(api.KeyEmulationX86, false)
	if v, _ := s.Get(api.KeyEmulationX86); v[api.KeyEmulationX86].Pending {
		t.Errorf("pending with no VM: %+v", v)
	}
}

func TestEmulationEngineSelectionIsValidatedAndWaitsForAVMRestart(t *testing.T) {
	s, _ := fresh(t)
	v, _ := s.Get(api.KeyEmulationEngine)
	if v[api.KeyEmulationEngine].Value != "fex" {
		t.Fatal(v)
	}
	for _, bad := range []any{"", "automatic", "Rosetta", true, "rosetta extra"} {
		if _, err := s.Set(map[string]any{api.KeyEmulationEngine: bad}); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	s.VMStarted()
	change, err := s.Set(map[string]any{api.KeyEmulationEngine: "rosetta"})
	if err != nil || len(change) != 1 || !change[0].Pending || change[0].Apply != api.ApplyVMRestart {
		t.Fatalf("%+v %v", change, err)
	}
	s.SetInEffect(api.KeyEmulationX86, true)
	v, _ = s.Get(api.KeyEmulationEngine)
	if !v[api.KeyEmulationEngine].Pending {
		t.Fatal("engine restart applied the VM's setting")
	}
	s.VMStopped()
	s.VMStarted()
	if value, _ := s.InEffect(api.KeyEmulationEngine); value != "rosetta" {
		t.Fatal(value)
	}
	if _, err := s.Set(map[string]any{api.KeyEmulationEngine: "auto"}); err != nil {
		t.Fatal(err)
	}
	s.VMStopped()
	s.VMStarted()
	if value, _ := s.InEffect(api.KeyEmulationEngine); value != "auto" {
		t.Fatal(value)
	}
}
