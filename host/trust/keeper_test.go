package trust

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	api "fev/proto/daemon"
)

var _ interface {
	State() api.HTTPSStatus
	Trust(context.Context, string) (api.HTTPSStatus, error)
} = (*Keeper)(nil)

func TestAFileKeeperMakesItsCAOnFirstUseAndIsNeverTrusted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	made := 0
	k := NewKeeper(KeeperOptions{Dir: dir, OnRootChange: func() { made++ }})
	st := k.State()
	if st.Key != api.KeyNone || st.Trust != api.TrustAbsent || st.Root != "" {
		t.Fatalf("before the first use: %+v", st)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("State made %s", dir)
	}
	ca, err := k.CA()
	if err != nil {
		t.Fatal(err)
	}
	if made != 1 {
		t.Fatalf("OnRootChange called %d times on the first use", made)
	}
	if again, _ := k.CA(); again != ca || made != 1 {
		t.Fatalf("a second use made another CA or told again (%d)", made)
	}
	st = k.State()
	if st.Key != api.KeyFile || st.Root != Fingerprint(ca.Root.Raw) {
		t.Fatalf("after the first use: %+v", st)
	}
	if k.RootPEM() != ca.RootPEM() {
		t.Fatal("RootPEM is not the CA's root")
	}
	k2 := NewKeeper(KeeperOptions{Dir: dir, OnRootChange: func() { made++ }})
	if ca2, err := k2.CA(); err != nil || Fingerprint(ca2.Root.Raw) != st.Root || made != 1 {
		t.Fatalf("opened again: %v, made %d", err, made)
	}
	for _, action := range []string{api.TrustInstall, api.TrustRemove, api.TrustRotate} {
		if _, err := k.Trust(context.Background(), action); !errors.Is(err, ErrFileCATrust) {
			t.Fatalf("%s on a file CA: %v", action, err)
		}
	}
}
