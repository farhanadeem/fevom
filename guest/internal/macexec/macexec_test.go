package macexec

import (
	"encoding/binary"
	"os"
	"runtime"
	"testing"
)

func head(order binary.ByteOrder, words ...uint32) []byte {
	b := make([]byte, 4*len(words))
	for i, w := range words {
		order.PutUint32(b[4*i:], w)
	}
	return b
}

func TestMacProgramsAreTakenAndNothingElse(t *testing.T) {
	le, be := binary.LittleEndian, binary.BigEndian
	const (
		mhMagic64  = 0xfeedfacf
		arm64      = 0x0100000c
		x86_64     = 0x01000007
		arm64e     = 0x80000002
		mhExecute  = 2
		mhDylib    = 6
		mhBundle   = 8
		mhObject   = 1
		fatMagic   = 0xcafebabe
		fatMagic64 = 0xcafebabf
	)
	for _, c := range []struct {
		what string
		head []byte
		want string
	}{
		{"arm64 executable", head(le, mhMagic64, arm64, 0, mhExecute), "fev-mac-arm64"},
		{"arm64e executable", head(le, mhMagic64, arm64, arm64e, mhExecute), "fev-mac-arm64"},
		{"x86_64 executable", head(le, mhMagic64, x86_64, 3, mhExecute), "fev-mac-x86_64"},
		{"universal of 2", head(be, fatMagic, 2, x86_64, 3), "fev-mac-universal"},
		{"universal of 1", head(be, fatMagic, 1, arm64, 0), "fev-mac-universal"},
		{"universal of 15", head(be, fatMagic, 15, arm64, 0), "fev-mac-universal"},
		{"universal64 of 3", head(be, fatMagic64, 3, arm64, 0), "fev-mac-universal64"},
		{"arm64 dylib", head(le, mhMagic64, arm64, 0, mhDylib), ""},
		{"arm64 bundle", head(le, mhMagic64, arm64, 0, mhBundle), ""},
		{"arm64 object", head(le, mhMagic64, arm64, 0, mhObject), ""},
		{"big-endian Mach-O", head(be, mhMagic64, arm64, 0, mhExecute), ""},
		{"universal of 16", head(be, fatMagic, 16, arm64, 0), ""},
		{"ELF aarch64", []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0xb7, 0}, ""},
		{"ELF x86_64", []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0x3e, 0}, ""},
		{"a script", []byte("#!/bin/sh\necho hi\n"), ""},
		{"too short", []byte{0xca, 0xfe, 0xba}, ""},
	} {
		e, ok := Matching(c.head)
		if got := map[bool]string{true: e.Name}[ok]; got != c.want {
			t.Errorf("%s (% x): taken by %q, want %q", c.what, c.head, got, c.want)
		}
	}
	if runtime.GOOS == "darwin" {
		for _, p := range []string{"/bin/echo", "/usr/bin/sw_vers", "/bin/sh"} {
			f, err := os.Open(p)
			if err != nil {
				continue
			}
			b := make([]byte, 256)
			n, _ := f.Read(b)
			f.Close()
			if _, ok := Matching(b[:n]); !ok {
				t.Errorf("%s (% x) is taken by no entry", p, b[:16])
			}
		}
	}
}

func TestNoJavaClassFileIsTaken(t *testing.T) {
	for major := 45; major <= 0xffff; major++ {
		for _, minor := range []int{0, 3, 0xffff} {
			b := []byte{0xca, 0xfe, 0xba, 0xbe, byte(minor >> 8), byte(minor), byte(major >> 8), byte(major), 0, 0x1d}
			if e, ok := Matching(b); ok {
				t.Fatalf("a class file of version %d.%d is taken by %s", major, minor, e.Name)
			}
		}
	}
	for major := 0; major < 45; major++ {
		b := []byte{0xca, 0xfe, 0xba, 0xbe, 0, 0, byte(major >> 8), byte(major)}
		if _, ok := Matching(b); ok != (major < 16) {
			t.Errorf("version 0.%d: taken %v", major, ok)
		}
	}
}

func TestTheRegisterLine(t *testing.T) {
	got := Entries[2].Line("/system/opt/fev/fev-macexec")
	want := `:fev-mac-universal:M:0:\xca\xfe\xba\xbe\x00\x00\x00\x00:\xff\xff\xff\xff\xff\xff\xff\xf0:/system/opt/fev/fev-macexec:PF`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	for _, e := range Entries {
		if len(e.Mask) != len(e.Magic) {
			t.Errorf("%s: a mask of %d bytes for a magic of %d", e.Name, len(e.Mask), len(e.Magic))
		}
		for i := range e.Magic {
			if e.Magic[i]&^e.Mask[i] != 0 {
				t.Errorf("%s: byte %d of the magic is outside its mask", e.Name, i)
			}
		}
		if e.Offset+len(e.Magic) > 128 {
			t.Errorf("%s: binfmt_misc reads 128 bytes at most for a magic", e.Name)
		}
	}
}
