package attach

import (
	"fmt"
	"sort"
)

const (
	bpfJGT = 0x20
	bpfJGE = 0x30
	bpfALU = 0x04
	bpfAND = 0x50

	insnJGTK = bpfJMP | bpfJGT | bpfK
	insnJGEK = bpfJMP | bpfJGE | bpfK
	insnANDK = bpfALU | bpfAND | bpfK
)

const (
	retKillThread  = 0x00000000
	retKillProcess = 0x80000000
	retTrap        = 0x00030000
	retErrnoBase   = 0x00050000
	retUserNotif   = 0x7fc00000
	retTraceBase   = 0x7ff00000
	retLog         = 0x7ffc0000

	defaultErrno  = 1
	enosysErrno   = 38
	badArchAction = retKillThread
)

func argLoOff(index uint32) uint32 { return 16 + 8*index }
func argHiOff(index uint32) uint32 { return 16 + 8*index + 4 }

type OCISeccomp struct {
	DefaultAction   string       `json:"defaultAction"`
	DefaultErrnoRet *uint        `json:"defaultErrnoRet,omitempty"`
	Architectures   []string     `json:"architectures,omitempty"`
	Flags           []string     `json:"flags,omitempty"`
	Syscalls        []OCISyscall `json:"syscalls,omitempty"`
}

type OCISyscall struct {
	Names    []string `json:"names"`
	Action   string   `json:"action"`
	ErrnoRet *uint    `json:"errnoRet,omitempty"`
	Args     []OCIArg `json:"args,omitempty"`
}

type OCIArg struct {
	Index    uint32 `json:"index"`
	Value    uint64 `json:"value"`
	ValueTwo uint64 `json:"valueTwo,omitempty"`
	Op       string `json:"op"`
}

//go:generate go run ./internal/gensyscalls syscallnum_gen.go

type scEntry struct {
	name string
	nr   uint32
}

func lookupSyscall(t []scEntry, name string) (uint32, bool) {
	i := sort.Search(len(t), func(i int) bool { return t[i].name >= name })
	if i < len(t) && t[i].name == name {
		return t[i].nr, true
	}
	return 0, false
}

func ARM64SyscallNR(name string) (uint32, bool) { return lookupSyscall(arm64Syscalls, name) }
func AMD64SyscallNR(name string) (uint32, bool) { return lookupSyscall(amd64Syscalls, name) }

func actionCode(action string, errnoRet *uint) (uint32, error) {
	ret := func(e *uint) uint32 {
		if e == nil {
			return uint32(defaultErrno)
		}
		return uint32(*e) & 0xffff
	}
	switch action {
	case "SCMP_ACT_ALLOW":
		return retAllow, nil
	case "SCMP_ACT_ERRNO":
		return retErrnoBase | ret(errnoRet), nil
	case "SCMP_ACT_TRACE":
		return retTraceBase | ret(errnoRet), nil
	case "SCMP_ACT_TRAP":
		return retTrap, nil
	case "SCMP_ACT_KILL", "SCMP_ACT_KILL_THREAD":
		return retKillThread, nil
	case "SCMP_ACT_KILL_PROCESS":
		return retKillProcess, nil
	case "SCMP_ACT_LOG":
		return retLog, nil
	case "SCMP_ACT_NOTIFY":
		return 0, fmt.Errorf("the profile uses SCMP_ACT_NOTIFY, which needs a user-space listener a debug session cannot provide")
	default:
		return 0, fmt.Errorf("the profile uses an unknown seccomp action %q", action)
	}
}

type asm struct {
	ins   []Insn
	jt    []string
	jf    []string
	ja    []string
	marks map[string]int
	n     int

	actLabels map[uint32]string
	actCodes  []uint32
}

func newAsm() *asm { return &asm{marks: map[string]int{}} }

func (a *asm) label() string { a.n++; return fmt.Sprintf("L%d", a.n) }

func (a *asm) mark(l string) { a.marks[l] = len(a.ins) }

func (a *asm) emit(in Insn) {
	a.ins = append(a.ins, in)
	a.jt = append(a.jt, "")
	a.jf = append(a.jf, "")
	a.ja = append(a.ja, "")
}

func (a *asm) cond(code uint16, k uint32, jtL, jfL string) {
	a.ins = append(a.ins, Insn{Code: code, K: k})
	a.jt = append(a.jt, jtL)
	a.jf = append(a.jf, jfL)
	a.ja = append(a.ja, "")
}

func (a *asm) jump(l string) {
	a.ins = append(a.ins, Insn{Code: insnJAK})
	a.jt = append(a.jt, "")
	a.jf = append(a.jf, "")
	a.ja = append(a.ja, l)
}

