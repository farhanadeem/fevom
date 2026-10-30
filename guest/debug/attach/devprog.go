package attach

import "encoding/binary"

func denyDeviceProgram() []byte {
	ins := [][]byte{
		bpfInsn(0x61, 2, 1, 0, 0),
		bpfInsn(0x61, 3, 1, 4, 0),
		bpfInsn(0x61, 4, 1, 8, 0),
		bpfInsn(0xbf, 5, 2, 0, 0),
		bpfInsn(0x57, 5, 0, 0, 0xffff),
		bpfInsn(0x55, 5, 0, 12, 2),
		bpfInsn(0x55, 3, 0, 6, 1),
		bpfInsn(0x15, 4, 0, 12, 3),
		bpfInsn(0x15, 4, 0, 11, 5),
		bpfInsn(0x15, 4, 0, 10, 7),
		bpfInsn(0x15, 4, 0, 9, 8),
		bpfInsn(0x15, 4, 0, 8, 9),
		bpfInsn(0x05, 0, 0, 5, 0),
		bpfInsn(0x55, 3, 0, 3, 5),
		bpfInsn(0x15, 4, 0, 5, 0),
		bpfInsn(0x15, 4, 0, 4, 2),
		bpfInsn(0x05, 0, 0, 1, 0),
		bpfInsn(0x15, 3, 0, 2, 136),
		bpfInsn(0xb7, 0, 0, 0, 0),
		bpfInsn(0x95, 0, 0, 0, 0),
		bpfInsn(0xb7, 0, 0, 0, 1),
		bpfInsn(0x95, 0, 0, 0, 0),
	}
	var p []byte
	for _, b := range ins {
		p = append(p, b...)
	}
	return p
}

func bpfInsn(code uint8, dst, src uint8, off int16, imm int32) []byte {
	b := make([]byte, 8)
	b[0] = code
	b[1] = src<<4 | dst
	binary.LittleEndian.PutUint16(b[2:], uint16(off))
	binary.LittleEndian.PutUint32(b[4:], uint32(imm))
	return b
}
