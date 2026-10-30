package trust

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fev/proto/agent"
	"fev/proto/daemon"
)

const RootLifetime = 10 * 365 * 24 * time.Hour

const MaxCNLen = 64

type CA struct {
	Root          *x509.Certificate
	Signer        crypto.Signer
	Key, Identity string
	Now           func() time.Time
	release       func()
}

func (ca *CA) close() {
	if ca.release != nil {
		ca.release()
		ca.release = nil
	}
}

type RootOptions struct {
	User, Host string
	Constrain  bool
	Now        time.Time
}

func NewRoot(key crypto.Signer, o RootOptions) (*x509.Certificate, error) {
	pub, ok := key.Public().(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, errors.New("the CA's key is not P-256")
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	ski, err := keyID(pub)
	if err != nil {
		return nil, err
	}
	cn := "fev development CA"
	if o.User != "" || o.Host != "" {
		cn = fmt.Sprintf("fev development CA %s@%s", o.User, o.Host)
	}
	t := &x509.Certificate{
		SerialNumber: serial(),
		Subject: pkix.Name{
			CommonName:         cn,
			Organization:       []string{"fev"},
			OrganizationalUnit: []string{fmt.Sprintf("%s@%s %s", o.User, o.Host, now.UTC().Format("2006-01-02"))},
		},
		NotBefore:             now.Add(-agent.LeafBackdate),
		NotAfter:              now.Add(RootLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		SubjectKeyId:          ski,
	}
	if o.Constrain {
		t.PermittedDNSDomainsCritical = true
		t.PermittedDNSDomains = []string{"local"}
	}
	der, err := x509.CreateCertificate(rand.Reader, t, t, pub, key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

func keyID(pub *ecdsa.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	var spki struct {
		Algorithm pkix.AlgorithmIdentifier
		Key       asn1.BitString
	}
	if _, err := asn1.Unmarshal(der, &spki); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(spki.Key.Bytes)
	return sum[:20], nil
}

func serial() *big.Int {
	b := make([]byte, 16)
	rand.Read(b)
	b[0] &= 0x7f
	b[0] |= 0x40
	return new(big.Int).SetBytes(b)
}

func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func (ca *CA) RootPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Root.Raw}))
}

func (ca *CA) now() time.Time {
	if ca.Now != nil {
		return ca.Now()
	}
	return time.Now()
}

func (ca *CA) Issue(csrPEM string, lifetime time.Duration, allowed func(name string) error) (certPEM string, notAfter time.Time, err error) {
	names, err := agent.CSRNames(csrPEM)
	if err != nil {
		return "", time.Time{}, err
	}
	for _, n := range names {
		if err := allowed(n); err != nil {
			return "", time.Time{}, err
		}
	}
	b, _ := pem.Decode([]byte(csrPEM))
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return "", time.Time{}, err
	}
	now := ca.now().Truncate(time.Second)
	t := &x509.Certificate{
		SerialNumber:          serial(),
		DNSNames:              names,
		NotBefore:             now.Add(-agent.LeafBackdate),
		NotAfter:              now.Add(lifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		AuthorityKeyId:        ca.Root.SubjectKeyId,
	}
	if len(names[0]) <= MaxCNLen {
		t.Subject.CommonName = names[0]
	}
	if t.NotAfter.After(ca.Root.NotAfter) {
		t.NotAfter = ca.Root.NotAfter
	}
	der, err := x509.CreateCertificate(rand.Reader, t, ca.Root, csr.PublicKey, ca.Signer)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), t.NotAfter, nil
}

const (
	fileKey  = "ca.key"
	fileRoot = "ca.pem"
)

func OpenFileCA(dir string, o RootOptions) (*CA, error) {
	keyPath, rootPath := filepath.Join(dir, fileKey), filepath.Join(dir, fileRoot)
	if kb, err := os.ReadFile(keyPath); err == nil {
		b, _ := pem.Decode(kb)
		if b == nil {
			return nil, fmt.Errorf("%s: not PEM", keyPath)
		}
		k, err := x509.ParseECPrivateKey(b.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", keyPath, err)
		}
		rb, err := os.ReadFile(rootPath)
		if err != nil {
			return nil, err
		}
		root, err := ParsePEM(rb)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rootPath, err)
		}
		if !root.PublicKey.(*ecdsa.PublicKey).Equal(k.Public()) {
			return nil, fmt.Errorf("%s is not %s's root", rootPath, keyPath)
		}
		return &CA{Root: root, Signer: k, Key: daemon.KeyFile}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	root, err := NewRoot(k, o)
	if err != nil {
		return nil, err
	}
	kder, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		return nil, err
	}
	if err := WriteRootPEM(rootPath, root); err != nil {
		return nil, err
	}
	return &CA{Root: root, Signer: k, Key: daemon.KeyFile}, nil
}

func WriteRootPEM(path string, root *x509.Certificate) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func ParsePEM(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

func canonicalName(n string) string { return strings.ToLower(strings.TrimSuffix(n, ".")) }
