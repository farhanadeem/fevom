package trust

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

func newCert(t *testing.T, name string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	signer, signKey := tmpl, key
	if parent != nil {
		signer, signKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c, key
}

func TestDecideReadsTrustSettingsAsTheMacDoes(t *testing.T) {
	root := Setting{HasResult: true, Result: resultTrustRoot}
	asRoot := Setting{HasResult: true, Result: resultTrustAsRoot}
	deny := Setting{HasResult: true, Result: resultDeny}
	for _, c := range []struct {
		name       string
		settings   []Setting
		selfSigned bool
		want       bool
	}{
		{"always trust (empty)", nil, true, true},
		{"empty, not self-signed", nil, false, false},
		{"trust root", []Setting{root}, true, true},
		{"no result is trust root", []Setting{{}}, true, true},
		{"trust root, SSL policy", []Setting{{Policy: true, PolicySSL: true, HasResult: true, Result: resultTrustRoot}}, true, true},
		{"trust root for another policy", []Setting{{Policy: true, HasResult: true, Result: resultTrustRoot}}, true, false},
		{"trust root for one host", []Setting{{PolicyString: true, HasResult: true, Result: resultTrustRoot}}, true, false},
		{"trust as root, an intermediate", []Setting{asRoot}, false, true},
		{"trust as root, self-signed", []Setting{asRoot}, true, false},
		{"deny", []Setting{deny}, true, false},
		{"deny first", []Setting{deny, root}, true, false},
		{"unspecified", []Setting{{HasResult: true, Result: resultUnspecified}}, true, false},
		{"unspecified then root", []Setting{{HasResult: true, Result: resultUnspecified}, root}, true, true},
	} {
		if got := Decide(c.settings, c.selfSigned); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTheUsersSettingsOverruleTheAdmins(t *testing.T) {
	ca, caKey := newCert(t, "corp root", nil, nil)
	dev, _ := newCert(t, "dev root", nil, nil)
	mid, _ := newCert(t, "corp intermediate", ca, caKey)
	denied, _ := newCert(t, "denied by the user", nil, nil)
	admin := []entry{
		{der: ca.Raw},
		{der: denied.Raw},
		{der: mid.Raw, settings: []Setting{{HasResult: true, Result: resultTrustAsRoot}}},
		{der: []byte("not a certificate")},
	}
	user := []entry{
		{der: dev.Raw, settings: []Setting{{HasResult: true, Result: resultTrustRoot}}},
		{der: denied.Raw, settings: []Setting{{HasResult: true, Result: resultDeny}}},
		{der: ca.Raw},
	}
	got := choose(admin, user)
	names := map[string]bool{}
	for _, c := range got {
		names[c.Subject.CommonName] = true
	}
	if len(got) != 3 || !names["corp root"] || !names["dev root"] || !names["corp intermediate"] {
		t.Errorf("roots: %v", names)
	}
}
