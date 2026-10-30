package agent

import (
	"context"
	"fmt"
	"strings"
)

const (
	MethodEmulationStatus = "emulation.status"
	MethodEmulationSet    = "emulation.set"
)

const EmulationCmdlineParam = "fev.x86"

const EmulationEngineParam = "fev.emulator"

const (
	EmulationEngineFEX     = "fex"
	EmulationEngineAuto    = "auto"
	EmulationEngineRosetta = "rosetta"
	EmulationRosetta       = "Rosetta"
	EmulationRosettaEntry  = "Rosetta-x86_64"
	EmulationRosettaMount  = "/run/fev/rosetta"
	EmulationRosettaPath   = EmulationRosettaMount + "/rosetta"
	EmulationRosettaCache  = "fev.rosetta.cache"
)

const (
	EmulationEntryX86_64 = "FEX-x86_64"
	EmulationEntryI386   = "FEX-x86"
	EmulationTranslator  = "FEX-Emu"
	EmulationPath        = "/system/emu/FEX"
)

type EmulationEntry struct {
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Interpreter string `json:"interpreter"`
	Flags       string `json:"flags"`
	Fevom       bool   `json:"fev,omitempty"`
	Offset      int    `json:"offset,omitempty"`
	Magic       string `json:"magic,omitempty"`
	Mask        string `json:"mask,omitempty"`
	Extension   string `json:"extension,omitempty"`
}

type EmulationStatus struct {
	Mode         string              `json:"mode,omitempty"`
	Reason       string              `json:"reason,omitempty"`
	Workloads    []EmulationWorkload `json:"workloads,omitempty"`
	X86          bool                `json:"x86"`
	Registered   bool                `json:"registered"`
	Translator   string              `json:"translator,omitempty"`
	Path         string              `json:"path,omitempty"`
	Version      string              `json:"version,omitempty"`
	MemoryModel  bool                `json:"memory_model"`
	TableEnabled bool                `json:"table_enabled"`
	Entries      []EmulationEntry    `json:"entries"`
	Restored     int                 `json:"restored"`
	Error        string              `json:"error,omitempty"`
}

type EmulationWorkload struct {
	ID     string `json:"id,omitempty"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Engine string `json:"engine"`
	Reason string `json:"reason"`
}

type EmulationSetParams struct {
	X86 bool `json:"x86"`
}

func CheckEmulationStatus(s EmulationStatus) error {
	if s.Entries == nil {
		return fmt.Errorf("emulation: entries is null")
	}
	fev := map[string]bool{}
	for _, e := range s.Entries {
		if e.Fevom {
			if e.Name != EmulationEntryX86_64 && e.Name != EmulationEntryI386 && e.Name != EmulationRosettaEntry {
				return fmt.Errorf("emulation: %s marked fev's", e.Name)
			}
			fev[e.Name] = e.Enabled
		}
	}
	amd64 := EmulationEntryX86_64
	if s.Translator == EmulationRosetta {
		amd64 = EmulationRosettaEntry
	}
	both := fev[amd64] && fev[EmulationEntryI386]
	for _, e := range s.Entries {
		if e.Name == EmulationRosettaEntry && strings.Contains(e.Flags, "1") {
			both = both && fev[EmulationEntryX86_64]
		}
	}
	if s.Registered != both {
		return fmt.Errorf("emulation: registered %v with fev's entries %v", s.Registered, fev)
	}
	if s.Registered && !s.X86 {
		return fmt.Errorf("emulation: registered while x86 is off")
	}
	if s.Error != "" && (!s.X86 || (s.Registered && s.TableEnabled)) {
		return fmt.Errorf("emulation: an error with x86 %v, registered %v and the table enabled %v", s.X86, s.Registered, s.TableEnabled)
	}
	if s.Version != "" && s.Translator == "" {
		return fmt.Errorf("emulation: a version and no translator")
	}
	if s.Restored < 0 {
		return fmt.Errorf("emulation: restored %d", s.Restored)
	}
	return nil
}

func (c *Client) EmulationStatus(ctx context.Context) (*EmulationStatus, error) {
	var r EmulationStatus
	return &r, c.conn.Call(ctx, MethodEmulationStatus, Empty{}, &r)
}

func (c *Client) EmulationSet(ctx context.Context, x86 bool) (*EmulationStatus, error) {
	var r EmulationStatus
	return &r, c.conn.Call(ctx, MethodEmulationSet, EmulationSetParams{X86: x86}, &r)
}
