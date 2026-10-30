package trust

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fev/proto/agent"
	"fev/proto/daemon"
)

func csrFor(t *testing.T, names ...string) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "ignored"}, DNSNames: names}, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func allowAll(string) error { return nil }

func TestTheRootIsMadeAsDecided(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ca, err := OpenFileCA(t.TempDir(), RootOptions{User: "dev", Host: "mac", Constrain: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	r := ca.Root
	if !r.IsCA || !r.BasicConstraintsValid || r.MaxPathLen != 0 || !r.MaxPathLenZero {
		t.Errorf("basic constraints: CA %v, path length %d (zero %v)", r.IsCA, r.MaxPathLen, r.MaxPathLenZero)
	}
	if r.KeyUsage != x509.KeyUsageCertSign|x509.KeyUsageCRLSign {
		t.Errorf("key usage %v", r.KeyUsage)
	}
	if len(r.SubjectKeyId) == 0 {
		t.Error("no subject key identifier")
	}
	if pub, ok := r.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		t.Errorf("key %T", r.PublicKey)
	}
	if r.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Errorf("signature %v", r.SignatureAlgorithm)
	}
	if got := r.NotAfter.Sub(now); got != RootLifetime {
		t.Errorf("lifetime %v", got)
	}
	if !strings.Contains(r.Subject.CommonName, "dev@mac") || !strings.Contains(r.Subject.OrganizationalUnit[0], "2026-10-16") {
		t.Errorf("subject %v", r.Subject)
	}
	if !r.PermittedDNSDomainsCritical || len(r.PermittedDNSDomains) != 1 || r.PermittedDNSDomains[0] != "local" {
		t.Errorf("name constraints %v (critical %v)", r.PermittedDNSDomains, r.PermittedDNSDomainsCritical)
	}
	for _, e := range r.Extensions {
		switch {
		case e.Id.Equal([]int{2, 5, 29, 19}), e.Id.Equal([]int{2, 5, 29, 15}), e.Id.Equal([]int{2, 5, 29, 30}):
			if !e.Critical {
				t.Errorf("extension %v is not critical", e.Id)
			}
		}
	}
	if err := r.CheckSignatureFrom(r); err != nil {
		t.Errorf("not self-signed: %v", err)
	}
	if ca.Key != daemon.KeyFile || len(Fingerprint(r.Raw)) != 64 {
		t.Errorf("key %q, fingerprint %q", ca.Key, Fingerprint(r.Raw))
	}
}

func TestTheFileCAOpensAgain(t *testing.T) {
	dir := t.TempDir()
	a, err := OpenFileCA(dir, RootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(dir, fileKey)); st.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Join(dir, fileRoot)); st.Mode().Perm() != 0o644 {
		t.Errorf("root mode %v", st.Mode().Perm())
	}
	b, err := OpenFileCA(dir, RootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(a.Root.Raw) != Fingerprint(b.Root.Raw) {
		t.Error("a second open made another root")
	}
	other, _ := OpenFileCA(t.TempDir(), RootOptions{})
	WriteRootPEM(filepath.Join(dir, fileRoot), other.Root)
	if _, err := OpenFileCA(dir, RootOptions{}); err == nil {
		t.Error("a root of another key was taken")
	}
}

func TestALeafIsMadeAsDecided(t *testing.T) {
	now := time.Now()
	ca, err := OpenFileCA(t.TempDir(), RootOptions{Constrain: true})
	if err != nil {
		t.Fatal(err)
	}
	ca.Now = func() time.Time { return now }
	long := strings.Repeat("a", 60) + ".web.fev.local"
	for _, c := range []struct {
		names    []string
		lifetime time.Duration
	}{
		{[]string{"web.fev.local"}, agent.LeafLifetime},
		{[]string{"api.shop.fev.local", "shop-custom.local"}, agent.ContainerLeafLifetime},
		{[]string{long}, agent.LeafLifetime},
	} {
		certPEM, notAfter, err := ca.Issue(csrFor(t, c.names...), c.lifetime, allowAll)
		if err != nil {
			t.Fatalf("%v: %v", c.names, err)
		}
		leaf, err := ParsePEM([]byte(certPEM))
		if err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		pool.AddCert(ca.Root)
		for _, n := range c.names {
			if _, err := leaf.Verify(x509.VerifyOptions{DNSName: n, Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
				t.Errorf("%s does not verify: %v", n, err)
			}
		}
		if leaf.IsCA || leaf.KeyUsage != x509.KeyUsageDigitalSignature || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
			t.Errorf("%v: CA %v, usage %v %v", c.names, leaf.IsCA, leaf.KeyUsage, leaf.ExtKeyUsage)
		}
		if string(leaf.AuthorityKeyId) != string(ca.Root.SubjectKeyId) {
			t.Error("the leaf's authority key identifier is not the root's")
		}
		if leaf.SerialNumber.BitLen() < 120 || leaf.SerialNumber.BitLen() > 128 {
			t.Errorf("serial of %d bits", leaf.SerialNumber.BitLen())
		}
		if got := now.Sub(leaf.NotBefore); got < agent.LeafBackdate-time.Second || got > agent.LeafBackdate+time.Second {
			t.Errorf("notBefore %v back", got)
		}
		if !notAfter.Equal(leaf.NotAfter) || leaf.NotAfter.Sub(now) > c.lifetime || leaf.NotAfter.Sub(now) < c.lifetime-time.Second {
			t.Errorf("notAfter %v for %v", leaf.NotAfter.Sub(now), c.lifetime)
		}
		wantCN := c.names[0]
		if len(wantCN) > MaxCNLen {
			wantCN = ""
		}
		if leaf.Subject.CommonName != wantCN {
			t.Errorf("CN %q", leaf.Subject.CommonName)
		}
	}
}

func TestTheConstraintKeepsNamesUnderLocal(t *testing.T) {
	ca, _ := OpenFileCA(t.TempDir(), RootOptions{Constrain: true})
	certPEM, _, err := ca.Issue(csrFor(t, "example.com"), agent.LeafLifetime, allowAll)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := ParsePEM([]byte(certPEM))
	pool := x509.NewCertPool()
	pool.AddCert(ca.Root)
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "example.com", Roots: pool}); err == nil {
		t.Error("a leaf for example.com verified under a root constrained to .local")
	}
}

func TestALeafIsRefusedForAnyNameNotAllowed(t *testing.T) {
	ca, _ := OpenFileCA(t.TempDir(), RootOptions{})
	refuse := func(n string) error {
		if n == "evil.example" {
			return errors.New("not registered")
		}
		return nil
	}
	if _, _, err := ca.Issue(csrFor(t, "web.fev.local", "evil.example"), agent.LeafLifetime, refuse); err == nil {
		t.Error("a leaf was signed with a refused name in it")
	}
	if _, _, err := ca.Issue("not a csr", agent.LeafLifetime, allowAll); err == nil {
		t.Error("a non-request was signed")
	}
	ca.Now = func() time.Time { return ca.Root.NotAfter.Add(-time.Hour) }
	certPEM, _, err := ca.Issue(csrFor(t, "web.fev.local"), agent.LeafLifetime, allowAll)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := ParsePEM([]byte(certPEM))
	if leaf.NotAfter.After(ca.Root.NotAfter) {
		t.Error("a leaf outlives its root")
	}
}
