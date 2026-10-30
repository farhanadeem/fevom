package trust

import (
	"bytes"
	"crypto/x509"
	"errors"
	"sort"
)

var ErrUnsupported = errors.New("the Mac's trust settings cannot be read in this build")

const (
	resultInvalid     = 0
	resultTrustRoot   = 1
	resultTrustAsRoot = 2
	resultDeny        = 3
	resultUnspecified = 4
)

type Setting struct {
	Policy, PolicySSL bool
	PolicyString      bool
	HasResult         bool
	Result            int32
}

func Decide(settings []Setting, selfSigned bool) bool {
	result := int32(resultUnspecified)
	if len(settings) == 0 {
		result = resultTrustRoot
	}
	for _, s := range settings {
		if s.Policy && !s.PolicySSL || s.PolicyString {
			continue
		}
		r := int32(resultTrustRoot)
		if s.HasResult {
			r = s.Result
		}
		if r == resultTrustRoot || r == resultTrustAsRoot || r == resultDeny {
			result = r
			break
		}
	}
	switch result {
	case resultTrustRoot:
		return selfSigned
	case resultTrustAsRoot:
		return !selfSigned
	}
	return false
}

type entry struct {
	der      []byte
	settings []Setting
}

func choose(admin, user []entry) []*x509.Certificate {
	byDER := map[string][]Setting{}
	for _, e := range admin {
		byDER[string(e.der)] = e.settings
	}
	for _, e := range user {
		byDER[string(e.der)] = e.settings
	}
	ders := make([]string, 0, len(byDER))
	for d := range byDER {
		ders = append(ders, d)
	}
	sort.Strings(ders)
	var out []*x509.Certificate
	for _, d := range ders {
		c, err := x509.ParseCertificate([]byte(d))
		if err != nil {
			continue
		}
		selfSigned := bytes.Equal(c.RawIssuer, c.RawSubject) && c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) == nil
		if Decide(byDER[d], selfSigned) {
			out = append(out, c)
		}
	}
	return out
}
