package nfsview

import (
	"strings"
	"testing"
)

func TestWordsReadAsTheKernelWritesThem(t *testing.T) {
	for _, s := range []string{
		"/run/fev/view/docker/containers/web",
		"/a b/c\td\ne\\f",
		"ghcr.io_org_app:1.2",
		"naïve café",
		`\x41`,
	} {
		q := Quote(s)
		if strings.ContainsAny(q, " \t\n") {
			t.Errorf("Quote(%q) = %q holds a separator", s, q)
		}
		got, err := Unquote(q)
		if err != nil || got != s {
			t.Errorf("Unquote(Quote(%q)) = %q, %v", s, got, err)
		}
	}
	for word, want := range map[string]string{
		`/export/a\040b`: "/export/a b",
		`/x\134y`:        `/x\y`,
		`\x01000000`:     "\x01\x00\x00\x00",
		`/odd\08`:        `/odd\08`,
		`/hi\477`:        `/hi\477`,
		`\xE8030000`:     "\xe8\x03\x00\x00",
	} {
		got, err := Unquote(word)
		if err != nil || got != want {
			t.Errorf("Unquote(%q) = %q, %v; want %q", word, got, err, want)
		}
	}
	if Quote("") == "" {
		t.Error("an empty word was written: the kernel would read the next word in its place")
	}
	if _, err := Unquote(`\x123`); err == nil {
		t.Error("an odd hex word was read")
	}
	if Hex([]byte{0xe8, 3, 0, 0}) != `\xe8030000` {
		t.Errorf("Hex: %s", Hex([]byte{0xe8, 3, 0, 0}))
	}
}
