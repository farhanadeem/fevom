package kernelid

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func mknote(name string, typ uint32, desc []byte) []byte {
	var b bytes.Buffer
	n := []byte(name)
	if name != "" {
		n = append(n, 0)
	}
	for _, v := range []uint32{uint32(len(n)), uint32(len(desc)), typ} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.Write(n)
	b.Write(make([]byte, (4-len(n)%4)%4))
	b.Write(desc)
	b.Write(make([]byte, (4-len(desc)%4)%4))
	return b.Bytes()
}

var (
	idA = bytes.Repeat([]byte{0xa1}, 20)
	idB = bytes.Repeat([]byte{0xb2}, 20)
)

func kernelNotes(id []byte) []byte {
	return append(mknote("Linux", 0x100, []byte{0}), mknote("GNU", ntGNUBuildID, id)...)
}

func TestFromNotes(t *testing.T) {
	got, err := FromNotes(kernelNotes(idA))
	if err != nil || got != strings.Repeat("a1", 20) {
		t.Fatalf("FromNotes = %q, %v", got, err)
	}
	for name, b := range map[string][]byte{
		"none":      mknote("Linux", 0x100, []byte{0}),
		"two":       append(kernelNotes(idA), mknote("GNU", ntGNUBuildID, idB)...),
		"cut short": kernelNotes(idA)[:30],
		"empty":     mknote("GNU", ntGNUBuildID, nil),
		"another type": append(mknote("Linux", 0x100, []byte{0}),
			mknote("GNU", 1, idA)...),
	} {
		if id, err := FromNotes(b); err == nil {
			t.Errorf("%s: FromNotes = %q, want an error", name, id)
		}
	}
}

func elf64(body []byte) []byte {
	var h [64]byte
	copy(h[:], "\x7fELF\x02\x01\x01")
	le := binary.LittleEndian
	le.PutUint16(h[0x10:], 3)
	le.PutUint16(h[0x12:], 183)
	shoff := uint64(64 + len(body))
	shoff = (shoff + 7) &^ 7
	le.PutUint64(h[0x28:], shoff)
	le.PutUint16(h[0x34:], 64)
	le.PutUint16(h[0x3a:], 64)
	le.PutUint16(h[0x3c:], 2)
	out := append(h[:], body...)
	out = append(out, make([]byte, int(shoff)-len(out))...)
	var null, sec [64]byte
	le.PutUint32(sec[4:], 7)
	le.PutUint64(sec[24:], 64)
	le.PutUint64(sec[32:], uint64(len(body)))
	return append(append(out, null[:]...), sec[:]...)
}

func image(parts ...[]byte) []byte {
	b := make([]byte, 64)
	copy(b[imageMagicAt:], imageMagic)
	for _, p := range parts {
		b = append(b, p...)
		b = append(b, make([]byte, (4-len(b)%4)%4)...)
	}
	return append(b, make([]byte, 256)...)
}

func TestFromImage(t *testing.T) {
	code := bytes.Repeat([]byte{0x1f, 0x20, 0x03, 0xd5}, 64)
	vdso := elf64(append(mknote("Linux", 0, []byte{9, 2, 7, 0}), mknote("GNU", ntGNUBuildID, idB)...))
	got, err := FromImage(image(code, vdso, code, kernelNotes(idA), code))
	if err != nil || got != strings.Repeat("a1", 20) {
		t.Fatalf("FromImage = %q, %v; want the kernel's own, not the vDSO's", got, err)
	}
	for name, b := range map[string][]byte{
		"only the vDSO's": image(code, vdso, code),
		"two":             image(kernelNotes(idA), code, kernelNotes(idB)),
		"no magic":        kernelNotes(idA),
	} {
		if id, err := FromImage(b); err == nil {
			t.Errorf("%s: FromImage = %q, want an error", name, id)
		}
	}
	bad := elf64(nil)[:64]
	binary.LittleEndian.PutUint64(bad[0x28:], 1<<40)
	if got, err := FromImage(image(bad, kernelNotes(idA))); err != nil || got != strings.Repeat("a1", 20) {
		t.Errorf("after a broken ELF header: FromImage = %q, %v", got, err)
	}
}

func TestTheBuiltImages(t *testing.T) {
	images, _ := filepath.Glob("../../../kernel/out/Image")
	more, _ := filepath.Glob("../../../kernel/out/*/Image")
	images = append(images, more...)
	if len(images) == 0 {
		t.Skip("no kernel/out/Image built")
	}
	hexID := regexp.MustCompile(`^[0-9a-f]{40}$`)
	for _, p := range images {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		id, err := FromImage(b)
		if err != nil || !hexID.MatchString(id) {
			t.Errorf("%s: %q, %v", p, id, err)
			continue
		}
		t.Logf("%s: %s", p, id)
	}
}
