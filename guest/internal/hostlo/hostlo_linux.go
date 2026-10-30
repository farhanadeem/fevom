package hostlo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"
)

const (
	offUserIP4  = 4
	offUserIP6  = 8
	offUserPort = 24
	offType     = 32
	offMsgSrc4  = 40
	offSk       = 64
)

const skMark = 16

const (
	stTuple = -16
	stKey   = -4
	stMark  = -20
)

var v4mapped = binary.LittleEndian.Uint32([]byte{0, 0, 0xff, 0xff})

type Counts struct {
	Offered, Kept uint64
}

type Programs struct {
	counts *ebpf.Map
	links  []link.Link
}

var loopback = binary.LittleEndian.Uint32([]byte{127, 0, 0, 1})

func count(counts *ebpf.Map, label string, idx int32, next string) asm.Instructions {
	return asm.Instructions{
		asm.StoreImm(asm.R10, stKey, int64(idx), asm.Word).WithSymbol(label),
		asm.LoadMapPtr(asm.R1, counts.FD()),
		asm.Mov.Reg(asm.R2, asm.R10),
		asm.Add.Imm(asm.R2, stKey),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, next),
		asm.Mov.Imm(asm.R1, 1),
		asm.StoreXAdd(asm.R0, asm.R1, asm.DWord),
		asm.Ja.Label(next),
	}
}

func lookup(cookie uint64, ip int16, v6 bool) asm.Instructions {
	in := asm.Instructions{asm.Mov.Reg(asm.R6, asm.R1)}
	if v6 {
		in = append(in, mapped("allow")...)
	}
	return append(in,
		asm.LoadMem(asm.R7, asm.R6, ip, asm.Word),
		asm.JNE.Imm32(asm.R7, int32(loopback), "allow"),
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.FnGetNetnsCookie.Call(),
		asm.LoadImm(asm.R1, int64(cookie), asm.DWord),
		asm.JNE.Reg(asm.R0, asm.R1, "allow"),
		asm.StoreImm(asm.R10, stTuple, 0, asm.Word),
		asm.StoreMem(asm.R10, stTuple+4, asm.R7, asm.Word),
		asm.LoadMem(asm.R8, asm.R6, offUserPort, asm.Word),
		asm.StoreImm(asm.R10, stTuple+8, 0, asm.Half),
		asm.StoreMem(asm.R10, stTuple+10, asm.R8, asm.Half),
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.Mov.Reg(asm.R2, asm.R10),
		asm.Add.Imm(asm.R2, stTuple),
		asm.Mov.Imm(asm.R3, 12),
		asm.LoadImm(asm.R4, -1, asm.DWord),
		asm.Mov.Imm(asm.R5, 0),
		asm.LoadMem(asm.R9, asm.R6, offType, asm.Word),
		asm.JEq.Imm(asm.R9, int32(unix.SOCK_DGRAM), "udp"),
		asm.FnSkLookupTcp.Call(),
		asm.Ja.Label("looked"),
		asm.FnSkLookupUdp.Call().WithSymbol("udp"),
		asm.JEq.Imm(asm.R0, 0, "none").WithSymbol("looked"),
		asm.Mov.Reg(asm.R1, asm.R0),
		asm.FnSkRelease.Call(),
		asm.Ja.Label("found"),
	)
}

func setMark(next string) asm.Instructions {
	return asm.Instructions{
		asm.StoreMem(asm.R10, stMark, asm.R9, asm.Word),
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.Mov.Imm(asm.R2, unix.SOL_SOCKET),
		asm.Mov.Imm(asm.R3, unix.SO_MARK),
		asm.Mov.Reg(asm.R4, asm.R10),
		asm.Add.Imm(asm.R4, stMark),
		asm.Mov.Imm(asm.R5, 4),
		asm.FnSetsockopt.Call(),
		asm.Ja.Label(next),
	}
}

