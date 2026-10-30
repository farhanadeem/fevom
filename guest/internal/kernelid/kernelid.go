package kernelid

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

const ntGNUBuildID = 3

var gnu = []byte("GNU\x00")

type note struct {
	at, desc, end int
	typ           uint32
	name          []byte
}

func notes(b []byte) ([]note, error) {
	var out []note
	for off := 0; off < len(b); {
		if len(b)-off < 12 {
			return nil, fmt.Errorf("a note header cut short at byte %d", off)
		}
		namesz := uint64(binary.LittleEndian.Uint32(b[off:]))
		descsz := uint64(binary.LittleEndian.Uint32(b[off+4:]))
		typ := binary.LittleEndian.Uint32(b[off+8:])
		name := uint64(off) + 12
		desc := name + (namesz+3)&^3
		end := desc + (descsz+3)&^3
		if end > uint64(len(b)) || desc+descsz > uint64(len(b)) {
			return nil, fmt.Errorf("the note at byte %d runs past the end (%d bytes)", off, len(b))
		}
		out = append(out, note{at: off, desc: int(desc), end: int(desc + descsz), typ: typ, name: b[name : name+namesz]})
		off = int(end)
	}
	return out, nil
}

func FromNotes(b []byte) (string, error) {
	ns, err := notes(b)
	if err != nil {
		return "", err
	}
	id := ""
	for _, n := range ns {
		if n.typ != ntGNUBuildID || !bytes.Equal(n.name, gnu) {
			continue
		}
		if id != "" {
			return "", errors.New("more than one GNU build ID note")
		}
		if n.end == n.desc {
			return "", errors.New("an empty GNU build ID")
		}
		id = hex.EncodeToString(b[n.desc:n.end])
	}
	if id == "" {
		return "", errors.New("no GNU build ID note")
	}
	return id, nil
}

const imageMagicAt = 56

var imageMagic = []byte("ARM\x64")

func FromImage(b []byte) (string, error) {
	if len(b) < imageMagicAt+4 || !bytes.Equal(b[imageMagicAt:imageMagicAt+4], imageMagic) {
		return "", errors.New("not an arm64 kernel Image (no ARM\\x64 magic at byte 56)")
	}
	objs := embedded(b)
	id := ""
	at := -1
	for off := 0; off+16 <= len(b); off += 4 {
		if binary.LittleEndian.Uint32(b[off:]) != 4 || binary.LittleEndian.Uint32(b[off+8:]) != ntGNUBuildID ||
			!bytes.Equal(b[off+12:off+16], gnu) {
			continue
		}
		descsz := int(binary.LittleEndian.Uint32(b[off+4:]))
		if descsz < 8 || descsz > 64 || off+16+descsz > len(b) {
			continue
		}
		if inside(objs, off) {
			continue
		}
		if id != "" {
			return "", fmt.Errorf("two GNU build ID notes outside the Image's own ELF objects, at bytes %d and %d", at, off)
		}
		id = hex.EncodeToString(b[off+16 : off+16+descsz])
		at = off
	}
	if id == "" {
		return "", errors.New("no GNU build ID note in the Image outside its own ELF objects")
	}
	return id, nil
}

type span struct{ from, to int }

func inside(objs []span, off int) bool {
	for _, o := range objs {
		if off >= o.from && off < o.to {
			return true
		}
	}
	return false
}

func embedded(b []byte) []span {
	var out []span
	magic := []byte("\x7fELF\x02\x01\x01")
	for from := 0; ; {
		i := bytes.Index(b[from:], magic)
		if i < 0 {
			return out
		}
		at := from + i
		from = at + 1
		if end, ok := elfEnd(b[at:]); ok {
			out = append(out, span{at, at + end})
		}
	}
}

func elfEnd(e []byte) (int, bool) {
	if len(e) < 64 {
		return 0, false
	}
	le := binary.LittleEndian
	phoff, shoff := le.Uint64(e[0x20:]), le.Uint64(e[0x28:])
	ehsize, phentsize, phnum := le.Uint16(e[0x34:]), le.Uint16(e[0x36:]), le.Uint16(e[0x38:])
	shentsize, shnum := le.Uint16(e[0x3a:]), le.Uint16(e[0x3c:])
	if ehsize != 64 || (phnum > 0 && phentsize != 56) || (shnum > 0 && shentsize != 64) {
		return 0, false
	}
	size := uint64(len(e))
	end := uint64(64)
	grow := func(off, n uint64) bool {
		if off > size || n > size-off {
			return false
		}
		if off+n > end {
			end = off + n
		}
		return true
	}
	if !grow(phoff, uint64(phnum)*56) || !grow(shoff, uint64(shnum)*64) {
		return 0, false
	}
	for i := uint64(0); i < uint64(phnum); i++ {
		p := e[phoff+i*56:]
		if !grow(le.Uint64(p[8:]), le.Uint64(p[32:])) {
			return 0, false
		}
	}
	for i := uint64(0); i < uint64(shnum); i++ {
		s := e[shoff+i*64:]
		const nobits = 8
		if le.Uint32(s[4:]) == nobits {
			continue
		}
		if !grow(le.Uint64(s[24:]), le.Uint64(s[32:])) {
			return 0, false
		}
	}
	return int(end), true
}
