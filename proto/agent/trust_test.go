package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"fev/proto/jsonrpc"
)

func TestTheTrustDigestIsTheSetsOwn(t *testing.T) {
	fixtures, err := Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	var p TrustParams
	var want Trusted
	for _, fx := range fixtures {
		if fx.Method == MethodTrustSet {
			p, _ = jsonrpc.Bind[TrustParams](fx.Params)
			want, _ = jsonrpc.Bind[Trusted](fx.Result)
		}
	}
	ders, err := TrustDER(p.Roots)
	if err != nil || len(ders) != 1 {
		t.Fatalf("the fixture's roots: %v", err)
	}
	if got := TrustDigest(ders); got != want.Digest || TrustCount(ders) != want.Count {
		t.Errorf("the fixture's digest %s, count %d; computed %s, %d", want.Digest, want.Count, got, TrustCount(ders))
	}
	a, b := []byte{1, 2, 3}, []byte{1, 2}
	if TrustDigest([][]byte{a, b}) != TrustDigest([][]byte{b, a, b}) || TrustCount([][]byte{b, a, b}) != 2 {
		t.Error("the digest depends on order or repeats")
	}
	if TrustDigest([][]byte{a}) == TrustDigest([][]byte{b}) || TrustDigest([][]byte{{1}, {2, 3}}) == TrustDigest([][]byte{{1, 2}, {3}}) {
		t.Error("two sets have one digest")
	}
	empty := sha256.Sum256(nil)
	if TrustDigest(nil) != hex.EncodeToString(empty[:]) {
		t.Errorf("the empty set's digest is %s", TrustDigest(nil))
	}
	for _, bad := range []string{"", "not pem", strings.Replace(p.Roots[0], "CERTIFICATE", "PRIVATE KEY", 2), p.Roots[0] + p.Roots[0]} {
		if _, err := TrustDER([]string{bad}); err == nil {
			t.Errorf("taken as a root: %.40q", bad)
		}
	}
	if _, err := TrustDER([]string{strings.Repeat("x", MaxTrustBytes+1)}); err == nil {
		t.Error("more than MaxTrustBytes taken")
	}
}
