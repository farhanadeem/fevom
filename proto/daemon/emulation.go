package daemon

import "fmt"

const KeyEmulationX86 = "emulation.x86"

const KeyEmulationEngine = "emulation.engine"

const (
	EmulationOn          = "on"
	EmulationOff         = "off"
	EmulationUnavailable = "unavailable"
)

type EmulationStatus struct {
	Mode             string              `json:"mode,omitempty"`
	Reason           string              `json:"reason,omitempty"`
	Workloads        []EmulationWorkload `json:"workloads,omitempty"`
	State            string              `json:"state"`
	Translator       string              `json:"translator,omitempty"`
	Version          string              `json:"version,omitempty"`
	HardwareOrdering bool                `json:"hardware_ordering,omitempty"`
	Pending          bool                `json:"pending,omitempty"`
	EnginePending    bool                `json:"engine_pending,omitempty"`
	Restored         int                 `json:"restored,omitempty"`
	Error            string              `json:"error,omitempty"`
}

type EmulationWorkload struct {
	ID     string `json:"id,omitempty"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Engine string `json:"engine"`
	Reason string `json:"reason"`
}

func CheckEmulationStatus(e EmulationStatus) error {
	switch e.State {
	case EmulationOn, EmulationOff, EmulationUnavailable:
	default:
		return fmt.Errorf("emulation: unknown state %q", e.State)
	}
	if (e.State == EmulationUnavailable) != (e.Error != "") {
		return fmt.Errorf("emulation: an error with state %s: %q", e.State, e.Error)
	}
	if e.Version != "" && e.Translator == "" {
		return fmt.Errorf("emulation: a version and no translator")
	}
	if e.State == EmulationOn && e.Translator == "" {
		return fmt.Errorf("emulation: on with no translator")
	}
	return nil
}
