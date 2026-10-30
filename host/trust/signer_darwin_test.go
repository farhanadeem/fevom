//go:build darwin && cgo

package trust

import (
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"fev/proto/daemon"
)

func partitionIDs(t *testing.T, line string) string {
	t.Helper()
	i := strings.Index(line, "desc=")
	if i < 0 {
		return ""
	}
	b, err := hex.DecodeString(strings.TrimSpace(line[i+5:]))
	if err != nil {
		t.Fatalf("a partition description not in hexadecimal: %q", line)
	}
	return strings.Join(regexp.MustCompile(`<string>([^<]*)</string>`).FindAllString(string(b), -1), " ")
}

func keptSigner(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "keysigner", "fev-keysigner"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("no kept signer: %v", err)
	}
	return p
}

func cdhashOf(t *testing.T, path string) string {
	t.Helper()
	out, _ := exec.Command("codesign", "-dvvv", path).CombinedOutput()
	m := regexp.MustCompile(`(?m)^CDHash=(\w+)`).FindSubmatch(out)
	if m == nil {
		t.Fatalf("no cdhash for %s: %s", path, out)
	}
	return string(m[1])
}

func fevdBuilds(t *testing.T, n int) []string {
	t.Helper()
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go")
	dir := t.TempDir()
	var out []string
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		d := filepath.Join(dir, "b"+string(rune('0'+i)))
		bin := filepath.Join(d, "fevd")
		cmd := exec.Command(gobin, "build", "-trimpath", "-ldflags", "-X main.stamp="+string(rune('0'+i)), "-o", bin, "./testdata/fevd-build")
		if o, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %d: %v: %s", i, err, o)
		}
		if b, err := os.ReadFile(keptSigner(t)); err == nil {
			os.WriteFile(filepath.Join(d, "fev-keysigner"), b, 0o755)
		}
		h := cdhashOf(t, bin)
		if seen[h] {
			t.Fatalf("two builds share the cdhash %s: the test would not model a rebuild", h)
		}
		seen[h] = true
		out = append(out, bin)
	}
	return out
}

func TestTheScratchKeychainRecordsPartitionsAsTheLoginKeychainDoes(t *testing.T) {
	kc := scratchKeychain(t)
	if err := makeKey(kc, "fev.test.partition", tagAny, false); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("security", "dump-keychain", "-a", kc).CombinedOutput()
	if err != nil {
		t.Fatalf("dump-keychain: %v: %s", err, out)
	}
	if !regexp.MustCompile(`partition_id\s+\S[^\n]*\n\s+description: (cdhash|teamid):`).Match(out) {
		t.Fatalf("no partition ID on a key made in the scratch keychain: it is not the login keychain's kind\n%s", out)
	}
}

func TestEveryBuildOfFevdSignsWithTheOneKey(t *testing.T) {
	kc := scratchKeychain(t)
	root := filepath.Join(t.TempDir(), "ca.pem")
	var first string
	for i, b := range fevdBuilds(t, 3) {
		out, err := exec.Command(b, kc, "fev.test.builds", root).CombinedOutput()
		line := strings.TrimSpace(string(out))
		t.Logf("build %d (cdhash %s): %s", i, cdhashOf(t, b), line)
		if err != nil {
			t.Errorf("build %d cannot sign with the CA's key: %s", i, line)
			continue
		}
		fp := regexp.MustCompile(`root=(\w+)`).FindStringSubmatch(line)
		if fp == nil {
			t.Fatalf("build %d: no root in %q", i, line)
		}
		if first == "" {
			first = fp[1]
		} else if fp[1] != first {
			t.Errorf("build %d made another root (%s, the first was %s)", i, fp[1], first)
		}
	}
}

