package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"fev/proto/agent"
	api "fev/proto/daemon"
)

func TestTheTranslatorIsSaidAndItsKeyWaitsForTheEnginesRestart(t *testing.T) {
	f := newFixture(t)
	e := &Emulation{}
	f.launch.Process.BootWords = e.BootWords
	f.opts.Modules = []Module{e}
	f.start()
	f.boot()
	st := f.until("the translator on", func(st api.Status) bool { return st.Emulation != nil })
	if b := st.Emulation; b.State != api.EmulationOn || b.Translator != agent.EmulationTranslator || b.Version != "FEX-2610" || b.Pending || api.CheckEmulationStatus(*b) != nil {
		t.Fatalf("on: %+v", *b)
	}
	c := f.client()
	changed, err := c.ConfigSet(bg, map[string]any{api.KeyEmulationX86: false})
	if err != nil || len(changed) != 1 || !changed[0].Pending || changed[0].Apply != api.ApplyEngineRestart {
		t.Fatalf("set false: %+v %v", changed, err)
	}
	st = f.until("pending", func(st api.Status) bool { return st.Emulation != nil && st.Emulation.Pending })
	if st.Emulation.State != api.EmulationOn || len(st.PendingRestart) != 0 {
		t.Fatalf("pending: %+v, waiting for the VM's restart %v", *st.Emulation, st.PendingRestart)
	}
	vm, ok := e.host.VM()
	if !ok {
		t.Fatal("no VM")
	}
	if err := e.BeforeEngineRestart(context.Background(), vm); err != nil {
		t.Fatal(err)
	}
	st = f.until("off", func(st api.Status) bool { return st.Emulation != nil && st.Emulation.State == api.EmulationOff })
	if st.Emulation.Pending {
		t.Errorf("still pending after the restart applied it: %+v", *st.Emulation)
	}
	if v, _ := c.ConfigGet(bg, api.KeyEmulationX86); v[api.KeyEmulationX86].Pending {
		t.Errorf("config.get says pending: %+v", v)
	}
	if _, err := c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	if st := f.d.hub.Status(); st.Emulation != nil {
		t.Errorf("a stopped VM's block: %+v", *st.Emulation)
	}
	f.boot()
	st = f.until("off from the boot", func(st api.Status) bool { return st.Emulation != nil })
	if st.Emulation.State != api.EmulationOff || st.Emulation.Pending {
		t.Fatalf("the boot given off: %+v", *st.Emulation)
	}
	if words := e.BootWords(); len(words) != 1 || words[0] != "fev.x86=0" {
		t.Errorf("boot words %v", words)
	}
	c.ConfigSet(bg, map[string]any{api.KeyEmulationX86: true})
	if words := e.BootWords(); len(words) != 0 {
		t.Errorf("boot words with x86 on: %v", words)
	}
}

func TestAGuestWithNoTranslatorIsSaidUnavailable(t *testing.T) {
	f := newFixture(t, []string{"emulation=none"})
	e := &Emulation{}
	f.launch.Process.BootWords = e.BootWords
	f.opts.Modules = []Module{e}
	f.start()
	f.boot()
	st := f.until("the translator's block", func(st api.Status) bool { return st.Emulation != nil })
	if b := st.Emulation; b.State != api.EmulationUnavailable || b.Error != "the system image has no x86 translator at "+agent.EmulationPath || api.CheckEmulationStatus(*b) != nil {
		t.Fatalf("%+v", *b)
	}
}

func TestAnotherToolsEntryMatchedFirstIsNotSaidOn(t *testing.T) {
	why := "another tool's entry qemu-x86_64 (interpreter /usr/bin/qemu-x86_64) is matched before FEX-x86_64 for the programs both take"
	g := &agent.EmulationStatus{X86: true, Registered: true, TableEnabled: true, Translator: agent.EmulationTranslator, Version: "FEX-2610", Error: why}
	b := emulationBlock(g)
	if b.State != api.EmulationUnavailable || b.Error != why || api.CheckEmulationStatus(*b) != nil {
		t.Fatalf("%+v", *b)
	}
	g.Error = ""
	if b := emulationBlock(g); b.State != api.EmulationOn || api.CheckEmulationStatus(*b) != nil {
		t.Fatalf("with nothing ahead of fev's: %+v", *b)
	}
}

