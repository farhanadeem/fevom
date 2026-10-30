package attach

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	StageSpecFd = 3
	StageUserFd = 4
)

const (
	SpecFlagJoinUser = 1 << 0
	SpecFlagFreshNS  = 1 << 1
)

var specMagic = [4]byte{'D', 'B', 'G', '1'}

type Spec struct {
	Flags    uint32
	UID, GID uint32
	CapKeep  uint64
	Dir      string
	Argv     []string
	Env      []string
	Seccomp  [][]Insn
}

func (s Spec) Encode() ([]byte, error) {
	if len(s.Argv) == 0 || s.Argv[0] == "" {
		return nil, errors.New("debug stage spec: no program")
	}
	var b []byte
	b = append(b, specMagic[:]...)
	b = le32(b, s.Flags)
	b = le32(b, s.UID)
	b = le32(b, s.GID)
	b = le64(b, s.CapKeep)
	b = leStr(b, s.Dir)
	b = le32(b, uint32(len(s.Argv)))
	for _, a := range s.Argv {
		b = leStr(b, a)
	}
	b = le32(b, uint32(len(s.Env)))
	for _, e := range s.Env {
		b = leStr(b, e)
	}
	b = le32(b, uint32(len(s.Seccomp)))
	for _, layer := range s.Seccomp {
		b = le32(b, uint32(len(layer)))
		for _, in := range layer {
			b = le16(b, in.Code)
			b = append(b, in.JT, in.JF)
			b = le32(b, in.K)
		}
	}
	return b, nil
}

func Decode(b []byte) (Spec, error) {
	r := reader{b: b}
	var magic [4]byte
	copy(magic[:], r.take(4))
	if r.err != nil || magic != specMagic {
		return Spec{}, errors.New("debug stage spec: bad magic")
	}
	var s Spec
	s.Flags = r.u32()
	s.UID = r.u32()
	s.GID = r.u32()
	s.CapKeep = r.u64()
	s.Dir = r.str()
	for n := r.u32(); n > 0 && r.err == nil; n-- {
		s.Argv = append(s.Argv, r.str())
	}
	for n := r.u32(); n > 0 && r.err == nil; n-- {
		s.Env = append(s.Env, r.str())
	}
	for n := r.u32(); n > 0 && r.err == nil; n-- {
		m := r.u32()
		layer := make([]Insn, 0, m)
		for ; m > 0 && r.err == nil; m-- {
			in := Insn{Code: r.u16()}
			in.JT = r.u8()
			in.JF = r.u8()
			in.K = r.u32()
			layer = append(layer, in)
		}
		s.Seccomp = append(s.Seccomp, layer)
	}
	if r.err != nil {
		return Spec{}, fmt.Errorf("debug stage spec: %w", r.err)
	}
	return s, nil
}

func le16(b []byte, v uint16) []byte { return binary.LittleEndian.AppendUint16(b, v) }
func le32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }
func le64(b []byte, v uint64) []byte { return binary.LittleEndian.AppendUint64(b, v) }
func leStr(b []byte, s string) []byte {
	b = le32(b, uint32(len(s)))
	return append(b, s...)
}

type reader struct {
	b   []byte
	off int
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || r.off+n > len(r.b) {
		r.err = errors.New("short")
		return nil
	}
	out := r.b[r.off : r.off+n]
	r.off += n
	return out
}
func (r *reader) u8() uint8 {
	p := r.take(1)
	if r.err != nil {
		return 0
	}
	return p[0]
}
func (r *reader) u16() uint16 {
	p := r.take(2)
	if r.err != nil {
		return 0
	}
	return binary.LittleEndian.Uint16(p)
}
func (r *reader) u32() uint32 {
	p := r.take(4)
	if r.err != nil {
		return 0
	}
	return binary.LittleEndian.Uint32(p)
}
func (r *reader) u64() uint64 {
	p := r.take(8)
	if r.err != nil {
		return 0
	}
	return binary.LittleEndian.Uint64(p)
}
func (r *reader) str() string {
	n := r.u32()
	p := r.take(int(n))
	if r.err != nil {
		return ""
	}
	return string(p)
}
