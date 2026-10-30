package daemon

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fev/proto/agent"
	api "fev/proto/daemon"
)

func scratchRoot(t *testing.T, name string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestTheTrustFileGivesItsCertificates(t *testing.T) {
	dir := t.TempDir()
	a, b := scratchRoot(t, "a"), scratchRoot(t, "b")
	file := filepath.Join(dir, "roots.pem")
	content := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("key")})...)
	content = append(content, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b})...)
	os.WriteFile(file, content, 0o644)
	roots, err := TrustFromFile(file)()
	if err != nil || len(roots) != 2 {
		t.Fatalf("%d roots, %v", len(roots), err)
	}
	ders, err := agent.TrustDER(roots)
	if err != nil || string(ders[0]) != string(a) || string(ders[1]) != string(b) {
		t.Fatalf("the roots given: %v", err)
	}
	os.WriteFile(file, nil, 0o644)
	if roots, err := TrustFromFile(file)(); err != nil || len(roots) != 0 {
		t.Errorf("an empty file: %d roots, %v", len(roots), err)
	}
	os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not one")}), 0o644)
	if _, err := TrustFromFile(file)(); err == nil {
		t.Error("a certificate that does not parse was given")
	}
}

func TestTheRootsAreGivenOnceABootBeforeAnyMachine(t *testing.T) {
	der := scratchRoot(t, "fev scratch root")
	var mu sync.Mutex
	reads := 0
	fail := false
	var runningAtRead []string
	f := machinesDaemon(t, func(m *Machines) {
		m.Trust = func() ([]string, error) {
			var running []string
			m.mu.Lock()
			for _, e := range m.entries {
				if e.State == api.MachineRunning || e.State == api.MachineStarting {
					running = append(running, e.Name)
				}
			}
			m.mu.Unlock()
			mu.Lock()
			defer mu.Unlock()
			runningAtRead = append(runningAtRead, running...)
			reads++
			if fail {
				return nil, errors.New("the keychain is not there")
			}
			return []string{string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}, nil
		}
	})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return reads
	}
	if count() != 0 {
		t.Fatalf("read before any boot: %d", count())
	}
	f.create("dev", "ubuntu")
	if count() != 1 {
		t.Fatalf("a create's boot read the roots %d times", count())
	}
	want := "digest=" + agent.TrustDigest([][]byte{der})[:12]
	log := f.logFile("fevd.log")
	if !strings.Contains(log, "the Mac's trusted roots given to the guest") || !strings.Contains(log, "count=1") || !strings.Contains(log, want) {
		t.Fatalf("the daemon's log does not say the roots were given (%s):\n%s", want, log)
	}
	if !strings.Contains(f.logFile("guest.log"), "the Mac's trusted roots: 1 (") {
		t.Errorf("the guest does not say it took them:\n%s", f.logFile("guest.log"))
	}
	if _, err := f.c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	mu.Lock()
	fail = true
	mu.Unlock()
	if _, err := f.c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	f.machineIs("dev", api.MachineRunning)
	if count() != 2 {
		t.Fatalf("two boots read the roots %d times", count())
	}
	if !strings.Contains(f.logFile("fevd.log"), "the Mac's trusted roots could not be read") {
		t.Error("a failed read is not said")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(runningAtRead) != 0 {
		t.Errorf("machines ran when the roots were read: %v", runningAtRead)
	}
}
