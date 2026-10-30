//go:build darwin && cgo

package trust

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"fev/proto/agent"
	"fev/proto/daemon"
)

func scratchKeychain(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("security"); err != nil {
		t.Skip("no security(1)")
	}
	list := func() string {
		out, err := exec.Command("security", "list-keychains", "-d", "user").Output()
		if err != nil {
			t.Fatalf("list-keychains: %v", err)
		}
		return string(out)
	}
	before := list()
	var prev []string
	for _, f := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(before, -1) {
		prev = append(prev, f[1])
	}
	restore := func() {
		exec.Command("security", append([]string{"list-keychains", "-d", "user", "-s"}, prev...)...).Run()
	}
	dir := filepath.Join(t.TempDir(), "Library", "Keychains")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	kc := filepath.Join(dir, "fev-ca-test.keychain")
	if out, err := exec.Command("security", "create-keychain", "-p", "", kc).CombinedOutput(); err != nil {
		t.Fatalf("create-keychain: %v: %s", err, out)
	}
	restore()
	t.Cleanup(func() {
		exec.Command("security", "delete-keychain", kc).Run()
		restore()
		if after := list(); after != before {
			t.Errorf("the keychain search list was\n%s\nand is\n%s", before, after)
		}
	})
	exec.Command("security", "set-keychain-settings", kc).Run()
	if _, err := os.Stat(kc); err != nil {
		kc += "-db"
	}
	return kc
}

func exported(t *testing.T, pub *ecdsa.PublicKey, attempts []exportAttempt) []string {
	t.Helper()
	var leaks []string
	isPriv := func(b []byte) bool {
		d := new(big.Int).SetBytes(b)
		if d.Sign() == 0 || d.Cmp(elliptic.P256().Params().N) >= 0 {
			return false
		}
		x, _ := elliptic.P256().ScalarBaseMult(b)
		return x.Cmp(pub.X) == 0
	}
	for _, a := range attempts {
		if len(a.Data) == 0 {
			continue
		}
		leak := false
		if a.Way == "SecKeyCopyExternalRepresentation" && len(a.Data) == 97 && isPriv(a.Data[65:]) {
			leak = true
		}
		for i := 0; !leak && i+32 <= len(a.Data); i++ {
			leak = isPriv(a.Data[i : i+32])
		}
		if strings.HasPrefix(a.Way, "SecItemExport") && a.Status == 0 {
			leak = true
		}
		if leak {
			leaks = append(leaks, a.Way)
		}
	}
	return leaks
}

func signs(t *testing.T, s crypto.Signer) {
	t.Helper()
	digest := sha256.Sum256([]byte("fev"))
	sig, err := s.Sign(nil, digest[:], crypto.SHA256)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !ecdsa.VerifyASN1(s.Public().(*ecdsa.PublicKey), digest[:], sig) {
		t.Fatal("the signature does not verify")
	}
}

