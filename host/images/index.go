package images

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"fev/proto/agent"
)

type Entry struct {
	agent.ImageRef
	Path string
}

const serialLayout = "20060102_15:04"

func ParseSerial(serial string) (time.Time, error) {
	t, err := time.Parse(serialLayout, serial)
	if err != nil {
		return time.Time{}, fmt.Errorf("the serial %q is not a build's (YYYYMMDD_HH:MM)", serial)
	}
	return t, nil
}

func pathPart(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-', c == ':':
		default:
			return false
		}
	}
	return true
}

func ParseIndex(data []byte) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := strings.Split(line, ";")
		if len(f) != 6 {
			return nil, fmt.Errorf("index-system, line %d: %d fields, not 6: %q", n, len(f), line)
		}
		for _, part := range f[:5] {
			if !pathPart(part) {
				return nil, fmt.Errorf("index-system, line %d: %q is not a plain name", n, part)
			}
		}
		e := Entry{ImageRef: agent.ImageRef{Distro: f[0], Release: f[1], Arch: f[2], Variant: f[3], Serial: f[4]}, Path: f[5]}
		if want := "/images/" + strings.Join(f[:5], "/") + "/"; e.Path != want {
			return nil, fmt.Errorf("index-system, line %d: the path %q is not %q", n, e.Path, want)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("index-system: %w", err)
	}
	return out, nil
}

func Newest(entries []Entry, distro, release, arch, variant string) (Entry, bool) {
	var best Entry
	var bestAt time.Time
	found := false
	for _, e := range entries {
		if e.Distro != distro || e.Release != release || e.Arch != arch || e.Variant != variant {
			continue
		}
		at, err := ParseSerial(e.Serial)
		if err != nil {
			continue
		}
		if !found || at.After(bestAt) {
			best, bestAt, found = e, at, true
		}
	}
	return best, found
}

func Releases(entries []Entry, distro, arch, variant string) []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Distro == distro && e.Arch == arch && e.Variant == variant && !seen[e.Release] {
			seen[e.Release] = true
			out = append(out, e.Release)
		}
	}
	return out
}

const maxSums = 64 << 10

func ParseSums(data []byte) (map[string][]byte, error) {
	out := map[string][]byte{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		sum, name, ok := strings.Cut(line, " ")
		name = strings.TrimPrefix(strings.TrimLeft(name, " "), "*")
		digest, err := hex.DecodeString(sum)
		if !ok || err != nil || len(digest) != 32 || name == "" {
			return nil, fmt.Errorf("SHA256SUMS, line %d: not a sha256 and a name: %q", n, line)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("SHA256SUMS, line %d: %s is listed twice", n, name)
		}
		out[name] = digest
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("SHA256SUMS: %w", err)
	}
	return out, nil
}
