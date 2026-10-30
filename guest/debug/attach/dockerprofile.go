package attach

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type KernelVersion struct {
	Kernel, Major uint64
}

func (k KernelVersion) atLeast(min KernelVersion) bool {
	if k.Kernel != min.Kernel {
		return k.Kernel > min.Kernel
	}
	return k.Major >= min.Major
}

type dockerProfile struct {
	DefaultAction   string          `json:"defaultAction"`
	DefaultErrnoRet *uint           `json:"defaultErrnoRet,omitempty"`
	Architectures   []string        `json:"architectures,omitempty"`
	ArchMap         []dockerArchMap `json:"archMap,omitempty"`
	Flags           []string        `json:"flags,omitempty"`
	Syscalls        []dockerSyscall `json:"syscalls"`
}

type dockerArchMap struct {
	Arch      string   `json:"architecture"`
	SubArches []string `json:"subArchitectures"`
}

type dockerFilter struct {
	Caps      []string `json:"caps,omitempty"`
	Arches    []string `json:"arches,omitempty"`
	MinKernel string   `json:"minKernel,omitempty"`
}

type dockerSyscall struct {
	Names    []string      `json:"names"`
	Name     string        `json:"name,omitempty"`
	Action   string        `json:"action"`
	ErrnoRet *uint         `json:"errnoRet,omitempty"`
	Args     []OCIArg      `json:"args,omitempty"`
	Includes *dockerFilter `json:"includes,omitempty"`
	Excludes *dockerFilter `json:"excludes,omitempty"`
}

var archToSeccomp = map[string]string{
	"amd64": "SCMP_ARCH_X86_64",
	"arm64": "SCMP_ARCH_AARCH64",
}

func nativeArchString(a Arch) string {
	switch a.Audit {
	case ArchARM64.Audit:
		return "arm64"
	case ArchAMD64.Audit:
		return "amd64"
	default:
		return ""
	}
}

func capNamesSet(mask uint64) map[string]bool {
	set := map[string]bool{}
	for name, bit := range capBit {
		if mask&(1<<bit) != 0 {
			set["CAP_"+name] = true
		}
	}
	return set
}

func parseMinKernel(s string) (KernelVersion, bool, error) {
	if s == "" {
		return KernelVersion{}, false, nil
	}
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 2 {
		return KernelVersion{}, false, fmt.Errorf("invalid minKernel %q, expected \"kernel.major\"", s)
	}
	k, err := strconv.ParseUint(parts[0], 10, 8)
	if err != nil {
		return KernelVersion{}, false, fmt.Errorf("invalid minKernel %q: %w", s, err)
	}
	maj, err := strconv.ParseUint(parts[1], 10, 8)
	if err != nil {
		return KernelVersion{}, false, fmt.Errorf("invalid minKernel %q: %w", s, err)
	}
	return KernelVersion{Kernel: k, Major: maj}, true, nil
}

func ResolveDockerProfile(raw []byte, bounding uint64, arch Arch, kernel KernelVersion) (OCISeccomp, error) {
	var p dockerProfile
	if err := json.Unmarshal(raw, &p); err != nil {
		return OCISeccomp{}, fmt.Errorf("decoding the seccomp profile: %w", err)
	}
	if p.DefaultAction == "" && len(p.Syscalls) == 0 {
		return OCISeccomp{}, fmt.Errorf("the seccomp profile has no defaultAction and no syscalls")
	}
	if len(p.Architectures) != 0 && len(p.ArchMap) != 0 {
		return OCISeccomp{}, fmt.Errorf("the seccomp profile sets both 'architectures' and 'archMap'")
	}

	archStr := nativeArchString(arch)
	caps := capNamesSet(bounding)

	out := OCISeccomp{
		DefaultAction:   p.DefaultAction,
		DefaultErrnoRet: p.DefaultErrnoRet,
		Flags:           p.Flags,
	}
	if len(p.Architectures) != 0 {
		out.Architectures = p.Architectures
	} else if scmp, ok := archToSeccomp[archStr]; ok {
		for _, am := range p.ArchMap {
			if am.Arch == scmp {
				out.Architectures = append(out.Architectures, am.Arch)
				out.Architectures = append(out.Architectures, am.SubArches...)
				break
			}
		}
	}

	for _, s := range p.Syscalls {
		names := s.Names
		if s.Name != "" {
			if len(names) != 0 {
				return OCISeccomp{}, fmt.Errorf("a syscall rule sets both 'name' and 'names'")
			}
			names = []string{s.Name}
		}
		keep, err := admit(s.Includes, s.Excludes, archStr, caps, kernel)
		if err != nil {
			return OCISeccomp{}, err
		}
		if !keep {
			continue
		}
		out.Syscalls = append(out.Syscalls, OCISyscall{
			Names:    names,
			Action:   s.Action,
			ErrnoRet: s.ErrnoRet,
			Args:     s.Args,
		})
	}
	return out, nil
}

func admit(inc, exc *dockerFilter, archStr string, caps map[string]bool, kernel KernelVersion) (bool, error) {
	if exc != nil {
		if len(exc.Arches) > 0 && contains(exc.Arches, archStr) {
			return false, nil
		}
		if len(exc.Caps) > 0 {
			for _, c := range exc.Caps {
				if caps[c] {
					return false, nil
				}
			}
		}
		if mk, ok, err := parseMinKernel(exc.MinKernel); err != nil {
			return false, err
		} else if ok && kernel.atLeast(mk) {
			return false, nil
		}
	}
	if inc != nil {
		if len(inc.Arches) > 0 && !contains(inc.Arches, archStr) {
			return false, nil
		}
		if len(inc.Caps) > 0 {
			for _, c := range inc.Caps {
				if !caps[c] {
					return false, nil
				}
			}
		}
		if mk, ok, err := parseMinKernel(inc.MinKernel); err != nil {
			return false, err
		} else if ok && !kernel.atLeast(mk) {
			return false, nil
		}
	}
	return true, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
