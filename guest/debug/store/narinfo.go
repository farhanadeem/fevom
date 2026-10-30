package store

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"fev/proto/debugstore"
)

const StoreDir = debugstore.StoreDir

const (
	MaxNarBytes     = debugstore.MaxNarBytes
	MaxClosurePaths = debugstore.MaxClosurePaths
	MaxNarDepth     = debugstore.MaxNarDepth
)

type narinfo struct {
	StorePath   string
	URL         string
	Compression string
	FileSize    int64
	NarHash     string
	NarSize     int64
	References  []string
	Sigs        []string
	raw         string
}

func base(p string) string { return strings.TrimPrefix(p, StoreDir+"/") }

func hashOf(b string) string {
	if i := strings.IndexByte(b, '-'); i > 0 {
		return b[:i]
	}
	return b
}

func parseNarinfo(s string) (*narinfo, error) {
	n := &narinfo{raw: s}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch k {
		case "StorePath":
			n.StorePath = v
		case "URL":
			n.URL = v
		case "Compression":
			n.Compression = v
		case "FileSize":
			n.FileSize, _ = strconv.ParseInt(v, 10, 64)
		case "NarHash":
			n.NarHash = v
		case "NarSize":
			n.NarSize, _ = strconv.ParseInt(v, 10, 64)
		case "References":
			if v != "" {
				n.References = strings.Fields(v)
			}
		case "Sig":
			n.Sigs = append(n.Sigs, v)
		}
	}
	if n.StorePath == "" || n.URL == "" || n.NarHash == "" {
		return nil, errors.New("narinfo lacks StorePath, URL or NarHash")
	}
	if strings.HasPrefix(n.URL, "/") || strings.Contains(n.URL, "://") || strings.Contains(n.URL, "..") {
		return nil, fmt.Errorf("narinfo URL %q", n.URL)
	}
	if !validBase(base(n.StorePath)) || !strings.HasPrefix(n.StorePath, StoreDir+"/") {
		return nil, fmt.Errorf("narinfo StorePath %q", n.StorePath)
	}
	for _, r := range n.References {
		if !validBase(r) {
			return nil, fmt.Errorf("narinfo reference %q", r)
		}
	}
	return n, nil
}

func validBase(b string) bool {
	if len(b) < 34 || b[32] != '-' {
		return false
	}
	for i := 0; i < 32; i++ {
		if !strings.ContainsRune(b32, rune(b[i])) {
			return false
		}
	}
	name := b[33:]
	if name[0] == '.' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("+-._?=", c) >= 0:
		default:
			return false
		}
	}
	return true
}

func (n *narinfo) fingerprint() string {
	refs := make([]string, len(n.References))
	for i, r := range n.References {
		refs[i] = StoreDir + "/" + r
	}
	return "1;" + n.StorePath + ";" + n.NarHash + ";" + strconv.FormatInt(n.NarSize, 10) + ";" + strings.Join(refs, ",")
}

type key struct {
	name string
	pub  ed25519.PublicKey
}

func parseKey(s string) (key, error) {
	name, b64, ok := strings.Cut(s, ":")
	if !ok {
		return key{}, errors.New("key is not NAME:BASE64")
	}
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return key{}, fmt.Errorf("key %s: bad public key", name)
	}
	return key{name, ed25519.PublicKey(b)}, nil
}

func (n *narinfo) verify(k key) error {
	for _, s := range n.Sigs {
		name, b64, ok := strings.Cut(s, ":")
		if !ok || name != k.name {
			continue
		}
		sig, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		if ed25519.Verify(k.pub, []byte(n.fingerprint()), sig) {
			return nil
		}
		return fmt.Errorf("%s: signature by %s does not verify", n.StorePath, name)
	}
	return fmt.Errorf("%s: no signature by %s", n.StorePath, k.name)
}

const b32 = "0123456789abcdfghijklmnpqrsvwxyz"

func nixBase32(h []byte) string {
	l := (len(h)*8-1)/5 + 1
	out := make([]byte, 0, l)
	for n := l - 1; n >= 0; n-- {
		b := n * 5
		i, j := b/8, uint(b%8)
		c := h[i] >> j
		if i+1 < len(h) {
			c |= h[i+1] << (8 - j)
		}
		out = append(out, b32[c&0x1f])
	}
	return string(out)
}