func TestTheCAKeyIsNeverExportable(t *testing.T) {
	kc := scratchKeychain(t)
	signer := keptSigner(t)
	ca, err := OpenKeychainCA(KeychainOptions{Keychain: kc, Label: "fev.test.ca", Signer: signer, RootPath: filepath.Join(t.TempDir(), "ca.pem"), AnyApplication: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ca.close()
	if ca.Key != daemon.KeySigner {
		t.Errorf("key kind %q", ca.Key)
	}
	s := ca.Signer.(*signerKey)
	signs(t, s)
	acl, err := s.p.acl()
	if err != nil {
		t.Fatal(err)
	}
	signers, partition := 0, false
	for _, line := range strings.Split(strings.TrimSpace(acl), "\n") {
		if strings.Contains(line, "ACLAuthorizationSign") {
			signers++
			if line != "apps=1 ACLAuthorizationSign" {
				t.Errorf("an entry that may sign grants more, or to more than the signer: %q", line)
			}
		}
		if strings.Contains(line, "ACLAuthorizationPartitionID") && strings.Contains(partitionIDs(t, line), "cdhash:"+cdhashOf(t, signer)) {
			partition = true
		}
		for _, wide := range []string{"ExportClear", "ExportWrapped", "ACLAuthorizationAny", "Decrypt", "Derive"} {
			if strings.Contains(line, wide) && !strings.Contains(line, "ChangeACL") {
				t.Errorf("an entry grants %s: %q", wide, line)
			}
		}
	}
	if signers != 1 || !partition {
		t.Errorf("not one entry granting sign, or no partition entry naming the signer's cdhash:\n%s", acl)
	}
	t.Logf("access list:\n%s", acl)
	attempts, err := s.p.probe()
	if err != nil || len(attempts) != 5 {
		t.Fatalf("the signer's export probe: %v, %d ways", err, len(attempts))
	}
	if leaks := exported(t, s.pub, attempts); len(leaks) > 0 {
		t.Errorf("the private key came out through %v", leaks)
	}
	for _, a := range attempts {
		t.Logf("%s: status %d, %d bytes", a.Way, a.Status, len(a.Data))
	}
	if _, _, err := findKey(kc, "fev.test.ca"); !errors.Is(err, errNotAdmitted) {
		t.Errorf("another program found the signer's key usable: %v", err)
	}

	if err := makeKeyAs(kc, "fev.test.trap", tagAny, false, true); err != nil {
		t.Fatal(err)
	}
	trap, _, err := findKey(kc, "fev.test.trap")
	if err != nil {
		t.Fatal(err)
	}
	if leaks := exported(t, trap.pub, probeExport(trap, kc, "fev.test.trap")); len(leaks) == 0 {
		t.Error("the trap's key did not come out: the export check cannot tell a leaking key")
	} else {
		t.Logf("the trap leaks through %v", leaks)
	}
}

func TestABoundKeyGrantsSignToItsBinaryAlone(t *testing.T) {
	kc := scratchKeychain(t)
	if err := makeKey(kc, "fev.test.bound", tagIdentity+"test", true); err != nil {
		t.Fatal(err)
	}
	s, tag, err := findKey(kc, "fev.test.bound")
	if err != nil {
		t.Fatal(err)
	}
	if tag != tagIdentity+"test" {
		t.Errorf("tag %q", tag)
	}
	signs(t, s)
	acl, _ := probeACL(s)
	if !strings.Contains(acl, "apps=1 ACLAuthorizationSign\n") {
		t.Errorf("no entry granting sign alone to this binary:\n%s", acl)
	}
	if strings.Contains(acl, "any ACLAuthorizationSign") {
		t.Errorf("an entry lets any application sign:\n%s", acl)
	}
	if leaks := exported(t, s.pub, probeExport(s, kc, "fev.test.bound")); len(leaks) > 0 {
		t.Errorf("the private key came out through %v", leaks)
	}
}

func TestTheKeyIsFoundAgainAndNoSecondKeyIsMade(t *testing.T) {
	kc := scratchKeychain(t)
	dir := t.TempDir()
	o := KeychainOptions{Keychain: kc, Label: "fev.test.again", Signer: keptSigner(t), RootPath: filepath.Join(dir, "ca.pem"), AnyApplication: true}
	a, err := OpenKeychainCA(o)
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenKeychainCA(o)
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(a.Root.Raw) != Fingerprint(b.Root.Raw) {
		t.Error("a second open made another root")
	}
	os.Remove(o.RootPath)
	c, err := OpenKeychainCA(o)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Root.PublicKey.(*ecdsa.PublicKey).Equal(a.Signer.Public()) {
		t.Error("a lost root brought a new key")
	}
	certPEM, _, err := c.Issue(csrFor(t, "web.fev.local"), agent.LeafLifetime, allowAll)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := ParsePEM([]byte(certPEM))
	pool := x509.NewCertPool()
	pool.AddCert(c.Root)
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "web.fev.local", Roots: pool}); err != nil {
		t.Errorf("a keychain-signed leaf does not verify: %v", err)
	}
	if Identity() != "" {
		t.Skip("this test binary is signed with an identity; the ad-hoc arm is not run")
	}
}

func TestStateTellsAbsentFromUntrusted(t *testing.T) {
	kc := scratchKeychain(t)
	ca, _ := OpenFileCA(t.TempDir(), RootOptions{User: "state", Host: "test"})
	if st, err := State(ca.Root.Raw, kc); err != nil || st != daemon.TrustAbsent {
		t.Fatalf("a new root in the scratch keychain: %q, %v", st, err)
	}
	if st, err := State(ca.Root.Raw, ""); err != nil || st != daemon.TrustAbsent {
		t.Fatalf("a new root in the user's search list: %q, %v", st, err)
	}
	pemFile := filepath.Join(t.TempDir(), "root.pem")
	os.WriteFile(pemFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Root.Raw}), 0o644)
	if out, err := exec.Command("security", "add-certificates", "-k", kc, pemFile).CombinedOutput(); err != nil {
		t.Fatalf("add-certificates: %v: %s", err, out)
	}
	if st, err := State(ca.Root.Raw, kc); err != nil || st != daemon.TrustUntrusted {
		t.Fatalf("the root in the scratch keychain: %q, %v", st, err)
	}
	other, _ := OpenFileCA(t.TempDir(), RootOptions{User: "state", Host: "test"})
	if bytes.Equal(other.Root.Raw, ca.Root.Raw) {
		t.Fatal("two roots alike")
	}
	if st, _ := State(other.Root.Raw, kc); st != daemon.TrustAbsent {
		t.Errorf("another root of the same subject: %q", st)
	}
}
