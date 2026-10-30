package nfsview

import "encoding/binary"

const (
	rpcCall       = 0
	rpcReply      = 1
	rpcVersion    = 2
	msgAccepted   = 0
	acceptSuccess = 0
	authNone      = 0
	nfsProgram    = 100003
	nfsVersion4   = 4
	procNull      = 0
	procCompound  = 1
	nfs4OK        = 0
	nfs4ErrDelay  = 10008
	opRenew       = 30
	headMax       = 2048
)

type callHead struct {
	xid    uint32
	proc   uint32
	tag    []byte
	numops uint32
	op     uint32
}

type headResult int

const (
	headMore headResult = iota
	headOurs
	headNotOurs
)

func parseHead(p []byte, complete bool) (callHead, headResult) {
	var c callHead
	off := 0
	short := func() (callHead, headResult) {
		if complete || len(p) >= headMax {
			return c, headNotOurs
		}
		return c, headMore
	}
	u32 := func() (uint32, bool) {
		if off+4 > len(p) {
			return 0, false
		}
		v := binary.BigEndian.Uint32(p[off:])
		off += 4
		return v, true
	}
	var tooLong bool
	opaque := func() ([]byte, bool) {
		n, ok := u32()
		if !ok {
			return nil, false
		}
		if n > headMax {
			tooLong = true
			return nil, false
		}
		end := off + int(n+3)&^3
		if end > len(p) {
			return nil, false
		}
		b := p[off : off+int(n)]
		off = end
		return b, true
	}
	var fields [6]uint32
	for i := range fields {
		v, ok := u32()
		if !ok {
			return short()
		}
		fields[i] = v
	}
	c.xid, c.proc = fields[0], fields[5]
	if fields[1] != rpcCall || fields[2] != rpcVersion || fields[3] != nfsProgram || fields[4] != nfsVersion4 {
		return c, headNotOurs
	}
	if c.proc != procNull && c.proc != procCompound {
		return c, headNotOurs
	}
	for range 2 {
		if _, ok := u32(); !ok {
			return short()
		}
		if _, ok := opaque(); !ok {
			if tooLong {
				return c, headNotOurs
			}
			return short()
		}
	}
	if c.proc == procNull {
		return c, headOurs
	}
	tag, ok := opaque()
	if !ok {
		if tooLong {
			return c, headNotOurs
		}
		return short()
	}
	c.tag = append([]byte(nil), tag...)
	minor, ok := u32()
	if !ok {
		return short()
	}
	if minor != 0 {
		return c, headNotOurs
	}
	if c.numops, ok = u32(); !ok {
		return short()
	}
	if c.numops == 0 {
		return c, headNotOurs
	}
	if c.op, ok = u32(); !ok {
		return short()
	}
	return c, headOurs
}

func pausedAnswer(c callHead) []byte {
	b := make([]byte, 4, 64+len(c.tag))
	for _, v := range []uint32{c.xid, rpcReply, msgAccepted, authNone, 0, acceptSuccess} {
		b = binary.BigEndian.AppendUint32(b, v)
	}
	if c.proc == procCompound {
		st := uint32(nfs4ErrDelay)
		if c.numops == 1 && c.op == opRenew {
			st = nfs4OK
		}
		b = binary.BigEndian.AppendUint32(b, st)
		b = binary.BigEndian.AppendUint32(b, uint32(len(c.tag)))
		b = append(b, c.tag...)
		b = append(b, make([]byte, (4-len(c.tag)%4)%4)...)
		b = binary.BigEndian.AppendUint32(b, 1)
		b = binary.BigEndian.AppendUint32(b, c.op)
		b = binary.BigEndian.AppendUint32(b, st)
	}
	binary.BigEndian.PutUint32(b, uint32(len(b)-4)|0x80000000)
	return b
}
