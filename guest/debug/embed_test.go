package debug

import (
	"os"
	"strings"
	"testing"
)

func TestEtcEmbedWrites(t *testing.T) {
	if !strings.Contains(DefaultSetLock, "default set") {
		t.Fatal("lock not embedded")
	}
	d := t.TempDir()
	if err := WriteEtc(d); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".zshrc", ".zshenv", "bashrc", "inputrc", "dircolors"} {
		if _, err := os.Stat(d + "/" + f); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
}
