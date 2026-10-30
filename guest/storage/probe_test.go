package storage

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"strings"
	"testing"
)

type disk struct {
	size   int64
	chunks map[int64][]byte
}

func newDisk(size int64) *disk { return &disk{size: size, chunks: map[int64][]byte{}} }

func (d *disk) put(off int64, b []byte) { d.chunks[off] = b }

func (d *disk) ReadAt(p []byte, off int64) (int, error) {
	clear(p)
	for at, b := range d.chunks {
		if at+int64(len(b)) <= off || at >= off+int64(len(p)) {
			continue
		}
		if at >= off {
			copy(p[at-off:], b)
		} else {
			copy(p, b[off-at:])
		}
	}
	return len(p), nil
}

func super(offset int64, logRoot uint64) []byte {
	sb := make([]byte, superSize)
	copy(sb[magicOffset:], btrfsMagic)
	binary.LittleEndian.PutUint64(sb[0x30:], uint64(offset))
	binary.LittleEndian.PutUint64(sb[logRootField:], logRoot)
	binary.LittleEndian.PutUint32(sb, crc32.Checksum(sb[0x20:], castagnoli))
	return sb
}

const testDisk = 1 << 40

func TestOnlyAnAllZeroDiskIsBlank(t *testing.T) {
	p, err := ProbeDevice(newDisk(testDisk), testDisk)
	if err != nil || p.Kind != Blank {
		t.Fatalf("an empty disk: %+v %v", p, err)
	}
	for _, off := range []int64{0, 511, 4096, superOffset, superOffset + 4095, 1 << 20, headLen - 1,
		mirror1, mirror1 + 100, mirror2 + 4095, testDisk - tailLen, testDisk - 1} {
		d := newDisk(testDisk)
		d.put(off, []byte{1})
		p, err := ProbeDevice(d, testDisk)
		if err != nil || p.Kind == Blank {
			t.Errorf("one byte at %d: %+v %v", off, p, err)
		}
	}
}

func TestProbeTellsFilesystemsApart(t *testing.T) {
	at := func(off int64, b []byte) *disk {
		d := newDisk(testDisk)
		d.put(off, b)
		return d
	}
	temp := make([]byte, superSize)
	copy(temp[magicOffset:], btrfsTempMagic)
	torn := super(superOffset, 0)
	torn[0x200] ^= 0xff
	damagedWithMirror := at(superOffset, torn)
	damagedWithMirror.put(mirror1, super(mirror1, 0))
	zeroedWithMirror := at(mirror1, super(mirror1, 0))

	cases := []struct {
		name          string
		d             *disk
		kind          Kind
		what          string
		primary, mirr bool
	}{
		{"btrfs", at(superOffset, super(superOffset, 0)), Btrfs, "btrfs", true, false},
		{"btrfs with a bad checksum", at(superOffset, torn), Btrfs, "damaged", false, false},
		{"btrfs, first superblock torn, mirror good", damagedWithMirror, Btrfs, "damaged", false, true},
		{"btrfs, first superblock zeroed, mirror good", zeroedWithMirror, Btrfs, "damaged", false, true},
		{"superblock in the wrong place", at(mirror1, super(superOffset, 0)), Foreign, "do not recognise", false, false},
		{"a format that did not finish", at(superOffset, temp), Interrupted, "did not finish", false, false},
		{"ext4", at(0x438, []byte{0x53, 0xef}), Foreign, "ext2/3/4", false, false},
		{"erofs", at(0x400, []byte{0xe2, 0xe1, 0xf5, 0xe0}), Foreign, "erofs", false, false},
		{"xfs", at(0, []byte("XFSB")), Foreign, "XFS", false, false},
		{"gpt", at(512, []byte("EFI PART")), Foreign, "GPT", false, false},
		{"noise", at(12345, []byte("hello")), Foreign, "do not recognise", false, false},
	}
	for _, c := range cases {
		p, err := ProbeDevice(c.d, testDisk)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if p.Kind != c.kind || !strings.Contains(p.What, c.what) || p.PrimaryValid != c.primary || p.MirrorValid != c.mirr {
			t.Errorf("%s: got %+v", c.name, p)
		}
	}
}

