package images

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

const MaxAge = 14 * 24 * time.Hour

const maxSignature = 64 << 10

var ErrStale = errors.New("signed too long ago")

type verifier struct {
	keyring     openpgp.EntityList
	fingerprint []byte
	now         func() time.Time
}

func newVerifier(kr openpgp.EntityList, now func() time.Time) *verifier {
	if now == nil {
		now = time.Now
	}
	return &verifier{keyring: kr, fingerprint: kr[0].PrimaryKey.Fingerprint, now: now}
}

func (v *verifier) check(signed, asc []byte) (time.Time, error) {
	block, err := armor.Decode(bytes.NewReader(asc))
	if err != nil {
		return time.Time{}, fmt.Errorf("not an armored signature: %w", err)
	}
	if block.Type != openpgp.SignatureType {
		return time.Time{}, fmt.Errorf("an armored %q, not a signature", block.Type)
	}
	body, err := io.ReadAll(io.LimitReader(block.Body, maxSignature))
	if err != nil {
		return time.Time{}, fmt.Errorf("the signature's armor: %w", err)
	}
	if n := countPackets(body); n != 1 {
		return time.Time{}, fmt.Errorf("the signature file holds %d packets, not one signature", n)
	}
	now := v.now()
	sig, _, err := openpgp.VerifyDetachedSignature(v.keyring, bytes.NewReader(signed), bytes.NewReader(body), &packet.Config{Time: func() time.Time { return now }})
	if err != nil {
		return time.Time{}, fmt.Errorf("the signature does not verify with the pinned key %s: %w", Fingerprint, err)
	}
	if err := v.issuer(sig); err != nil {
		return time.Time{}, err
	}
	return sig.CreationTime, nil
}

func (v *verifier) issuer(sig *packet.Signature) error {
	if sig.IssuerFingerprint == nil {
		return fmt.Errorf("the signature names no issuer fingerprint (only a key ID); fev accepts only one naming %s", Fingerprint)
	}
	if !bytes.Equal(sig.IssuerFingerprint, v.fingerprint) {
		return fmt.Errorf("the signature's issuer is %s, not the pinned key %s", strings.ToUpper(hex.EncodeToString(sig.IssuerFingerprint)), Fingerprint)
	}
	return nil
}

func (v *verifier) fresh(made time.Time) error {
	if age := v.now().Sub(made); age > MaxAge {
		return fmt.Errorf("%w: on %s, %d days ago (fev takes a build signed within %d days)", ErrStale, made.UTC().Format("2006-01-02 15:04 MST"), int(age.Hours()/24), int(MaxAge.Hours()/24))
	}
	return nil
}

func countPackets(body []byte) int {
	r := packet.NewReader(bytes.NewReader(body))
	n := 0
	for {
		_, err := r.Next()
		if err == io.EOF {
			return n
		}
		if err != nil {
			return -1
		}
		n++
	}
}