func (a *asm) program() ([]Insn, error) {
	out := make([]Insn, len(a.ins))
	copy(out, a.ins)
	for i := range out {
		if l := a.ja[i]; l != "" {
			t, ok := a.marks[l]
			if !ok {
				return nil, fmt.Errorf("internal: unresolved jump label %q", l)
			}
			out[i].K = uint32(t - (i + 1))
			continue
		}
		if l := a.jt[i]; l != "" {
			t, ok := a.marks[l]
			if !ok {
				return nil, fmt.Errorf("internal: unresolved label %q", l)
			}
			off := t - (i + 1)
			if off < 0 || off > 255 {
				return nil, fmt.Errorf("the profile needs a conditional jump of %d (over the kernel's 255 limit)", off)
			}
			out[i].JT = uint8(off)
		}
		if l := a.jf[i]; l != "" {
			t, ok := a.marks[l]
			if !ok {
				return nil, fmt.Errorf("internal: unresolved label %q", l)
			}
			off := t - (i + 1)
			if off < 0 || off > 255 {
				return nil, fmt.Errorf("the profile needs a conditional jump of %d (over the kernel's 255 limit)", off)
			}
			out[i].JF = uint8(off)
		}
	}
	return out, nil
}

func stubEnosys(p OCISeccomp) bool {
	switch p.DefaultAction {
	case "SCMP_ACT_ALLOW", "SCMP_ACT_LOG", "SCMP_ACT_TRACE":
		return false
	}
	if p.DefaultAction == "SCMP_ACT_ERRNO" && p.DefaultErrnoRet != nil && *p.DefaultErrnoRet == enosysErrno {
		return false
	}
	return true
}

func CompileProfile(p OCISeccomp, arch Arch, nr func(string) (uint32, bool)) ([]Insn, error) {
	if p.DefaultAction == "" {
		return nil, fmt.Errorf("the seccomp profile has no defaultAction")
	}
	defAction, err := actionCode(p.DefaultAction, p.DefaultErrnoRet)
	if err != nil {
		return nil, fmt.Errorf("the profile's default action: %w", err)
	}

	type rule struct {
		action uint32
		args   []OCIArg
		orArgs bool
	}
	order := []uint32{}
	seen := map[uint32]bool{}
	rules := map[uint32][]rule{}
	fallAct := map[uint32]uint32{}
	for _, s := range p.Syscalls {
		act, err := actionCode(s.Action, s.ErrnoRet)
		if err != nil {
			return nil, err
		}
		for _, ar := range s.Args {
			if ar.Index > 5 {
				return nil, fmt.Errorf("the profile compares argument %d (only 0..5 exist)", ar.Index)
			}
			switch ar.Op {
			case "SCMP_CMP_EQ", "SCMP_CMP_NE", "SCMP_CMP_LT", "SCMP_CMP_LE",
				"SCMP_CMP_GT", "SCMP_CMP_GE", "SCMP_CMP_MASKED_EQ":
			default:
				return nil, fmt.Errorf("the profile uses an unknown comparison operator %q", ar.Op)
			}
		}
		if act == defAction {
			continue
		}
		idxCount := map[uint32]int{}
		for _, ar := range s.Args {
			idxCount[ar.Index]++
		}
		orArgs := false
		for _, c := range idxCount {
			if c > 1 {
				orArgs = true
				break
			}
		}
		unconditional := len(s.Args) == 0
		for _, name := range s.Names {
			num, ok := nr(name)
			if !ok {
				continue
			}
			if !seen[num] {
				order = append(order, num)
				seen[num] = true
			}
			if unconditional {
				rules[num] = nil
				fallAct[num] = act
			} else {
				rules[num] = append(rules[num], rule{action: act, args: s.Args, orArgs: orArgs})
			}
		}
	}

	a := newAsm()
	lDefault := a.label()
	lOK := a.label()

	a.emit(Insn{Code: insnLDWABS, K: dataArch})
	a.cond(insnJEQK, arch.Audit, lOK, "")
	a.emit(Insn{Code: insnRETK, K: badArchAction})

	a.mark(lOK)
	a.emit(Insn{Code: insnLDWABS, K: dataNR})

	for _, num := range order {
		lIn := a.label()
		lNext := a.label()
		a.cond(insnJEQK, num, lIn, "")
		a.jump(lNext)
		a.mark(lIn)
		rs := rules[num]
		lNoMatch := a.label()
		for ri, r := range rs {
			lFail := lNoMatch
			if ri < len(rs)-1 {
				lFail = a.label()
			}
			if r.orArgs {
				for _, ar := range r.args {
					lSkip := a.label()
					emitArgCond(a, ar, lSkip)
					a.jump(a.actionLabel(r.action))
					a.mark(lSkip)
				}
				a.jump(lFail)
			} else {
				for _, ar := range r.args {
					emitArgCond(a, ar, lFail)
				}
				a.jump(a.actionLabel(r.action))
			}
			if ri < len(rs)-1 {
				a.mark(lFail)
			}
		}
		a.mark(lNoMatch)
		fa := defAction
		if f, ok := fallAct[num]; ok {
			fa = f
		}
		if fa == defAction {
			a.jump(lDefault)
		} else {
			a.jump(a.actionLabel(fa))
		}
		a.mark(lNext)
	}

	if stubEnosys(p) {
		var maxNr uint32
		for _, n := range order {
			if n > maxNr {
				maxNr = n
			}
		}
		if maxNr != 0 {
			a.cond(insnJGTK, maxNr, "", lDefault)
			a.jump(a.actionLabel(retErrnoBase | enosysErrno))
		}
	}

	a.mark(lDefault)
	a.emit(Insn{Code: insnRETK, K: defAction})
	if err := a.emitActions(); err != nil {
		return nil, err
	}
	return a.program()
}

