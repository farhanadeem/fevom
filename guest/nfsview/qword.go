package nfsview

import (
	"errors"
	"strings"
)

func Quote(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c == '\\' || c == 0x7f {
			b.WriteByte('\\')
			b.WriteByte('0' + c>>6)
			b.WriteByte('0' + (c>>3)&7)
			b.WriteByte('0' + c&7)
			continue
		}
		b.WriteByte(c)
	}
	if b.Len() == 0 {
		return `\000`
	}
	return b.String()
}

var errHexWord = errors.New("nfsview: malformed hex word")

func Unquote(w string) (string, error) {
	if strings.HasPrefix(w, `\x`) {
		h := w[2:]
		if len(h)%2 != 0 {
			return "", errHexWord
		}
		out := make([]byte, len(h)/2)
		for i := range out {
			hi, lo := unhex(h[2*i]), unhex(h[2*i+1])
			if hi < 0 || lo < 0 {
				return "", errHexWord
			}
			out[i] = byte(hi<<4 | lo)
		}
		return string(out), nil
	}
	var b strings.Builder
	for i := 0; i < len(w); i++ {
		if w[i] == '\\' && i+3 < len(w) &&
			w[i+1] >= '0' && w[i+1] <= '3' && isOctal(w[i+2]) && isOctal(w[i+3]) {
			b.WriteByte((w[i+1]-'0')<<6 | (w[i+2]-'0')<<3 | (w[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(w[i])
	}
	return b.String(), nil
}

func Hex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 2, 2+2*len(b))
	out[0], out[1] = '\\', 'x'
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

func Fields(line string) []string { return strings.Fields(line) }

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
