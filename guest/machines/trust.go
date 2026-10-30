package machines

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"strings"
)

const TrustedFile = "/var/lib/fev/trusted-roots"

const trustPrefix = "fev-"

type trustTool struct {
	dir, ext string
	argv     []string
}

var (
	updateCACertificates = []string{"/usr/sbin/update-ca-certificates", "/usr/bin/update-ca-certificates", "/sbin/update-ca-certificates", "/bin/update-ca-certificates"}
	updateCATrust        = []string{"/usr/bin/update-ca-trust", "/usr/sbin/update-ca-trust", "/bin/update-ca-trust", "/sbin/update-ca-trust"}
)

const (
	localCertificates = "/usr/local/share/ca-certificates"
	archAnchors       = "/etc/ca-certificates/trust-source/anchors"
	archSource        = "/etc/ca-certificates/trust-source"
	fedoraAnchors     = "/etc/pki/ca-trust/source/anchors"
	suseTrust         = "/etc/pki/trust"
	suseAnchors       = "/etc/pki/trust/anchors"
)

func chooseTrustTool(has func(string) bool) (t trustTool, ok bool) {
	for _, p := range updateCACertificates {
		if has(p) {
			if has(suseTrust) {
				return trustTool{dir: suseAnchors, ext: ".pem", argv: []string{p}}, true
			}
			return trustTool{dir: localCertificates, ext: ".crt", argv: []string{p}}, true
		}
	}
	for _, p := range updateCATrust {
		if has(p) {
			dir := fedoraAnchors
			if has(archSource) {
				dir = archAnchors
			}
			return trustTool{dir: dir, ext: ".pem", argv: []string{p}}, true
		}
	}
	return trustTool{dir: localCertificates, ext: ".crt"}, false
}

func trustFileName(der []byte, ext string) string {
	sum := sha256.Sum256(der)
	return trustPrefix + hex.EncodeToString(sum[:8]) + ext
}

type trustPlan struct {
	write  map[string][]byte
	remove []string
}

func planTrust(ders [][]byte, ext string, present []string) trustPlan {
	p := trustPlan{write: map[string][]byte{}}
	for _, d := range ders {
		p.write[trustFileName(d, ext)] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})
	}
	for _, name := range present {
		if _, keep := p.write[name]; !keep && strings.HasPrefix(name, trustPrefix) && (strings.HasSuffix(name, ".crt") || strings.HasSuffix(name, ".pem")) {
			p.remove = append(p.remove, name)
		}
	}
	return p
}

func parseTrustPEM(b []byte) [][]byte {
	var ders [][]byte
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return ders
		}
		if blk.Type == "CERTIFICATE" {
			ders = append(ders, blk.Bytes)
		}
	}
}
