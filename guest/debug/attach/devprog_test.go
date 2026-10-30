package attach

import (
	"encoding/binary"
	"testing"
)

func runDevProg(t *testing.T, prog []byte, typ, major, minor uint32) uint64 {
	t.Helper()
	ctx := [3]uint32{2<<16 | typ, major, minor}
	var r [11]uint64
	for pc := 0; pc*8 < len(prog); {
		b := prog[pc*8 : pc*8+8]
		code, dst, src := b[0], b[1]&0xf, b[1]>>4
		off := int(int16(binary.LittleEndian.Uint16(b[2:])))
		imm := uint64(uint32(binary.LittleEndian.Uint32(b[4:])))
		pc++
		switch code {
		case 0x61:
			r[dst] = uint64(ctx[off/4])
		case 0xbf:
			r[dst] = r[src]
		case 0x57:
			r[dst] &= imm
		case 0xb7:
			r[dst] = imm
		case 0x55:
			if r[dst] != imm {
				pc += off
			}
		case 0x15:
			if r[dst] == imm {
				pc += off
			}
		case 0x05:
			pc += off
		case 0x95:
			return r[0]
		default:
			t.Fatalf("opcode %#x", code)
		}
	}
	t.Fatal("ran off the program")
	return 0
}

func TestDenyDeviceProgram(t *testing.T) {
	p := denyDeviceProgram()
	const blk, chr = 1, 2
	for _, c := range []struct {
		typ, major, minor uint32
		allow             bool
		what              string
	}{
		{chr, 1, 3, true, "null"}, {chr, 1, 5, true, "zero"}, {chr, 1, 7, true, "full"},
		{chr, 1, 8, true, "random"}, {chr, 1, 9, true, "urandom"}, {chr, 5, 0, true, "tty"},
		{chr, 5, 2, true, "ptmx"}, {chr, 136, 4, true, "a pts"},
		{chr, 5, 1, false, "the console"}, {chr, 1, 1, false, "mem"}, {chr, 1, 2, false, "kmem"},
		{chr, 4, 0, false, "tty0"}, {blk, 254, 0, false, "vda"}, {blk, 8, 0, false, "sda"},
		{blk, 7, 0, false, "loop0"}, {chr, 10, 200, false, "net/tun"},
	} {
		got := runDevProg(t, p, c.typ, c.major, c.minor) == 1
		if got != c.allow {
			t.Errorf("%s (%d:%d): allowed %v", c.what, c.major, c.minor, got)
		}
	}
}
