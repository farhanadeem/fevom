package attach

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

const (
	auditAARCH64 = 0xc00000b7
	auditX8664   = 0xc000003e
	nrRead       = 63
	nrWrite      = 64
	nrOpenat     = 56
	nrExecve     = 221
	nrClone      = 220
	nrBrk        = 214
	nrPersona    = 92
	nrKeyctl     = 219
	nrAddKey     = 217
	nrRequestKey = 218
	nrBpf        = 280
	nrMount      = 40
	nrReboot     = 142
	nrInitMod    = 105
	nrFinitMod   = 273
	nrKexec      = 104
	nrUnshare    = 97
	nrPtrace     = 117
	nrVMReadv    = 270
	nrVMWritev   = 271
	cloneNewuser = 0x10000000
)

type seccompData struct {
	nr   uint32
	arch uint32
	args [6]uint64
}

func (d seccompData) load(off uint32) uint32 {
	switch {
	case off == 0:
		return d.nr
	case off == 4:
		return d.arch
	case off >= 16 && off < 16+6*8:
		idx := (off - 16) / 8
		lo := (off-16)%8 == 0
		v := d.args[idx]
		if lo {
			return uint32(v)
		}
		return uint32(v >> 32)
	}
	return 0
}

func runFilter(t *testing.T, prog []Insn, d seccompData) uint32 {
	t.Helper()
	var a uint32
	pc := 0
	for steps := 0; steps < 100000; steps++ {
		if pc < 0 || pc >= len(prog) {
			t.Fatalf("pc %d out of range", pc)
		}
		in := prog[pc]
		switch in.Code & 0x07 {
		case 0x00:
			a = d.load(in.K)
			pc++
		case 0x04:
			if in.Code&0xf0 != 0x50 {
				t.Fatalf("unexpected ALU op %#x at %d", in.Code&0xf0, pc)
			}
			a &= in.K
			pc++
		case 0x05:
			switch in.Code & 0xf0 {
			case 0x00:
				pc += 1 + int(in.K)
			case 0x10:
				pc += 1 + jump(a == in.K, in)
			case 0x20:
				pc += 1 + jump(a > in.K, in)
			case 0x30:
				pc += 1 + jump(a >= in.K, in)
			case 0x40:
				pc += 1 + jump(a&in.K != 0, in)
			default:
				t.Fatalf("unexpected JMP op %#x at %d", in.Code&0xf0, pc)
			}
		case 0x06:
			return in.K & 0xffff0000
		default:
			t.Fatalf("unexpected insn class %#x at %d", in.Code&0x07, pc)
		}
	}
	t.Fatal("filter did not return")
	return 0
}

func jump(cond bool, in Insn) int {
	if cond {
		return int(in.JT)
	}
	return int(in.JF)
}

const (
	retAllowK = 0x7fff0000
	retErrnoK = 0x00050000
	retKillTh = 0x00000000
	retKillPr = 0x80000000
)

func call(nr uint32, args ...uint64) seccompData {
	d := seccompData{nr: nr, arch: auditAARCH64}
	copy(d.args[:], args)
	return d
}

func TestDefaultSeccompProgram(t *testing.T) {
	prog := DefaultProgram()
	if len(prog) != 846 {
		t.Fatalf("default program length %d, want 846 (the dumped Docker default)", len(prog))
	}
	deny := map[string]uint32{
		"keyctl": nrKeyctl, "add_key": nrAddKey, "request_key": nrRequestKey,
		"bpf": nrBpf, "mount": nrMount, "reboot": nrReboot,
		"init_module": nrInitMod, "finit_module": nrFinitMod, "kexec_load": nrKexec,
		"unshare": nrUnshare,
	}
	for name, nr := range deny {
		if got := runFilter(t, prog, call(nr)); got != retErrnoK {
			t.Errorf("%s: action %#x, want ERRNO (the default profile denies it)", name, got)
		}
	}
	allow := map[string]uint32{
		"read": nrRead, "write": nrWrite, "openat": nrOpenat,
		"execve": nrExecve, "brk": nrBrk, "personality": nrPersona,
	}
	for name, nr := range allow {
		if got := runFilter(t, prog, call(nr)); got != retAllowK {
			t.Errorf("%s: action %#x, want ALLOW", name, got)
		}
	}
	if got := runFilter(t, prog, call(nrClone, 0)); got != retAllowK {
		t.Errorf("clone(no flags): action %#x, want ALLOW (threads need it)", got)
	}
	if got := runFilter(t, prog, call(nrClone, cloneNewuser)); got != retErrnoK {
		t.Errorf("clone(CLONE_NEWUSER): action %#x, want ERRNO", got)
	}
	if got := runFilter(t, prog, seccompData{nr: nrRead, arch: auditX8664}); got != retKillTh && got != retKillPr {
		t.Errorf("foreign-arch read: action %#x, want a KILL", got)
	}
}

func TestDefaultSeccompWithPrefix(t *testing.T) {
	layers := DefaultSeccomp(ArchARM64)
	if len(layers) != 1 {
		t.Fatalf("DefaultSeccomp returned %d layers, want 1", len(layers))
	}
	prog := layers[0]
	for name, nr := range map[string]uint32{
		"ptrace": nrPtrace, "process_vm_readv": nrVMReadv, "process_vm_writev": nrVMWritev,
	} {
		if got := runFilter(t, prog, call(nr)); got != retAllowK {
			t.Errorf("%s through the prefix: action %#x, want ALLOW", name, got)
		}
	}
	if got := runFilter(t, prog, call(nrKeyctl)); got != retErrnoK {
		t.Errorf("keyctl after prefix: action %#x, want ERRNO", got)
	}
	if got := runFilter(t, prog, seccompData{nr: nrPtrace, arch: auditX8664}); got == retAllowK {
		t.Errorf("foreign-arch ptrace was allowed by the prefix; the guard should pass it to the program")
	}
}

func TestDefaultSeccompIsTheRecordedCapture(t *testing.T) {
	sum := sha256.Sum256(defaultProgramBytes)
	if got := hex.EncodeToString(sum[:]); got != DefaultSeccompSHA256 {
		t.Fatalf("the shipped program's sha256 is %s, recorded %s", got, DefaultSeccompSHA256)
	}
	lock, err := os.ReadFile("../../../inputs.lock")
	if err != nil {
		t.Skipf("no inputs.lock: %v", err)
	}
	for _, want := range []string{
		"moby/moby/" + DefaultSeccompEngine + "/",
		"opencontainers/runc/" + DefaultSeccompRunc + "/",
		"seccomp/libseccomp/" + DefaultSeccompLibseccomp + "/",
	} {
		if !strings.Contains(string(lock), want) {
			t.Errorf("inputs.lock pins no %s: the engine moved; capture the default program again", want)
		}
	}
}
