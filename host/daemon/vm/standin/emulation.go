package standin

import (
	"encoding/json"
	"os"
	"strings"
	"sync"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
)

type emulationState struct {
	mu      sync.Mutex
	off     bool
	none    bool
	rosetta bool
	auto    bool
	sets    []bool
	shadow  string
}

func (a *Agent) SetEmulationShadow(path string) {
	a.emu.mu.Lock()
	defer a.emu.mu.Unlock()
	a.emu.shadow = path
}

const standinVersion = "FEX-2610"

func (a *Agent) SetEmulation(cmdline string, none bool) {
	a.emu.mu.Lock()
	defer a.emu.mu.Unlock()
	a.emu.none = none
	for _, w := range strings.Fields(cmdline) {
		if w == api.EmulationEngineParam+"="+api.EmulationEngineAuto {
			a.emu.auto, a.emu.rosetta = true, true
		}
		if w == api.EmulationEngineParam+"="+api.EmulationEngineRosetta {
			a.emu.rosetta = true
		}
		if w == api.EmulationCmdlineParam+"=0" {
			a.emu.off = true
		}
	}
}

func (a *Agent) EmulationSets() []bool {
	a.emu.mu.Lock()
	defer a.emu.mu.Unlock()
	return append([]bool(nil), a.emu.sets...)
}

func (s *emulationState) statusLocked() api.EmulationStatus {
	st := api.EmulationStatus{X86: !s.off, TableEnabled: true, MemoryModel: true, Entries: []api.EmulationEntry{}}
	if s.auto {
		st.Mode = api.EmulationEngineAuto
	}
	if s.none {
		if st.X86 {
			st.Error = "the system image has no x86 translator at " + api.EmulationPath
		}
	} else {
		st.Translator, st.Path, st.Version = api.EmulationTranslator, api.EmulationPath, standinVersion
		if s.rosetta {
			st.Translator, st.Path, st.Version = api.EmulationRosetta, api.EmulationRosettaPath, ""
		}
		if st.X86 {
			st.Registered = true
			for _, name := range []string{api.EmulationEntryI386, api.EmulationEntryX86_64} {
				path, flags := api.EmulationPath, "POCF"
				if s.rosetta && name == api.EmulationEntryX86_64 {
					name, path, flags = api.EmulationRosettaEntry, api.EmulationRosettaPath, "POCFJ1"
				}
				st.Entries = append(st.Entries, api.EmulationEntry{Name: name, Enabled: true, Interpreter: path, Flags: flags, Fevom: true})
			}
			if s.rosetta {
				st.Entries = append(st.Entries, api.EmulationEntry{Name: api.EmulationEntryX86_64, Enabled: true, Interpreter: api.EmulationPath, Flags: "POCF2", Fevom: true})
			}
		}
	}
	st.Entries = append(st.Entries, api.EmulationEntry{Name: "qemu-riscv64", Enabled: true, Interpreter: "/usr/bin/qemu-riscv64", Flags: "POCF"})
	if s.shadow != "" {
		if _, err := os.Stat(s.shadow); err == nil {
			st.Entries = append(st.Entries, api.EmulationEntry{Name: "qemu-x86_64", Enabled: true, Interpreter: "/usr/bin/qemu-x86_64", Flags: "POCF",
				Magic: "7f454c4602010100000000000000000002003e00", Mask: "fffffffffffefe00000000fffffffffffeffffff"})
			if st.X86 && st.Registered {
				name := api.EmulationEntryX86_64
				if s.rosetta {
					name = api.EmulationRosettaEntry
				}
				st.Error = "another tool's entry qemu-x86_64 (interpreter /usr/bin/qemu-x86_64) is matched before " + name + " for the programs both take"
			}
		}
	}
	return st
}

func (a *Agent) emulationCall(method string, params json.RawMessage) (any, error) {
	a.emu.mu.Lock()
	defer a.emu.mu.Unlock()
	if method == api.MethodEmulationSet {
		p, err := jsonrpc.Bind[api.EmulationSetParams](params)
		if err != nil {
			return nil, err
		}
		a.emu.off = !p.X86
		a.emu.sets = append(a.emu.sets, p.X86)
	}
	return a.emu.statusLocked(), nil
}