func TestProbeReadsTheLogPointerAndRefusesTinyDisks(t *testing.T) {
	d := newDisk(testDisk)
	d.put(superOffset, super(superOffset, 0x1d2c000))
	if p, _ := ProbeDevice(d, testDisk); p.LogRoot != 0x1d2c000 {
		t.Fatalf("log root %#x", p.LogRoot)
	}
	if _, err := ProbeDevice(newDisk(1<<20), 1<<20); err == nil {
		t.Fatal("a 1 MiB disk was accepted")
	}
	small := newDisk(32 << 20)
	if p, err := ProbeDevice(small, 32<<20); err != nil || p.Kind != Blank {
		t.Fatalf("32 MiB blank disk: %+v %v", p, err)
	}
	if !bytes.Equal(btrfsMagic, []byte("_BHRfS_M")) {
		t.Fatal("magic changed")
	}
}

func TestAStaleMirrorUnderAnotherFilesystemIsNotBtrfs(t *testing.T) {
	for name, sig := range map[string]struct {
		off   int64
		magic []byte
	}{
		"XFS":      {0, []byte("XFSB")},
		"ext2/3/4": {0x438, []byte{0x53, 0xef}},
		"GPT":      {512, []byte("EFI PART")},
		"LUKS":     {0, []byte("LUKS\xba\xbe")},
	} {
		d := newDisk(testDisk)
		d.put(sig.off, sig.magic)
		d.put(mirror1, super(mirror1, 0))
		p, err := ProbeDevice(d, testDisk)
		if err != nil || p.Kind != Foreign || !strings.Contains(p.What, name) {
			t.Errorf("%s over a stale btrfs mirror: %+v %v", name, p, err)
		}
	}
	d := newDisk(testDisk)
	d.put(mirror1, super(mirror1, 0))
	d.put(superOffset+8, []byte("scribble"))
	if p, _ := ProbeDevice(d, testDisk); p.Kind != Btrfs || !p.MirrorValid {
		t.Errorf("a scribbled first superblock with a good mirror: %+v", p)
	}
}

func TestProbeReadsTheVerityFlag(t *testing.T) {
	flagged := func(offset int64, flags uint64) []byte {
		sb := super(offset, 0)
		binary.LittleEndian.PutUint64(sb[compatROField:], flags)
		binary.LittleEndian.PutUint32(sb, crc32.Checksum(sb[0x20:], castagnoli))
		return sb
	}
	const ours = 1 | 2 | 8
	for _, c := range []struct {
		name            string
		primary, mirror uint64
		breakPrimary    bool
		want            bool
	}{
		{"a fev filesystem", ours, ours, false, false},
		{"with the flag", ours | 4, ours | 4, false, true},
		{"the flag alone", 4, 4, false, true},
		{"the first superblock decides when it is valid", ours, ours | 4, false, false},
		{"a mirror decides when the first is not", ours, ours | 4, true, true},
		{"a mirror without it", ours | 4, ours, true, false},
	} {
		d := newDisk(testDisk)
		primary := flagged(superOffset, c.primary)
		if c.breakPrimary {
			primary[0x100] ^= 0xff
		}
		d.put(superOffset, primary)
		d.put(mirror1, flagged(mirror1, c.mirror))
		p, err := ProbeDevice(d, testDisk)
		if err != nil || p.Kind != Btrfs || p.PrimaryValid == c.breakPrimary {
			t.Fatalf("%s: %+v %v", c.name, p, err)
		}
		if p.Verity != c.want {
			t.Errorf("%s: Verity = %v, want %v", c.name, p.Verity, c.want)
		}
	}
}

func tempSuper(offset int64) []byte {
	sb := super(offset, 0)
	copy(sb[magicOffset:], btrfsTempMagic)
	binary.LittleEndian.PutUint32(sb, crc32.Checksum(sb[0x20:], castagnoli))
	return sb
}

