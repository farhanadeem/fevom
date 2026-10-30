package viewhops

import (
	"encoding/binary"
	"slices"
	"testing"
)

func record(xid uint32, frags ...int) []byte {
	var out []byte
	for i, n := range frags {
		mark := uint32(n)
		if i == len(frags)-1 {
			mark |= 0x80000000
		}
		out = binary.BigEndian.AppendUint32(out, mark)
		body := make([]byte, n)
		if i == 0 {
			binary.BigEndian.PutUint32(body, xid)
		}
		out = append(out, body...)
	}
	return out
}

func TestMarksFindEveryXid(t *testing.T) {
	var s []byte
	s = append(s, record(1, 40)...)
	s = append(s, record(2, 4200)...)
	s = append(s, record(3, 100, 9, 4)...)
	s = append(s, record(4, 8)...)
	for _, cut := range []int{1, 3, 7, 8, 64, 5000, len(s)} {
		var m Marks
		var got []uint32
		for p := s; len(p) > 0; {
			n := min(cut, len(p))
			m.Feed(p[:n], func(x, _ uint32, _ []byte) { got = append(got, x) })
			p = p[n:]
		}
		if !slices.Equal(got, []uint32{1, 2, 3, 4}) {
			t.Errorf("cut %d: %v", cut, got)
		}
	}
}