func TestAKeyAnotherProgramMadeIsNamedAndReplaced(t *testing.T) {
	kc := scratchKeychain(t)
	kept := keptSigner(t)
	other := filepath.Join(t.TempDir(), SignerName)
	if b, err := os.ReadFile(kept); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(other, b, 0o755); err != nil {
		t.Fatal(err)
	}
	if o, err := exec.Command("codesign", "-s", "-", "-f", "-i", "dev.fevom.keysigner.other", other).CombinedOutput(); err != nil {
		t.Fatalf("codesign: %v: %s", err, o)
	}
	if cdhashOf(t, other) == cdhashOf(t, kept) {
		t.Fatal("the other signer has the kept one's cdhash")
	}
	for _, c := range []struct {
		name string
		make func(label string) error
	}{
		{"a fev that held the key itself", func(label string) error { return makeKey(kc, label, tagAny, false) }},
		{"another key signer", func(label string) error {
			ca, err := OpenKeychainCA(KeychainOptions{Keychain: kc, Label: label, Signer: other, RootPath: filepath.Join(t.TempDir(), "ca.pem"), AnyApplication: true})
			if ca != nil {
				ca.close()
			}
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			label := "fev.test.other." + strings.ReplaceAll(c.name, " ", "-")
			if err := c.make(label); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(t.TempDir(), "ca.pem")
			o := KeychainOptions{Keychain: kc, Label: label, Signer: kept, RootPath: root, AnyApplication: true}
			ca, err := OpenKeychainCA(o)
			if !errors.Is(err, ErrKeyUnusable) || !errors.Is(err, ErrKeyOtherProgram) || ca == nil || ca.Key != daemon.KeyUnusable {
				t.Fatalf("opened: %+v, %v", ca, err)
			}
			if !strings.Contains(err.Error(), "fev setup --https") {
				t.Errorf("the error does not name the remedy: %v", err)
			}
			t.Logf("%v", err)
			if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
				t.Error("a root was made for a key the signer cannot use")
			}
			if kind, _ := peekKey(kc, label, kept); kind != daemon.KeyUnusable {
				t.Errorf("peek: %s", kind)
			}
			if err := DeleteKeychainKey(kc, label, kept); err != nil {
				t.Fatal(err)
			}
			ca, err = OpenKeychainCA(o)
			if err != nil || ca.Key != daemon.KeySigner {
				t.Fatalf("after the delete: %+v, %v", ca, err)
			}
			defer ca.close()
			signs(t, ca.Signer)
			if kind, _ := peekKey(kc, label, kept); kind != daemon.KeySigner {
				t.Errorf("peek after: %s", kind)
			}
		})
	}
}

func TestTheSignerIsStartedAgainIfItEnds(t *testing.T) {
	kc := scratchKeychain(t)
	ca, err := OpenKeychainCA(KeychainOptions{Keychain: kc, Label: "fev.test.restart", Signer: keptSigner(t), RootPath: filepath.Join(t.TempDir(), "ca.pem"), AnyApplication: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ca.close()
	p := ca.Signer.(*signerKey).p
	p.mu.Lock()
	pid := p.cmd.Process.Pid
	p.cmd.Process.Kill()
	p.mu.Unlock()
	signs(t, ca.Signer)
	p.mu.Lock()
	again := p.cmd.Process.Pid
	p.mu.Unlock()
	if again == pid {
		t.Fatal("the same signer process signed after it was killed")
	}
}

func TestTheKeptSignerRebuildsBitForBit(t *testing.T) {
	cmd := exec.Command(filepath.Join("..", "keysigner", "build.sh"), "--check")
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 2 {
		t.Skip("another toolchain than host/keysigner/TOOLCHAIN records")
	}
	if err != nil {
		t.Fatalf("the kept signer does not rebuild from its source: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join("..", "keysigner", "CDHASH")); strings.TrimSpace(string(b)) != cdhashOf(t, keptSigner(t)) {
		t.Errorf("CDHASH says %q, the kept binary is %s", b, cdhashOf(t, keptSigner(t)))
	}
}
