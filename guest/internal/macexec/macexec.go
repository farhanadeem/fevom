package macexec

import (
	"fmt"
	"strings"
)

const Flags = "PF"

type Entry struct {
	Name        string
	Offset      int
	Magic, Mask []byte
}

var (
	machoMk = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}
	fatMask = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xf0}
)

var Entries = []Entry{
	{Name: "fev-mac-arm64", Magic: []byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0, 0, 0x01, 0, 0, 0, 0, 0x02, 0, 0, 0}, Mask: machoMk},
	{Name: "fev-mac-x86_64", Magic: []byte{0xcf, 0xfa, 0xed, 0xfe, 0x07, 0, 0, 0x01, 0, 0, 0, 0, 0x02, 0, 0, 0}, Mask: machoMk},
	{Name: "fev-mac-universal", Magic: []byte{0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 0}, Mask: fatMask},
	{Name: "fev-mac-universal64", Magic: []byte{0xca, 0xfe, 0xba, 0xbf, 0, 0, 0, 0}, Mask: fatMask},
}

func (e Entry) Line(interp string) string {
	return fmt.Sprintf(":%s:M:%d:%s:%s:%s:%s", e.Name, e.Offset, escape(e.Magic), escape(e.Mask), interp, Flags)
}

func escape(b []byte) string {
	var s strings.Builder
	for _, c := range b {
		fmt.Fprintf(&s, "\\x%02x", c)
	}
	return s.String()
}

func (e Entry) Matches(head []byte) bool {
	if len(head) < e.Offset+len(e.Magic) {
		return false
	}
	for i, m := range e.Magic {
		mask := byte(0xff)
		if e.Mask != nil {
			mask = e.Mask[i]
		}
		if (head[e.Offset+i]^m)&mask != 0 {
			return false
		}
	}
	return true
}

func Matching(head []byte) (Entry, bool) {
	for _, e := range Entries {
		if e.Matches(head) {
			return e, true
		}
	}
	return Entry{}, false
}
