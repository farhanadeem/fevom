package netstack

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"strings"
)

const sniffMax = 16 << 10

type sniffResult int

const (
	sniffMore sniffResult = iota
	sniffDone
)

func sniffName(b []byte) (name, kind string, res sniffResult) {
	if len(b) == 0 {
		return "", "", sniffMore
	}
	if b[0] == 0x16 {
		name, res := sniffSNI(b)
		return name, "tls", res
	}
	if looksLikeHTTP(b) {
		name, res := sniffHost(b)
		return name, "http", res
	}
	return "", "", sniffDone
}

func sniffSNI(b []byte) (string, sniffResult) {
	if len(b) < 5 {
		return "", sniffMore
	}
	if b[1] != 3 {
		return "", sniffDone
	}
	n := int(binary.BigEndian.Uint16(b[3:5]))
	if n > sniffMax {
		return "", sniffDone
	}
	if len(b) < 5+n {
		return "", sniffMore
	}
	h := b[5 : 5+n]
	if len(h) < 4 || h[0] != 1 {
		return "", sniffDone
	}
	hl := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
	if hl > len(h)-4 {
		return "", sniffDone
	}
	p := h[4 : 4+hl]
	skip := func(k int) bool {
		if len(p) < k {
			return false
		}
		p = p[k:]
		return true
	}
	vec := func(lenBytes int) bool {
		if len(p) < lenBytes {
			return false
		}
		var l int
		if lenBytes == 1 {
			l = int(p[0])
		} else {
			l = int(binary.BigEndian.Uint16(p))
		}
		return skip(lenBytes + l)
	}
	if !skip(2+32) || !vec(1) || !vec(2) || !vec(1) {
		return "", sniffDone
	}
	if len(p) < 2 {
		return "", sniffDone
	}
	ext := p[2:]
	if el := int(binary.BigEndian.Uint16(p)); el <= len(ext) {
		ext = ext[:el]
	}
	for len(ext) >= 4 {
		typ := binary.BigEndian.Uint16(ext)
		l := int(binary.BigEndian.Uint16(ext[2:]))
		if 4+l > len(ext) {
			break
		}
		body := ext[4 : 4+l]
		ext = ext[4+l:]
		if typ != 0 {
			continue
		}
		if len(body) < 2 {
			break
		}
		list := body[2:]
		for len(list) >= 3 {
			nt := list[0]
			nl := int(binary.BigEndian.Uint16(list[1:]))
			if 3+nl > len(list) {
				break
			}
			if nt == 0 {
				return validName(string(list[3 : 3+nl])), sniffDone
			}
			list = list[3+nl:]
		}
		break
	}
	return "", sniffDone
}

func looksLikeHTTP(b []byte) bool {
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			if i > 16 {
				return false
			}
		case c == ' ':
			return i > 0
		default:
			return false
		}
	}
	return true
}

func sniffHost(b []byte) (string, sniffResult) {
	end := bytes.Index(b, []byte("\r\n\r\n"))
	if end < 0 {
		if len(b) >= sniffMax {
			return "", sniffDone
		}
		if name, ok := hostLine(b); ok {
			return name, sniffDone
		}
		return "", sniffMore
	}
	name, _ := hostLine(b[:end+2])
	return name, sniffDone
}

func hostLine(b []byte) (string, bool) {
	lines := bytes.Split(b, []byte("\r\n"))
	if len(lines) < 3 {
		return "", false
	}
	for _, l := range lines[1 : len(lines)-1] {
		k, v, ok := bytes.Cut(l, []byte(":"))
		if !ok || !strings.EqualFold(string(k), "Host") {
			continue
		}
		h := strings.TrimSpace(string(v))
		if host, _, err := net.SplitHostPort(h); err == nil {
			h = host
		}
		return validName(h), true
	}
	return "", false
}

func validName(s string) string {
	s = strings.ToLower(strings.TrimSuffix(s, "."))
	if s == "" || len(s) > 253 {
		return ""
	}
	if _, err := netip.ParseAddr(strings.Trim(s, "[]")); err == nil {
		return ""
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return ""
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return ""
			}
		}
	}
	return s
}
