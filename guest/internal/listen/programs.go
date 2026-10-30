package listen

import (
	"github.com/cilium/ebpf/asm"
)

const recordSize = 48

const (
	kindListen   = 1
	kindClosed   = 2
	kindBound    = 3
	kindReleased = 4
)

const (
	boundAsked    = 1
	boundReported = 2
)

const (
	afInet   = 2
	afInet6  = 10
	protoTCP = 6
	protoUDP = 17

	opPassiveEstablished = 5
	opState              = 10
	opListen             = 11
	stateCBFlag          = 4
	tcpListen            = 10

	opsOp       = 0
	opsArg0     = 4
	opsArg1     = 8
	opsFamily   = 20
	opsLocalIP4 = 28
	opsLocalIP6 = 48
	opsPort     = 68
	opsCBFlags  = 84

	skFamily   = 4
	skProtocol = 12
	skSrcIP4   = 24
	skSrcIP6   = 28
	skSrcPort  = 44

	saUserPort = 24
	saProtocol = 36

	updateNoExist = 1
)

const rec = -recordSize

func zeroRecord() asm.Instructions {
	in := asm.Instructions{asm.Mov.Imm(asm.R1, 0)}
	for off := int16(0); off < recordSize; off += 8 {
		in = append(in, asm.StoreMem(asm.RFP, rec+off, asm.R1, asm.DWord))
	}
	return in
}

func address(prefix string, ip4, ip6 int16) asm.Instructions {
	return append(asm.Instructions{
		asm.JNE.Imm(asm.R7, afInet, prefix+"_v6"),
		asm.LoadMem(asm.R2, asm.R6, ip4, asm.Word),
		asm.StoreMem(asm.RFP, rec+24, asm.R2, asm.Word),
		asm.Ja.Label(prefix + "_done"),
		asm.Mov.Imm(asm.R0, 0).WithSymbol(prefix + "_v6"),
	}, append(address6(ip6), asm.Mov.Imm(asm.R0, 0).WithSymbol(prefix+"_done"))...)
}

func address6(ip6 int16) asm.Instructions {
	var in asm.Instructions
	for i := int16(0); i < 16; i += 4 {
		in = append(in,
			asm.LoadMem(asm.R2, asm.R6, ip6+i, asm.Word),
			asm.StoreMem(asm.RFP, rec+24+i, asm.R2, asm.Word),
		)
	}
	return in
}

func cookies() asm.Instructions {
	return asm.Instructions{
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.FnGetSocketCookie.Call(),
		asm.StoreMem(asm.RFP, rec+8, asm.R0, asm.DWord),
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.FnGetNetnsCookie.Call(),
		asm.StoreMem(asm.RFP, rec+16, asm.R0, asm.DWord),
	}
}

func emit(prefix string, ring, lost int) asm.Instructions {
	return asm.Instructions{
		asm.LoadMapPtr(asm.R1, ring),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, rec),
		asm.Mov.Imm(asm.R3, recordSize),
		asm.Mov.Imm(asm.R4, 0),
		asm.FnRingbufOutput.Call(),
		asm.JEq.Imm(asm.R0, 0, prefix+"_sent"),
		asm.StoreImm(asm.RFP, rec, 0, asm.Word),
		asm.LoadMapPtr(asm.R1, lost),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, rec),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, prefix+"_sent"),
		asm.Mov.Imm(asm.R1, 1),
		asm.StoreXAdd(asm.R0, asm.R1, asm.DWord),
		asm.Mov.Imm(asm.R0, 0).WithSymbol(prefix + "_sent"),
	}
}

func sockOps(ring, lost int) asm.Instructions {
	in := asm.Instructions{
		asm.Mov.Reg(asm.R6, asm.R1),
		asm.LoadMem(asm.R2, asm.R6, opsOp, asm.Word),
		asm.JEq.Imm(asm.R2, opListen, "listen"),
		asm.JEq.Imm(asm.R2, opState, "state"),
		asm.JEq.Imm(asm.R2, opPassiveEstablished, "clear"),
		asm.Ja.Label("out"),
	}
	in = append(in, asm.Mov.Imm(asm.R0, 0).WithSymbol("listen"))
	in = append(in, zeroRecord()...)
	in = append(in,
		asm.StoreImm(asm.RFP, rec+0, kindListen, asm.Byte),
		asm.StoreImm(asm.RFP, rec+1, protoTCP, asm.Byte),
		asm.LoadMem(asm.R7, asm.R6, opsFamily, asm.Word),
		asm.StoreMem(asm.RFP, rec+2, asm.R7, asm.Half),
		asm.LoadMem(asm.R2, asm.R6, opsPort, asm.Word),
		asm.StoreMem(asm.RFP, rec+4, asm.R2, asm.Half),
	)
	in = append(in, address("l", opsLocalIP4, opsLocalIP6)...)
	in = append(in, cookies()...)
	in = append(in,
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.LoadMem(asm.R2, asm.R6, opsCBFlags, asm.Word),
		asm.Or.Imm(asm.R2, stateCBFlag),
		asm.FnSockOpsCbFlagsSet.Call(),
	)
	in = append(in, emit("el", ring, lost)...)
	in = append(in, asm.Ja.Label("out"))
	in = append(in,
		asm.LoadMem(asm.R2, asm.R6, opsArg0, asm.Word).WithSymbol("state"),
		asm.JNE.Imm(asm.R2, tcpListen, "clear"),
	)
	in = append(in, zeroRecord()...)
	in = append(in,
		asm.StoreImm(asm.RFP, rec+0, kindClosed, asm.Byte),
		asm.StoreImm(asm.RFP, rec+1, protoTCP, asm.Byte),
		asm.LoadMem(asm.R2, asm.R6, opsArg0, asm.Word),
		asm.StoreMem(asm.RFP, rec+40, asm.R2, asm.Word),
		asm.LoadMem(asm.R2, asm.R6, opsArg1, asm.Word),
		asm.StoreMem(asm.RFP, rec+44, asm.R2, asm.Word),
	)
	in = append(in, cookies()...)
	in = append(in, emit("es", ring, lost)...)
	in = append(in, asm.Ja.Label("out"))
	in = append(in,
		asm.LoadMem(asm.R2, asm.R6, opsCBFlags, asm.Word).WithSymbol("clear"),
		asm.JSet.Imm(asm.R2, stateCBFlag, "unset"),
		asm.Ja.Label("out"),
		asm.And.Imm(asm.R2, ^int32(stateCBFlag)).WithSymbol("unset"),
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.FnSockOpsCbFlagsSet.Call(),
		asm.Mov.Imm(asm.R0, 1).WithSymbol("out"),
		asm.Return(),
	)
	return in
}

