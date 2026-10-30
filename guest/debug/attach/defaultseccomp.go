package attach

import (
	_ "embed"
	"encoding/binary"
)

const (
	DefaultSeccompEngine     = "docker-v29.9.0"
	DefaultSeccompRunc       = "v1.5.2"
	DefaultSeccompLibseccomp = "v2.6.1"
	DefaultSeccompSHA256     = "4b2732e76a6ca9d3b16fe4e782c24ea27ad239318bdd6cc635a13acd94da96df"
)

//go:embed defaultseccomp_arm64.bpf
var defaultProgramBytes []byte

var defaultProgram = decodeProgram(defaultProgramBytes)

func decodeProgram(b []byte) []Insn {
	if len(b)%8 != 0 {
		panic("attach: default seccomp program length is not a multiple of 8")
	}
	out := make([]Insn, len(b)/8)
	for i := range out {
		off := i * 8
		out[i] = Insn{
			Code: binary.LittleEndian.Uint16(b[off:]),
			JT:   b[off+2],
			JF:   b[off+3],
			K:    binary.LittleEndian.Uint32(b[off+4:]),
		}
	}
	return out
}

func DefaultProgram() []Insn {
	out := make([]Insn, len(defaultProgram))
	copy(out, defaultProgram)
	return out
}

func DefaultSeccomp(a Arch) [][]Insn {
	return [][]Insn{WithPrefix(BuildPrefix(a, true), DefaultProgram())}
}
