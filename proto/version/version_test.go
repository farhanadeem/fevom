package version

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckAcceptsOnlyTheSameBuild(t *testing.T) {
	if err := Check("dcd8a24", "dcd8a24", "guest agent"); err != nil {
		t.Fatalf("same build refused: %v", err)
	}
	err := Check("dcd8a24-dirty.0a1b2c3d", "dcd8a24-dirty.ffffffff", "guest agent")
	var m *Mismatch
	if !errors.As(err, &m) {
		t.Fatalf("got %v, want a *Mismatch", err)
	}
	if m.Ours != "dcd8a24-dirty.0a1b2c3d" || m.Theirs != "dcd8a24-dirty.ffffffff" || m.Peer != "guest agent" {
		t.Fatalf("mismatch fields: %+v", m)
	}
	for _, want := range []string{`"dcd8a24-dirty.0a1b2c3d"`, `"dcd8a24-dirty.ffffffff"`, "guest agent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %s", err, want)
		}
	}
}

func TestExplainSaysHowTwoBuildsDiffer(t *testing.T) {
	for _, c := range []struct{ a, b, want string }{
		{"dcd8a24", "dcd8a24", "the same"},
		{"dcd8a24", "ea6ff97", "different commits"},
		{"v0.1.0-3-gdcd8a24", "v0.1.0-4-gea6ff97-dirty.00000000", "different commits"},
		{"dcd8a24", "dcd8a24-dirty.0a1b2c3d", "one was built with uncommitted changes"},
		{"dcd8a24-dirty.0a1b2c3d", "dcd8a24-dirty.ffffffff", "one side is stale"},
		{"dev", "dcd8a24", "without tools/build-id.sh"},
		{"dcd8a24", "", "no build identifier"},
	} {
		if got := Explain(c.a, c.b); !strings.Contains(got, c.want) {
			t.Errorf("Explain(%q, %q) = %q, want it to mention %q", c.a, c.b, got, c.want)
		}
		if got, back := Explain(c.a, c.b), Explain(c.b, c.a); got != back {
			t.Errorf("Explain is not symmetric for %q and %q: %q, %q", c.a, c.b, got, back)
		}
	}
}

func TestAnUnstampedBinaryIsDev(t *testing.T) {
	if Build != Unstamped {
		t.Fatalf("Build = %q in a test binary, want %q", Build, Unstamped)
	}
}
