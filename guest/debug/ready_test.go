package debug

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreReadyNeedsNoLinkToResolve(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "profiles", "default", "bin")
	os.MkdirAll(bin, 0o755)
	if err := os.Symlink("/nix/store/v5grxq3yy7rkry3rpmqjk1cr95pkc6y2-zsh-5.9.2/bin/zsh", filepath.Join(bin, "zsh")); err != nil {
		t.Fatal(err)
	}
	stamp := Stamp([]byte("# fev-debug default set x\n/nix/store/a\n"))
	if StoreReady(root, stamp) {
		t.Fatal("ready with no stamp recorded")
	}
	os.WriteFile(filepath.Join(root, StampFile), []byte(stamp+"\n"), 0o644)
	if _, err := os.Stat(filepath.Join(bin, "zsh")); err == nil {
		t.Fatal("the test's link resolves here; it must not")
	}
	if !StoreReady(root, stamp) {
		t.Fatal("a store made whole for this stamp is not ready")
	}
	if StoreReady(root, Stamp([]byte("# another release\n"))) {
		t.Fatal("ready for another release's stamp")
	}
	os.Remove(filepath.Join(bin, "zsh"))
	if StoreReady(root, stamp) {
		t.Fatal("ready with no shell")
	}
}

func TestStampCoversTheLockAndTheShellFiles(t *testing.T) {
	a, b := Stamp([]byte("x")), Stamp([]byte("y"))
	if a == b || len(a) != 16 {
		t.Fatalf("stamps %q %q", a, b)
	}
}
