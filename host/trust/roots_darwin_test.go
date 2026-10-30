//go:build darwin && cgo

package trust

import (
	"bytes"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

func TestTrustSettingsAreReadFromTheFramework(t *testing.T) {
	for kind, want := range map[int][]Setting{
		0: {},
		1: {{HasResult: true, Result: resultTrustRoot}},
		2: {{Policy: true, PolicySSL: true, HasResult: true, Result: resultTrustRoot}},
		3: {{Policy: true, HasResult: true, Result: resultTrustRoot}},
		4: {{PolicyString: true, HasResult: true, Result: resultTrustRoot}},
		5: {{HasResult: true, Result: resultDeny}},
		6: {{Policy: true, HasResult: true, Result: resultDeny}, {Policy: true, PolicySSL: true, HasResult: true, Result: resultTrustAsRoot}},
		7: {{}},
	} {
		got := synthSettings(kind)
		if len(got) != len(want) {
			t.Errorf("kind %d: %+v, want %+v", kind, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("kind %d, setting %d: %+v, want %+v", kind, i, got[i], want[i])
			}
		}
	}
	if !Decide(synthSettings(6), false) || Decide(synthSettings(3), true) || !Decide(synthSettings(2), true) {
		t.Error("the converted settings are decided wrongly")
	}
}

func TestTheReaderReadsAScratchKeychain(t *testing.T) {
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
	dir := t.TempDir()
	kc := filepath.Join(dir, "fev-trust-test.keychain")
	if out, err := exec.Command("security", "create-keychain", "-p", "", kc).CombinedOutput(); err != nil {
		t.Fatalf("create-keychain: %v: %s", err, out)
	}
	var prev []string
	for _, f := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(before, -1) {
		prev = append(prev, f[1])
	}
	restore := func() {
		exec.Command("security", append([]string{"list-keychains", "-d", "user", "-s"}, prev...)...).Run()
	}
	restore()
	t.Cleanup(func() {
		exec.Command("security", "delete-keychain", kc).Run()
		restore()
		if after := list(); after != before {
			t.Errorf("the keychain search list was\n%s\nand is\n%s", before, after)
		}
	})
	c, _ := newCert(t, "fev scratch root", nil, nil)
	pemFile := filepath.Join(dir, "root.pem")
	os.WriteFile(pemFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}), 0o644)
	if out, err := exec.Command("security", "add-certificates", "-k", kc, pemFile).CombinedOutput(); err != nil {
		t.Fatalf("add-certificates: %v: %s", err, out)
	}
	path := kc
	if _, err := os.Stat(path); err != nil {
		path = kc + "-db"
	}
	ders, err := keychainCertificates(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ders) != 1 || !bytes.Equal(ders[0], c.Raw) {
		t.Fatalf("%d certificates read; the one put there: %v", len(ders), len(ders) == 1 && bytes.Equal(ders[0], c.Raw))
	}
}

func TestRootsReadsTheMacsTrustSettings(t *testing.T) {
	roots, err := Roots()
	if err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, args := range [][]string{{"dump-trust-settings"}, {"dump-trust-settings", "-d"}} {
		out, _ := exec.Command("security", args...).CombinedOutput()
		if m := regexp.MustCompile(`Number of trusted certs = (\d+)`).FindStringSubmatch(string(out)); m != nil {
			n := 0
			for _, c := range m[1] {
				n = n*10 + int(c-'0')
			}
			listed += n
		}
	}
	if len(roots) > listed {
		t.Errorf("%d roots read, %d certificates with settings listed", len(roots), listed)
	}
	t.Logf("%d roots of %d certificates with settings", len(roots), listed)
}
