package emulation

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	Auto       = "auto"
	FEX        = "fex"
	Rosetta    = "rosetta"
	Label      = "dev.fevom.emulation"
	ConfigPath = "/run/fev/emulation.json"
	RecordName = ".fev-emulation.json"
	ProbePath  = "/system/emu/rosetta-probe"
)

type Config struct {
	Mode       string `json:"mode"`
	Rosetta    bool   `json:"rosetta"`
	Faccessat2 bool   `json:"faccessat2"`
	Seccomp    bool   `json:"seccomp"`
	Reason     string `json:"reason,omitempty"`
}

type Decision struct {
	Engine string `json:"engine"`
	Reason string `json:"reason"`
}

type Facts struct {
	Machine         bool
	AMD64           bool
	Distro, Version string
	Sandbox         string
}

func Valid(engine string) bool { return engine == Auto || engine == FEX || engine == Rosetta }

func Group(engine string) int {
	if engine == Rosetta {
		return 1
	}
	if engine == FEX {
		return 2
	}
	return 0
}

func Select(c Config, override string, f Facts) (Decision, error) {
	if override != "" && !Valid(override) {
		return Decision{}, fmt.Errorf("%s must be auto, fex or rosetta, got %q", Label, override)
	}
	if override == FEX {
		return Decision{FEX, "explicit workload override"}, nil
	}
	if override == Rosetta {
		if !c.Rosetta {
			return Decision{}, fmt.Errorf("Rosetta was explicitly requested but is unavailable: %s; select emulation.engine auto or rosetta and restart the VM", c.Reason)
		}
		return Decision{Rosetta, "explicit workload override"}, nil
	}
	if c.Mode == "" || c.Mode == FEX {
		return Decision{FEX, "emulation.engine is fex"}, nil
	}
	if c.Mode == Rosetta {
		if !c.Rosetta {
			if !f.AMD64 {
				return Decision{FEX, "Rosetta unavailable: " + c.Reason}, nil
			}
			return Decision{}, fmt.Errorf("Rosetta is unavailable: %s", c.Reason)
		}
		return Decision{Rosetta, "emulation.engine is rosetta"}, nil
	}
	if c.Mode != Auto {
		return Decision{}, fmt.Errorf("unknown emulation engine %q", c.Mode)
	}
	if !c.Rosetta {
		return Decision{FEX, "Rosetta unavailable: " + c.Reason}, nil
	}
	if !c.Faccessat2 && (f.Distro == "arch" || f.Distro == "archlinux" ||
		f.Distro == "ubuntu" && major(f.Version) >= 26 || f.Distro == "fedora" && major(f.Version) >= 44) {
		version := ""
		if n := major(f.Version); n > 0 {
			version = " " + strconv.Itoa(n)
		}
		return Decision{FEX, f.Distro + version + ": installed Rosetta lacks faccessat2/AT_EMPTY_PATH"}, nil
	}
	if !c.Seccomp && f.Sandbox != "" {
		return Decision{FEX, f.Sandbox + ": installed Rosetta cannot enforce x86 seccomp filters"}, nil
	}
	if !c.Seccomp {
		return Decision{Rosetta, "automatic selection: no known Rosetta incompatibility; x86 seccomp filters are not enforced under this Rosetta"}, nil
	}
	return Decision{Rosetta, "automatic selection: no known Rosetta incompatibility"}, nil
}

func major(s string) int { n, _ := strconv.Atoi(strings.SplitN(s, ".", 2)[0]); return n }

func SandboxName(name string) string {
	switch filepath.Base(name) {
	case "nix", "nix-daemon", "nix-build", "nix-shell", "pacman", "bwrap", "firejail", "podman", "crun", "runc", "containerd", "k3s":
		return filepath.Base(name)
	}
	return ""
}