func postBind(ring, bound, lost int, v6 bool) asm.Instructions {
	in := asm.Instructions{
		asm.Mov.Reg(asm.R6, asm.R1),
		asm.LoadMem(asm.R2, asm.R6, skProtocol, asm.Word),
		asm.JNE.Imm(asm.R2, protoUDP, "out"),
	}
	in = append(in, zeroRecord()...)
	in = append(in,
		asm.StoreImm(asm.RFP, rec+0, kindBound, asm.Byte),
		asm.StoreImm(asm.RFP, rec+1, protoUDP, asm.Byte),
		asm.LoadMem(asm.R7, asm.R6, skFamily, asm.Word),
		asm.StoreMem(asm.RFP, rec+2, asm.R7, asm.Half),
		asm.LoadMem(asm.R2, asm.R6, skSrcPort, asm.Word),
		asm.StoreMem(asm.RFP, rec+4, asm.R2, asm.Half),
	)
	if v6 {
		in = append(in, address6(skSrcIP6)...)
	} else {
		in = append(in,
			asm.LoadMem(asm.R2, asm.R6, skSrcIP4, asm.Word),
			asm.StoreMem(asm.RFP, rec+24, asm.R2, asm.Word),
		)
	}
	in = append(in, cookies()...)
	in = append(in,
		asm.LoadMapPtr(asm.R1, bound),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, rec+8),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "out"),
		asm.LoadMem(asm.R2, asm.R0, 0, asm.Byte),
		asm.JNE.Imm(asm.R2, boundAsked, "out"),
		asm.StoreImm(asm.R0, 0, boundReported, asm.Byte),
	)
	in = append(in, emit("eb", ring, lost)...)
	in = append(in,
		asm.Mov.Imm(asm.R0, 1).WithSymbol("out"),
		asm.Return(),
	)
	return in
}

func bindHook(bound int) asm.Instructions {
	const key, val = -8, -16
	return asm.Instructions{
		asm.Mov.Reg(asm.R6, asm.R1),
		asm.LoadMem(asm.R2, asm.R6, saProtocol, asm.Word),
		asm.JNE.Imm(asm.R2, protoUDP, "out"),
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.FnGetSocketCookie.Call(),
		asm.StoreMem(asm.RFP, key, asm.R0, asm.DWord),
		asm.LoadMem(asm.R2, asm.R6, saUserPort, asm.Word),
		asm.JEq.Imm(asm.R2, 0, "zero"),
		asm.StoreImm(asm.RFP, val, boundAsked, asm.Byte),
		asm.LoadMapPtr(asm.R1, bound),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, key),
		asm.Mov.Reg(asm.R3, asm.RFP),
		asm.Add.Imm(asm.R3, val),
		asm.Mov.Imm(asm.R4, updateNoExist),
		asm.FnMapUpdateElem.Call(),
		asm.Ja.Label("out"),
		asm.LoadMapPtr(asm.R1, bound).WithSymbol("zero"),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, key),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "out"),
		asm.LoadMem(asm.R2, asm.R0, 0, asm.Byte),
		asm.JNE.Imm(asm.R2, boundAsked, "out"),
		asm.LoadMapPtr(asm.R1, bound),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, key),
		asm.FnMapDeleteElem.Call(),
		asm.Mov.Imm(asm.R0, 1).WithSymbol("out"),
		asm.Return(),
	}
}

func release(ring, bound, lost int) asm.Instructions {
	in := asm.Instructions{
		asm.Mov.Reg(asm.R6, asm.R1),
		asm.LoadMem(asm.R2, asm.R6, skProtocol, asm.Word),
		asm.JNE.Imm(asm.R2, protoUDP, "out"),
	}
	in = append(in, zeroRecord()...)
	in = append(in,
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.FnGetSocketCookie.Call(),
		asm.StoreMem(asm.RFP, rec+8, asm.R0, asm.DWord),
		asm.LoadMapPtr(asm.R1, bound),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, rec+8),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "out"),
		asm.LoadMem(asm.R7, asm.R0, 0, asm.Byte),
		asm.LoadMapPtr(asm.R1, bound),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, rec+8),
		asm.FnMapDeleteElem.Call(),
		asm.JNE.Imm(asm.R7, boundReported, "out"),
		asm.StoreImm(asm.RFP, rec+0, kindReleased, asm.Byte),
		asm.StoreImm(asm.RFP, rec+1, protoUDP, asm.Byte),
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.FnGetNetnsCookie.Call(),
		asm.StoreMem(asm.RFP, rec+16, asm.R0, asm.DWord),
	)
	in = append(in, emit("er", ring, lost)...)
	in = append(in,
		asm.Mov.Imm(asm.R0, 1).WithSymbol("out"),
		asm.Return(),
	)
	return in
}

const license = "Dual MIT/GPL"
