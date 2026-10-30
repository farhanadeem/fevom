package storage

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
)

type Kind int

const (
	Blank Kind = iota
	Btrfs
	Interrupted
	Foreign
)

func (k Kind) String() string {
	return [...]string{"blank", "btrfs", "interrupted format", "foreign"}[k]
}

type Probe struct {
	Kind         Kind
	What         string
	PrimaryValid bool
	MirrorValid  bool
	LogRoot      uint64
	Verity       bool
	Unfinished   bool
	NotProven    string
}

const (
	superSize   = 4096
	superOffset = 64 << 10
	mirror1     = 64 << 20
	mirror2     = 256 << 30

	magicOffset    = 0x40
	logRootField   = 0x60
	compatROField  = 0xb4
	compatROVerity = 1 << 2
	csumTypeAt     = 0xc4

	headLen     = 2 << 20
	reservedLen = 1 << 20
	tailLen     = 64 << 10
)

var (
	btrfsMagic     = []byte("_BHRfS_M")
	btrfsTempMagic = []byte("!BHRfS_M")
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func validSuper(sb []byte, offset int64) bool { return superWith(sb, offset, btrfsMagic) }

func superWith(sb []byte, offset int64, magic []byte) bool {
	if len(sb) < superSize || !bytes.Equal(sb[magicOffset:magicOffset+8], magic) {
		return false
	}
	if binary.LittleEndian.Uint64(sb[0x30:]) != uint64(offset) {
		return false
	}
	if binary.LittleEndian.Uint16(sb[csumTypeAt:]) != 0 {
		return false
	}
	return binary.LittleEndian.Uint32(sb[0:]) == crc32.Checksum(sb[0x20:superSize], castagnoli)
}

func unfinished(head []byte, mirrors [][]byte, tail []byte) string {
	primary := head[superOffset : superOffset+superSize]
	if !superWith(primary, superOffset, btrfsTempMagic) {
		return "the superblock under the marker is not whole: its offset or its checksum is wrong"
	}
	if !allZero(head[:superOffset]) || !allZero(head[superOffset+superSize:reservedLen]) {
		return "there is data in the first megabyte beside the superblock, where a format leaves none"
	}
	for i, sb := range mirrors {
		off := []int64{mirror1, mirror2}[i]
		if !allZero(sb) && !superWith(sb, off, btrfsTempMagic) {
			return fmt.Sprintf("the superblock position at %d holds something that is not the format's own", off)
		}
	}
	if !allZero(tail) {
		return "there is data at the end of the disk, where a format leaves none"
	}
	return ""
}

func allZero(b []byte) bool {
	for len(b) >= 8 {
		if binary.LittleEndian.Uint64(b) != 0 {
			return false
		}
		b = b[8:]
	}
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

var signatures = []struct {
	name   string
	offset int
	magic  []byte
}{
	{"ext2/3/4", 0x438, []byte{0x53, 0xef}},
	{"XFS", 0, []byte("XFSB")},
	{"erofs", 0x400, []byte{0xe2, 0xe1, 0xf5, 0xe0}},
	{"squashfs", 0, []byte("hsqs")},
	{"f2fs", 0x400, []byte{0x10, 0x20, 0xf5, 0xf2}},
	{"LUKS", 0, []byte("LUKS\xba\xbe")},
	{"LVM", 0x218, []byte("LVM2 001")},
	{"swap", 4096 - 10, []byte("SWAPSPACE2")},
	{"a GPT partition table", 512, []byte("EFI PART")},
	{"an MBR partition table or boot sector", 510, []byte{0x55, 0xaa}},
}

const AllocatedParam = "fev.data_allocated"

const AllocatedUnknown = -1

func ParseAllocated(value string) int64 {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return AllocatedUnknown
	}
	return n
}

const blankAllocatedMax = 1 << 20

func BlankButOccupied(p Probe, allocated int64) string {
	if p.Kind != Blank || allocated <= blankAllocatedMax {
		return ""
	}
	return fmt.Sprintf("reads as blank, but its image occupies %d bytes on the Mac, which a blank image fev made never does: "+
		"most likely it held a filesystem whose superblocks are gone, and its files may still be in it", allocated)
}

func ProbeDevice(r io.ReaderAt, size int64) (Probe, error) {
	if size < headLen {
		return Probe{}, fmt.Errorf("disk is %d bytes, too small to hold a filesystem", size)
	}
	read := func(off int64, n int) ([]byte, error) {
		b := make([]byte, n)
		if _, err := r.ReadAt(b, off); err != nil {
			return nil, fmt.Errorf("read %d bytes at %d: %w", n, off, err)
		}
		return b, nil
	}
	head, err := read(0, headLen)
	if err != nil {
		return Probe{}, err
	}
	zero := allZero(head)
	var p Probe
	primary := head[superOffset : superOffset+superSize]
	p.PrimaryValid = validSuper(primary, superOffset)
	hasMagic := bytes.Equal(primary[magicOffset:magicOffset+8], btrfsMagic)
	if p.PrimaryValid {
		p.LogRoot = binary.LittleEndian.Uint64(primary[logRootField:])
		p.Verity = binary.LittleEndian.Uint64(primary[compatROField:])&compatROVerity != 0
	}
	var mirrors [][]byte
	for _, off := range []int64{mirror1, mirror2} {
		if off+superSize > size {
			continue
		}
		sb, err := read(off, superSize)
		if err != nil {
			return Probe{}, err
		}
		zero = zero && allZero(sb)
		mirrors = append(mirrors, sb)
		if validSuper(sb, off) {
			if !p.PrimaryValid && !p.MirrorValid {
				p.Verity = binary.LittleEndian.Uint64(sb[compatROField:])&compatROVerity != 0
			}
			p.MirrorValid = true
		}
	}
	tail, err := read(size-tailLen, tailLen)
	if err != nil {
		return Probe{}, err
	}
	zero = zero && allZero(tail)

	other := ""
	for _, s := range signatures {
		if bytes.Equal(head[s.offset:s.offset+len(s.magic)], s.magic) {
			other = s.name
			break
		}
	}

	switch {
	case zero:
		p.Kind, p.What = Blank, "blank"
	case hasMagic || (p.MirrorValid && other == ""):
		p.Kind, p.What = Btrfs, "btrfs"
		if !p.PrimaryValid {
			p.What = "btrfs with a damaged first superblock"
		}
	case bytes.Equal(primary[magicOffset:magicOffset+8], btrfsTempMagic):
		p.Kind, p.What = Interrupted, "a btrfs format that did not finish"
		p.NotProven = unfinished(head, mirrors, tail)
		p.Unfinished = p.NotProven == ""
	case other != "":
		p.Kind, p.What = Foreign, other
	default:
		p.Kind, p.What = Foreign, "data we do not recognise"
	}
	return p, nil
}
