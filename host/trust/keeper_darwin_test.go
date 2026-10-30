//go:build darwin && cgo

package trust

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	api "fev/proto/daemon"
)

func TestTheKeeperMakesTheKeyOnFirstUseOnly(t *testing.T) {
	kc := scratchKeychain(t)
	rootPath := filepath.Join(t.TempDir(), "https", "ca.pem")
	made := 0
	o := KeeperOptions{Keychain: kc, Label: "dev.fevom.ca.keeper-test", Signer: keptSigner(t), RootPath: rootPath, Root: RootOptions{User: "t", Host: "h", Constrain: true},
		OnRootChange: func() { made++ }}
	k := NewKeeper(o)
	st := k.State()
	if st.Key != api.KeyNone || st.Trust != api.TrustAbsent || st.Root != "" {
		t.Fatalf("before the first use: %+v", st)
	}
	if kind, _ := peekKey(kc, o.Label, o.Signer); kind != api.KeyNone {
		t.Fatalf("State made a key (%s)", kind)
	}
	if _, err := os.Stat(rootPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("State wrote the root")
	}
	ca, err := k.CA()
	if err != nil {
		t.Fatal(err)
	}
	if made != 1 || ca.Key != api.KeySigner {
		t.Fatalf("first use: told %d times, key %s", made, ca.Key)
	}
	st = k.State()
	if st.Key != api.KeySigner || st.Root != Fingerprint(ca.Root.Raw) || st.Trust != api.TrustAbsent || st.Error != "" {
		t.Fatalf("after the first use: %+v", st)
	}
	o.OnRootChange = func() { made += 10 }
	k2 := NewKeeper(o)
	if st := k2.State(); st.Key != api.KeySigner || st.Root != Fingerprint(ca.Root.Raw) {
		t.Fatalf("a new keeper before its first use: %+v", st)
	}
	ca2, err := k2.CA()
	if err != nil || Fingerprint(ca2.Root.Raw) != Fingerprint(ca.Root.Raw) || made != 1 {
		t.Fatalf("opened again: %v, told %d", err, made)
	}
}

func TestALockedKeychainIsUnusableUntilItIsUnlocked(t *testing.T) {
	kc := scratchKeychain(t)
	rootPath := filepath.Join(t.TempDir(), "ca.pem")
	o := KeeperOptions{Keychain: kc, Label: "dev.fevom.ca.locked-test", Signer: keptSigner(t), RootPath: rootPath}
	first, err := NewKeeper(o).CA()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("security", "lock-keychain", kc).CombinedOutput(); err != nil {
		t.Fatalf("lock-keychain: %v: %s", err, out)
	}
	k := NewKeeper(o)
	_, err = k.CA()
	if !errors.Is(err, ErrKeyUnusable) {
		t.Fatalf("a locked keychain: %v", err)
	}
	st := k.State()
	if st.Key != api.KeyUnusable || !strings.Contains(st.Error, "locked") || st.Root != Fingerprint(first.Root.Raw) {
		t.Fatalf("locked: %+v", st)
	}
	if out, err := exec.Command("security", "unlock-keychain", "-p", "", kc).CombinedOutput(); err != nil {
		t.Fatalf("unlock-keychain: %v: %s", err, out)
	}
	ca, err := k.CA()
	if err != nil {
		t.Fatalf("unlocked: %v", err)
	}
	if Fingerprint(ca.Root.Raw) != Fingerprint(first.Root.Raw) {
		t.Fatal("a second root was made")
	}
	if st := k.State(); st.Key != api.KeySigner || st.Error != "" {
		t.Fatalf("unlocked: %+v", st)
	}
}

func TestTheResidentSignerSignsAgainOnceTheKeychainIsUnlocked(t *testing.T) {
	kc := scratchKeychain(t)
	o := KeeperOptions{Keychain: kc, Label: "dev.fevom.ca.resident-test", Signer: keptSigner(t), RootPath: filepath.Join(t.TempDir(), "ca.pem")}
	k := NewKeeper(o)
	ca, err := k.CA()
	if err != nil {
		t.Fatal(err)
	}
	signs(t, ca.Signer)
	p := ca.Signer.(*signerKey).p
	p.mu.Lock()
	pid := p.cmd.Process.Pid
	p.mu.Unlock()
	if out, err := exec.Command("security", "lock-keychain", kc).CombinedOutput(); err != nil {
		t.Fatalf("lock-keychain: %v: %s", err, out)
	}
	digest := make([]byte, 32)
	if _, err := ca.Signer.Sign(nil, digest, nil); !errors.Is(err, ErrKeyUnusable) || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("a locked keychain: %v", err)
	}
	if out, err := exec.Command("security", "unlock-keychain", "-p", "", kc).CombinedOutput(); err != nil {
		t.Fatalf("unlock-keychain: %v: %s", err, out)
	}
	again, err := k.CA()
	if err != nil || again != ca {
		t.Fatalf("the keeper's CA after the unlock: %p %v (was %p)", again, err, ca)
	}
	signs(t, ca.Signer)
	p.mu.Lock()
	now := p.cmd.Process.Pid
	p.mu.Unlock()
	if now != pid {
		t.Fatalf("the signer was restarted (%d, was %d): the resident one did not sign again", now, pid)
	}
}

func TestTheTrustFailsPlainlyWithNoWindowServer(t *testing.T) {
	kc := scratchKeychain(t)
	o := KeeperOptions{Keychain: kc, Label: "dev.fevom.ca.nogui-test", Signer: keptSigner(t), RootPath: filepath.Join(t.TempDir(), "ca.pem"), Root: RootOptions{User: "t", Host: "h", Constrain: true}}
	k := NewKeeper(o)
	old := trustSSL
	trustSSL = func([]byte) int { return errAuthorizationInteractionNotAllow }
	defer func() { trustSSL = old }()
	st, err := k.Trust(context.Background(), api.TrustInstall)
	if !errors.Is(err, ErrNoGUI) {
		t.Fatalf("the trust with no window server: %v, want ErrNoGUI", err)
	}
	if !strings.Contains(err.Error(), "not SSH") {
		t.Errorf("the error does not say why: %v", err)
	}
	if st.Trust == api.TrustTrusted || st.Trust == api.TrustDeclined {
		t.Errorf("the state after: %+v", st)
	}
}