func TestTheStatusReadsTheGuestsTableWhenAsked(t *testing.T) {
	shadow := filepath.Join(t.TempDir(), "shadow")
	f := newFixture(t, []string{"emulation-shadow-file=" + shadow})
	e := &Emulation{}
	f.launch.Process.BootWords = e.BootWords
	f.opts.Modules = []Module{e}
	f.start()
	f.boot()
	f.until("the translator on", func(st api.Status) bool { return st.Emulation != nil && st.Emulation.State == api.EmulationOn })
	c := f.client()
	if err := os.WriteFile(shadow, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(bg)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Emulation; b == nil || b.State != api.EmulationUnavailable || !strings.Contains(b.Error, "qemu-x86_64") || api.CheckEmulationStatus(*b) != nil {
		t.Fatalf("with another tool's entry registered since the boot: %+v", b)
	}
	os.Remove(shadow)
	if st, err := c.Status(bg); err != nil || st.Emulation == nil || st.Emulation.State != api.EmulationOn {
		t.Fatalf("once it went: %+v %v", st.Emulation, err)
	}
}

func TestRosettaSelectionAppliesOnAVMRestartAndKeepsTheX86Switch(t *testing.T) {
	f := newFixture(t)
	e := &Emulation{}
	f.launch.Process.BootWords = e.BootWords
	f.opts.Modules = []Module{e}
	f.start()
	f.boot()
	f.until("FEX", func(st api.Status) bool {
		return st.Emulation != nil && st.Emulation.Translator == agent.EmulationTranslator
	})
	c := f.client()
	if _, err := c.ConfigSet(bg, map[string]any{api.KeyEmulationEngine: "rosetta"}); err != nil {
		t.Fatal(err)
	}
	if words := e.BootWords(); len(words) != 0 {
		t.Fatalf("changed the boot's captured FEX choice without a restart: %v", words)
	}
	f.until("the engine's value pending in the status", func(st api.Status) bool {
		return st.Emulation != nil && st.Emulation.EnginePending && !st.Emulation.Pending
	})
	vm, _ := e.host.VM()
	if err := e.BeforeEngineRestart(bg, vm); err != nil {
		t.Fatal(err)
	}
	if st := f.d.hub.Status(); st.Emulation.Translator != agent.EmulationTranslator {
		t.Fatal("applied without a VM restart")
	}
	if _, err := c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	f.boot()
	f.until("Rosetta", func(st api.Status) bool {
		return st.Emulation != nil && st.Emulation.Translator == agent.EmulationRosetta && st.Emulation.State == api.EmulationOn && !st.Emulation.EnginePending
	})
	if _, err := c.ConfigSet(bg, map[string]any{api.KeyEmulationX86: false}); err != nil {
		t.Fatal(err)
	}
	vm, _ = e.host.VM()
	if err := e.BeforeEngineRestart(bg, vm); err != nil {
		t.Fatal(err)
	}
	f.until("Rosetta off", func(st api.Status) bool {
		return st.Emulation != nil && st.Emulation.Translator == agent.EmulationRosetta && st.Emulation.State == api.EmulationOff
	})
	if words := e.BootWords(); !slices.Equal(words, []string{agent.EmulationEngineParam + "=rosetta", agent.EmulationCmdlineParam + "=0"}) {
		t.Fatalf("the words with x86 off: %v", words)
	}
}

func TestAutomaticEmulationIsCapturedAtBootAndReportsChoices(t *testing.T) {
	f := newFixture(t)
	e := &Emulation{}
	f.launch.Process.BootWords = e.BootWords
	f.opts.Modules = []Module{e}
	f.start()
	c := f.client()
	if _, err := c.ConfigSet(bg, map[string]any{api.KeyEmulationEngine: "auto"}); err != nil {
		t.Fatal(err)
	}
	f.boot()
	f.until("automatic mode", func(st api.Status) bool {
		return st.Emulation != nil && st.Emulation.Mode == "auto" && st.Emulation.State == api.EmulationOn
	})
	g := &agent.EmulationStatus{Mode: "auto", X86: true, Registered: true, TableEnabled: true, Translator: agent.EmulationRosetta,
		Reason: "missing x86 seccomp", Workloads: []agent.EmulationWorkload{{ID: "one", Kind: "container", Name: "app", Engine: "fex", Reason: "explicit workload override"}}}
	b := emulationBlock(g)
	if b.Mode != "auto" || b.Reason != g.Reason || len(b.Workloads) != 1 || b.Workloads[0].ID != "one" || b.Workloads[0].Engine != "fex" {
		t.Fatalf("%+v", b)
	}
}
