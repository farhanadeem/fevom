package responsible

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestTheAppIsNamedFromItsMainProgram(t *testing.T) {
	for _, c := range []struct {
		path, self string
		name, app  string
		isSelf     bool
	}{
		{"/Applications/Ghostty.app/Contents/MacOS/ghostty", "", "Ghostty", "/Applications/Ghostty.app", false},
		{"/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal", "", "Terminal", "/System/Applications/Utilities/Terminal.app", false},
		{"/Users/u/Applications/Fevom.app/Contents/MacOS/Fevom", "", "Fevom", "/Users/u/Applications/Fevom.app", false},
		{"/A.app/Contents/Frameworks/Inner.app/Contents/MacOS/inner", "", "Inner", "/A.app/Contents/Frameworks/Inner.app", false},
		{"/Users/u/Applications/Fevom.app/Contents/Helpers/fevd", "/Users/u/Applications/Fevom.app/Contents/Helpers/fevd", "", "", true},
		{"/usr/local/bin/tool", "/opt/fevd", "", "", false},
		{"/x/.app/Contents/MacOS/y", "", "", "", false},
		{"/x/Y.app/Contents/Resources/y", "", "", "", false},
	} {
		w := Describe(7, c.path, c.self)
		if w.Name != c.name || w.App != c.app || w.Self != c.isSelf || !w.Known() {
			t.Errorf("%s: %+v", c.path, w)
		}
	}
	if w := Describe(0, "", ""); w.Known() || w.Self || w.Name != "" {
		t.Errorf("nothing said: %+v", w)
	}
}

func TestTheGrantNamesWhoMacOSAsks(t *testing.T) {
	app := Describe(7, "/Users/u/Applications/Fevom.app/Contents/MacOS/Fevom", "")
	if g := app.Grant(); !strings.Contains(g, "Allow it for Fevom,") || !strings.Contains(g, "give Fevom Full Disk Access") {
		t.Errorf("app: %s", g)
	}
	self := Describe(7, "/opt/b/fevd", "/opt/b/fevd")
	if g := self.Grant(); !strings.Contains(g, "fevd (/opt/b/fevd) Full Disk Access") {
		t.Errorf("self: %s", g)
	}
	other := Describe(7, "/usr/bin/script", "/opt/b/fevd")
	if g := other.Grant(); !strings.Contains(g, "/usr/bin/script, which started fev") {
		t.Errorf("other: %s", g)
	}
	if g := (Who{}).Grant(); !strings.Contains(g, "the app fev was started from (usually your terminal)") {
		t.Errorf("unknown: %s", g)
	}
}

func TestMacOSIsAsked(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	w := Self()
	if !w.Known() || w.PID <= 0 {
		t.Fatalf("macOS said nothing: %+v", w)
	}
	if w.Name == "" && !w.Self && w.PID != os.Getpid() {
		t.Logf("responsible: %+v (neither an app nor this program)", w)
	}
	t.Logf("responsible for this test: %+v", w)
	if o := Of(w.PID); !o.Known() {
		t.Errorf("the responsible process's own: %+v", o)
	}
}
