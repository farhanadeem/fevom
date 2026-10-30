package update

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(seed)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, ed25519.NewKeyFromSeed(seed)
}

func TestTheKeyFileForms(t *testing.T) {
	path, k := testKey(t)
	got, err := ReadKey(path)
	if err != nil || !bytes.Equal(got, k) {
		t.Fatalf("seed form: %v", err)
	}
	old := append(append([]byte{}, k...), k.Public().(ed25519.PublicKey)...)
	os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(old)), 0o600)
	if got, err = ReadKey(path); err != nil || !bytes.Equal(got, k) {
		t.Fatalf("Sparkle's older form: %v", err)
	}
	old[len(old)-1] ^= 1
	os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(old)), 0o600)
	if _, err = ReadKey(path); err == nil {
		t.Fatal("a mismatched public half was taken")
	}
	os.Chmod(path, 0o644)
	if _, err = ReadKey(path); err == nil || !strings.Contains(err.Error(), "others may read it") {
		t.Fatalf("a readable key file was taken: %v", err)
	}
	fresh := filepath.Join(t.TempDir(), "new")
	nk, err := NewKey(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ReadKey(fresh); err != nil || !bytes.Equal(got, nk) {
		t.Fatalf("a new key read back: %v", err)
	}
	if _, err := NewKey(fresh); err == nil {
		t.Fatal("an existing key file was written over")
	}
}

func TestAFeedVerifiesAndATamperedOneDoesNot(t *testing.T) {
	_, k := testKey(t)
	pub := k.Public().(ed25519.PublicKey)
	zip := []byte("an archive")
	it := Item{Version: "0.1.14", Date: "Tue, 27 Oct 2026 12:00:00 +0000", URL: "https://example.invalid/Fevom-v0.1.14.zip?a=1&b=2",
		Length: len(zip), Signature: Sign(k, zip), MinOS: "26.0", Notes: "## What changed\n\n- a <b>thing</b> & more\n"}
	b, err := Render(it, k)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(b, pub)
	if err != nil {
		t.Fatalf("the feed as written: %v\n%s", err, b)
	}
	if got != it {
		t.Fatalf("read back %+v, wrote %+v", got, it)
	}
	if err := VerifyArchive(got, zip, pub); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("?a=1&amp;b=2")) || !bytes.Contains(b, []byte("<![CDATA[## What changed")) {
		t.Fatalf("escaping:\n%s", b)
	}
	if _, err := Verify(bytes.Replace(b, []byte("0.1.14</sparkle:version>"), []byte("0.1.15</sparkle:version>"), 1), pub); err == nil {
		t.Fatal("a changed version verified")
	}
	if err := VerifyArchive(got, []byte("an archivf"), pub); err == nil {
		t.Fatal("another archive verified")
	}
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, 32))
	b2, _ := Render(it, other)
	if _, err := Verify(b2, pub); err == nil {
		t.Fatal("a feed signed by another key verified")
	}
	if _, err := Verify(b[:bytes.LastIndex(b, []byte(sigPrefix))], pub); err == nil {
		t.Fatal("an unsigned feed verified")
	}
	it.Notes = "x ]]> y"
	if _, err := Render(it, k); err == nil {
		t.Fatal("notes closing the CDATA were taken")
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"0.1.14", "0.1.13", true}, {"0.1.13", "0.1.13", false}, {"0.1.9", "0.1.10", false}, {"1.0.0", "0.9.9", true}, {"0.2.0", "0.10.0", false}} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}