func connect(counts *ebpf.Map, cookie uint64, v6 bool) asm.Instructions {
	ip := int16(offUserIP4)
	if v6 {
		ip = offUserIP6 + 12
	}
	in := lookup(cookie, ip, v6)
	in = append(in,
		asm.LoadMem(asm.R8, asm.R6, offSk, asm.DWord).WithSymbol("found"),
		asm.LoadMem(asm.R9, asm.R8, skMark, asm.Word),
		asm.Mov.Reg(asm.R1, asm.R9),
		asm.And.Imm(asm.R1, Mark),
		asm.JEq.Imm(asm.R1, 0, "kept"),
		asm.And.Imm32(asm.R9, ^int32(Mark)),
	)
	in = append(in, setMark("kept")...)
	in = append(in, count(counts, "kept", 1, "allow")...)
	in = append(in,
		asm.LoadMem(asm.R8, asm.R6, offSk, asm.DWord).WithSymbol("none"),
		asm.LoadMem(asm.R9, asm.R8, skMark, asm.Word),
		asm.Or.Imm(asm.R9, Mark),
	)
	in = append(in, setMark("offered")...)
	in = append(in, count(counts, "offered", 0, "allow")...)
	return append(in,
		asm.Mov.Imm(asm.R0, 1).WithSymbol("allow"),
		asm.Return(),
	)
}

func sendmsg(counts *ebpf.Map, cookie uint64, source uint32) asm.Instructions {
	in := lookup(cookie, offUserIP4, false)
	in = append(in, asm.Ja.Label("kept").WithSymbol("found"))
	in = append(in, count(counts, "kept", 1, "allow")...)
	in = append(in,
		asm.LoadMem(asm.R9, asm.R6, offMsgSrc4, asm.Word).WithSymbol("none"),
		asm.JNE.Imm(asm.R9, 0, "allow"),
		asm.Mov.Imm32(asm.R1, int32(source)),
		asm.StoreMem(asm.R6, offMsgSrc4, asm.R1, asm.Word),
	)
	in = append(in, count(counts, "offered", 0, "allow")...)
	return append(in,
		asm.Mov.Imm(asm.R0, 1).WithSymbol("allow"),
		asm.Return(),
	)
}

func mapped(label string) asm.Instructions {
	return asm.Instructions{
		asm.LoadMem(asm.R7, asm.R6, offUserIP6, asm.Word),
		asm.JNE.Imm(asm.R7, 0, label),
		asm.LoadMem(asm.R7, asm.R6, offUserIP6+4, asm.Word),
		asm.JNE.Imm(asm.R7, 0, label),
		asm.LoadMem(asm.R7, asm.R6, offUserIP6+8, asm.Word),
		asm.JNE.Imm32(asm.R7, int32(v4mapped), label),
	}
}

func le(a netip.Addr) uint32 {
	b := a.As4()
	return binary.LittleEndian.Uint32(b[:])
}

func Attach(cgroup string, source netip.Addr) (_ *Programs, err error) {
	if !source.Is4() {
		return nil, fmt.Errorf("the source %s is not IPv4", source)
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	cookie, err := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
	unix.Close(fd)
	if err != nil {
		return nil, fmt.Errorf("this namespace's cookie: %w", err)
	}
	p := &Programs{}
	defer func() {
		if err != nil {
			p.Close()
		}
	}()
	if p.counts, err = ebpf.NewMap(&ebpf.MapSpec{Name: "hostlo_counts", Type: ebpf.Array, KeySize: 4, ValueSize: 8, MaxEntries: 2}); err != nil {
		return nil, fmt.Errorf("the counts: %w", err)
	}
	for _, a := range []struct {
		attach ebpf.AttachType
		name   string
		code   asm.Instructions
	}{
		{ebpf.AttachCGroupInet4Connect, "hostlo_connect", connect(p.counts, cookie, false)},
		{ebpf.AttachCGroupUDP4Sendmsg, "hostlo_sendmsg", sendmsg(p.counts, cookie, le(source))},
		{ebpf.AttachCGroupInet6Connect, "hostlo_connect6", connect(p.counts, cookie, true)},
	} {
		prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
			Name: a.name, Type: ebpf.CGroupSockAddr, AttachType: a.attach, Instructions: a.code,
			License: "Dual MIT/GPL",
		})
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", a.name, err)
		}
		l, err := link.AttachCgroup(link.CgroupOptions{Path: cgroup, Attach: a.attach, Program: prog})
		prog.Close()
		if err != nil {
			return nil, fmt.Errorf("attach %s at %s: %w", a.name, cgroup, err)
		}
		p.links = append(p.links, l)
	}
	return p, nil
}

func (p *Programs) Counts() (Counts, error) {
	var c Counts
	err := errors.Join(p.counts.Lookup(uint32(0), &c.Offered), p.counts.Lookup(uint32(1), &c.Kept))
	return c, err
}

func (p *Programs) Close() error {
	var errs []error
	for _, l := range p.links {
		errs = append(errs, l.Close())
	}
	p.links = nil
	if p.counts != nil {
		errs = append(errs, p.counts.Close())
	}
	return errors.Join(errs...)
}
