package images

import (
	"bytes"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
)

const Fingerprint = "E7FB0CAEC8173D669066514CBAEFF88C22F6E216"

//go:embed lxc-images.asc
var pinnedKey []byte

func Keyring() (openpgp.EntityList, error) {
	return keyring(pinnedKey, Fingerprint)
}

func keyring(armored []byte, fingerprint string) (openpgp.EntityList, error) {
	kr, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(armored))
	if err != nil {
		return nil, fmt.Errorf("images: the pinned key: %w", err)
	}
	if len(kr) != 1 {
		return nil, fmt.Errorf("images: the pinned key's file holds %d keys, not 1", len(kr))
	}
	got := strings.ToUpper(hex.EncodeToString(kr[0].PrimaryKey.Fingerprint))
	if got != fingerprint {
		return nil, fmt.Errorf("images: the pinned key's file holds the key %s, not %s", got, fingerprint)
	}
	return kr, nil
}
