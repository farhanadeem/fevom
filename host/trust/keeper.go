package trust

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	api "fev/proto/daemon"
)

type Keeper struct {
	o KeeperOptions

	mu       sync.Mutex
	ca       *CA
	err      error
	unusable *CA
	declined bool
	trustMu  sync.Mutex
}

type KeeperOptions struct {
	Dir          string
	Keychain     string
	Label        string
	Signer       string
	RootPath     string
	Root         RootOptions
	OnRootChange func()
}

func NewKeeper(o KeeperOptions) *Keeper {
	if o.Label == "" {
		o.Label = DefaultKeyLabel
	}
	if o.Dir != "" {
		o.RootPath = filepath.Join(o.Dir, fileRoot)
	}
	return &Keeper{o: o}
}

func (k *Keeper) File() bool { return k.o.Dir != "" }

func (k *Keeper) CA() (*CA, error) {
	ca, made, err := k.open()
	if made && k.o.OnRootChange != nil {
		k.o.OnRootChange()
	}
	return ca, err
}

func (k *Keeper) open() (ca *CA, made bool, err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.ca != nil {
		return k.ca, false, nil
	}
	before := rootFingerprint(k.o.RootPath)
	if k.o.Dir != "" {
		ca, err = OpenFileCA(k.o.Dir, k.o.Root)
	} else {
		ca, err = OpenKeychainCA(KeychainOptions{Keychain: k.o.Keychain, Label: k.o.Label, Signer: k.o.Signer, RootPath: k.o.RootPath, Root: k.o.Root})
	}
	if err != nil {
		k.err = err
		if errors.Is(err, ErrKeyUnusable) {
			k.unusable = ca
		}
		return nil, false, err
	}
	k.ca, k.err, k.unusable = ca, nil, nil
	return ca, Fingerprint(ca.Root.Raw) != before, nil
}

func (k *Keeper) RootPEM() string {
	ca, err := k.CA()
	if err != nil {
		return ""
	}
	return ca.RootPEM()
}

func rootFingerprint(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	c, err := ParsePEM(b)
	if err != nil {
		return ""
	}
	return Fingerprint(c.Raw)
}

func (k *Keeper) State() api.HTTPSStatus {
	k.mu.Lock()
	ca, lastErr, unusable, declined := k.ca, k.err, k.unusable, k.declined
	k.mu.Unlock()
	st := api.HTTPSStatus{Trust: api.TrustAbsent, Key: api.KeyNone}
	var der []byte
	switch {
	case ca != nil:
		st.Key, st.Identity = ca.Key, ca.Identity
		der = ca.Root.Raw
	case unusable != nil:
		st.Key, st.Identity = api.KeyUnusable, unusable.Identity
	default:
		st.Key, st.Identity = k.peek()
	}
	if der == nil {
		if b, err := os.ReadFile(k.o.RootPath); err == nil {
			if c, err := ParsePEM(b); err == nil {
				der = c.Raw
			}
		}
	}
	if lastErr != nil {
		st.Error = lastErr.Error()
	}
	if der == nil {
		return st
	}
	st.Root = Fingerprint(der)
	trusted, err := State(der, k.o.Keychain)
	switch {
	case err != nil && !errors.Is(err, ErrUnsupported):
		if st.Error == "" {
			st.Error = "the Mac's trust settings: " + err.Error()
		}
	case err == nil:
		st.Trust = trusted
	}
	if declined && st.Trust != api.TrustTrusted {
		st.Trust = api.TrustDeclined
	}
	return st
}

func (k *Keeper) peek() (kind, identity string) {
	if k.o.Dir != "" {
		if _, err := os.Stat(filepath.Join(k.o.Dir, fileKey)); err == nil {
			return api.KeyFile, ""
		}
		return api.KeyNone, ""
	}
	return peekKey(k.o.Keychain, k.o.Label, k.o.Signer)
}

var ErrFileCATrust = errors.New("a development CA (--ca-dir) is never put in the Mac's trust: give its ca.pem to the client")

func (k *Keeper) Trust(ctx context.Context, action string) (api.HTTPSStatus, error) {
	if k.o.Dir != "" {
		return k.State(), ErrFileCATrust
	}
	k.trustMu.Lock()
	defer k.trustMu.Unlock()
	if err := ctx.Err(); err != nil {
		return k.State(), err
	}
	var err error
	switch action {
	case api.TrustInstall:
		err = k.install()
	case api.TrustRemove:
		err = k.remove()
	case api.TrustRotate:
		err = k.rotate()
	default:
		return k.State(), fmt.Errorf("https.trust: %q is not install, remove or rotate", action)
	}
	k.mu.Lock()
	switch {
	case errors.Is(err, ErrDeclined):
		k.declined, err = true, nil
	case err == nil:
		k.declined = false
	}
	k.mu.Unlock()
	return k.State(), err
}

func (k *Keeper) install() error {
	ca, err := k.CA()
	if errors.Is(err, ErrKeyOtherProgram) {
		return k.rotate()
	}
	if err != nil {
		return err
	}
	return Install(ca.Root.Raw, k.o.Keychain)
}

func (k *Keeper) remove() error {
	k.mu.Lock()
	ca := k.ca
	k.mu.Unlock()
	var der []byte
	if ca != nil {
		der = ca.Root.Raw
	} else if b, err := os.ReadFile(k.o.RootPath); err == nil {
		if c, err := ParsePEM(b); err == nil {
			der = c.Raw
		}
	}
	if der == nil {
		return nil
	}
	return Remove(der, k.o.Keychain)
}

func (k *Keeper) rotate() error {
	if err := k.remove(); err != nil {
		return err
	}
	k.mu.Lock()
	if k.ca != nil {
		k.ca.close()
	}
	k.ca, k.err, k.unusable = nil, nil, nil
	k.mu.Unlock()
	if err := DeleteKeychainKey(k.o.Keychain, k.o.Label, k.o.Signer); err != nil {
		return err
	}
	if err := os.Remove(k.o.RootPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ca, err := k.CA()
	if err != nil {
		return err
	}
	return Install(ca.Root.Raw, k.o.Keychain)
}