func TestOnlyWhatAFormatLeavesIsAnUnfinishedFormat(t *testing.T) {
	cut := func() *disk {
		d := newDisk(testDisk)
		d.put(superOffset, tempSuper(superOffset))
		d.put(1<<20, bytes.Repeat([]byte{0x5a}, 16384))
		return d
	}
	with := func(change func(d *disk)) *disk {
		d := cut()
		change(d)
		return d
	}
	mirrors := with(func(d *disk) {
		d.put(mirror1, tempSuper(mirror1))
		d.put(mirror2, tempSuper(mirror2))
	})
	for name, d := range map[string]*disk{"the first superblock alone": cut(), "all three superblocks": mirrors} {
		p, err := ProbeDevice(d, testDisk)
		if err != nil || p.Kind != Interrupted || !p.Unfinished || p.NotProven != "" {
			t.Errorf("%s: %+v %v, want an unfinished format", name, p, err)
		}
	}

	torn := tempSuper(superOffset)
	torn[0x200] ^= 0xff
	bare := make([]byte, superSize)
	copy(bare[magicOffset:], btrfsTempMagic)
	for name, c := range map[string]struct {
		d   *disk
		why string
	}{
		"the marker with no superblock around it":     {with(func(d *disk) { d.put(superOffset, bare) }), "not whole"},
		"a marked superblock that fails its checksum": {with(func(d *disk) { d.put(superOffset, torn) }), "not whole"},
		"a marked superblock made for another place":  {with(func(d *disk) { d.put(superOffset, tempSuper(mirror1)) }), "not whole"},
		"a byte before the superblock":                {with(func(d *disk) { d.put(4096, []byte{1}) }), "first megabyte"},
		"a byte after it, below one megabyte":         {with(func(d *disk) { d.put(1<<20-1, []byte{1}) }), "first megabyte"},
		"something else at a mirror":                  {with(func(d *disk) { d.put(mirror1, []byte("data")) }), "not the format's own"},
		"a superblock in the wrong mirror":            {with(func(d *disk) { d.put(mirror2, tempSuper(mirror1)) }), "not the format's own"},
		"a byte at the end of the disk":               {with(func(d *disk) { d.put(testDisk-1, []byte{1}) }), "end of the disk"},
		"another filesystem's signature":              {with(func(d *disk) { d.put(0x438, []byte{0x53, 0xef}) }), "first megabyte"},
	} {
		p, err := ProbeDevice(c.d, testDisk)
		if err != nil || p.Kind != Interrupted || p.Unfinished || !strings.Contains(p.NotProven, c.why) {
			t.Errorf("%s: %+v %v, want an interrupted format not proven (%q)", name, p, err, c.why)
		}
	}

	for name, d := range map[string]*disk{
		"a finished mirror beside the marker": with(func(d *disk) { d.put(mirror1, super(mirror1, 0)) }),
		"a finished first superblock":         with(func(d *disk) { d.put(superOffset, super(superOffset, 0)) }),
	} {
		p, err := ProbeDevice(d, testDisk)
		if err != nil || p.Kind != Btrfs || p.Unfinished {
			t.Errorf("%s: %+v %v, want btrfs", name, p, err)
		}
	}
}

func TestABlankDiskThatOccupiesBlocksIsNotFormatted(t *testing.T) {
	for in, want := range map[string]int64{
		"": AllocatedUnknown, "x": AllocatedUnknown, "-4096": AllocatedUnknown, "1.5": AllocatedUnknown,
		"0": 0, "1048576": 1 << 20, "5368709120": 5 << 30,
	} {
		if got := ParseAllocated(in); got != want {
			t.Errorf("ParseAllocated(%q) = %d, want %d", in, got, want)
		}
	}
	blank, err := ProbeDevice(newDisk(testDisk), testDisk)
	if err != nil || blank.Kind != Blank {
		t.Fatalf("an all-zero disk: %+v %v", blank, err)
	}
	for _, c := range []struct {
		p         Probe
		allocated int64
		refused   bool
	}{
		{blank, AllocatedUnknown, false},
		{blank, 0, false},
		{blank, blankAllocatedMax, false},
		{blank, blankAllocatedMax + 1, true},
		{blank, 6 << 20, true},
		{Probe{Kind: Interrupted, Unfinished: true}, 64 << 20, false},
		{Probe{Kind: Btrfs, PrimaryValid: true}, 1 << 30, false},
		{Probe{Kind: Foreign}, 1 << 30, false},
	} {
		why := BlankButOccupied(c.p, c.allocated)
		if (why != "") != c.refused {
			t.Errorf("%v with %d bytes: %q", c.p.Kind, c.allocated, why)
		}
		if c.refused && !strings.Contains(why, fmt.Sprint(c.allocated)) {
			t.Errorf("the refusal does not say what the image occupies: %q", why)
		}
	}
}
