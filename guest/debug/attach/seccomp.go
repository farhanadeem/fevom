package attach

type Insn struct {
	Code uint16
	JT   uint8
	JF   uint8
	K    uint32
}

const (
	bpfLD  = 0x00
	bpfW   = 0x00
	bpfABS = 0x20
	bpfJMP = 0x05
	bpfJEQ = 0x10
	bpfJA  = 0x00
	bpfK   = 0x00
	bpfRET = 0x06

	insnLDWABS  = bpfLD | bpfW | bpfABS
	insnJEQK    = bpfJMP | bpfJEQ | bpfK
	insnJAK     = bpfJMP | bpfJA | bpfK
	insnRETK    = bpfRET | bpfK
	retAllow    = 0x7fff0000
	dataNR      = 0
	dataArch    = 4
	maxPrefixes = 8
)

type Arch struct {
	Audit    uint32
	Ptrace   uint32
	VMReadv  uint32
	VMWritev uint32
}

var (
	ArchARM64 = Arch{Audit: 0xc00000b7, Ptrace: 117, VMReadv: 270, VMWritev: 271}
	ArchAMD64 = Arch{Audit: 0xc000003e, Ptrace: 101, VMReadv: 310, VMWritev: 311}
)

func (a Arch) PrefixCalls() []uint32 {
	return []uint32{a.Ptrace, a.VMReadv, a.VMWritev}
}

func BuildPrefix(a Arch, guard bool) []Insn {
	calls := a.PrefixCalls()
	if len(calls) == 0 {
		return nil
	}
	archlen := 0
	if guard {
		archlen = 2
	}
	prefixLen := archlen + 1 + len(calls) + 1 + 1
	out := make([]Insn, 0, prefixLen)
	if guard {
		out = append(out, Insn{Code: insnLDWABS, K: dataArch})
		out = append(out, Insn{Code: insnJEQK, JT: 0, JF: uint8(prefixLen - 2), K: a.Audit})
	}
	out = append(out, Insn{Code: insnLDWABS, K: dataNR})
	for i, nr := range calls {
		out = append(out, Insn{Code: insnJEQK, JT: uint8(len(calls) - i), JF: 0, K: nr})
	}
	out = append(out, Insn{Code: insnJAK, K: 1})
	out = append(out, Insn{Code: insnRETK, K: retAllow})
	return out
}

func WithPrefix(prefix, program []Insn) []Insn {
	out := make([]Insn, 0, len(prefix)+len(program))
	out = append(out, prefix...)
	out = append(out, program...)
	return out
}