func (a *asm) actionLabel(code uint32) string {
	if a.actLabels == nil {
		a.actLabels = map[uint32]string{}
	}
	if l, ok := a.actLabels[code]; ok {
		return l
	}
	l := a.label()
	a.actLabels[code] = l
	a.actCodes = append(a.actCodes, code)
	return l
}

func (a *asm) emitActions() error {
	for _, code := range a.actCodes {
		a.mark(a.actLabels[code])
		a.emit(Insn{Code: insnRETK, K: code})
	}
	return nil
}

func (a *asm) ld(off uint32) { a.emit(Insn{Code: insnLDWABS, K: off}) }

func pick(cond bool, pass, fail string) string {
	if cond {
		return pass
	}
	return fail
}

func emitArgCond(a *asm, ar OCIArg, failLabel string) {
	lo, hi := uint32(ar.Value), uint32(ar.Value>>32)
	loOff, hiOff := argLoOff(ar.Index), argHiOff(ar.Index)
	switch ar.Op {
	case "SCMP_CMP_EQ":
		a.ld(loOff)
		a.cond(insnJEQK, lo, "", failLabel)
		a.ld(hiOff)
		a.cond(insnJEQK, hi, "", failLabel)
	case "SCMP_CMP_NE":
		lPass := a.label()
		a.ld(loOff)
		a.cond(insnJEQK, lo, "", lPass)
		a.ld(hiOff)
		a.cond(insnJEQK, hi, failLabel, lPass)
		a.mark(lPass)
	case "SCMP_CMP_MASKED_EQ":
		a.ld(loOff)
		a.emit(Insn{Code: insnANDK, K: lo})
		a.cond(insnJEQK, uint32(ar.ValueTwo), "", failLabel)
		a.ld(hiOff)
		a.emit(Insn{Code: insnANDK, K: hi})
		a.cond(insnJEQK, uint32(ar.ValueTwo>>32), "", failLabel)
	case "SCMP_CMP_LT":
		emitRange(a, loOff, hiOff, lo, hi, failLabel, false, true, insnJGEK, false)
	case "SCMP_CMP_LE":
		emitRange(a, loOff, hiOff, lo, hi, failLabel, false, true, insnJGTK, false)
	case "SCMP_CMP_GT":
		emitRange(a, loOff, hiOff, lo, hi, failLabel, true, false, insnJGTK, true)
	case "SCMP_CMP_GE":
		emitRange(a, loOff, hiOff, lo, hi, failLabel, true, false, insnJGEK, true)
	}
}

func emitRange(a *asm, loOff, hiOff, lo, hi uint32, failLabel string, hiGreaterPass, hiLessPass bool, loOp uint16, loTruePass bool) {
	lPass := a.label()
	a.ld(hiOff)
	a.cond(insnJGTK, hi, pick(hiGreaterPass, lPass, failLabel), "")
	a.cond(insnJEQK, hi, "", pick(hiLessPass, lPass, failLabel))
	a.ld(loOff)
	a.cond(loOp, lo, pick(loTruePass, lPass, failLabel), pick(loTruePass, failLabel, lPass))
	a.mark(lPass)
}

func CompileContainerProfile(raw []byte, bounding uint64, arch Arch, kernel KernelVersion, nr func(string) (uint32, bool)) ([][]Insn, error) {
	oci, err := ResolveDockerProfile(raw, bounding, arch, kernel)
	if err != nil {
		return nil, err
	}
	prog, err := CompileProfile(oci, arch, nr)
	if err != nil {
		return nil, err
	}
	return [][]Insn{WithPrefix(BuildPrefix(arch, true), prog)}, nil
}
