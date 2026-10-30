package daemon

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoginItemRemovesTheOldLabel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "LaunchAgents")
	l := LoginItem{Dir: dir, Program: "/x/fevd"}
	if err := l.Apply(false); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "local.fev.fevd.plist")
	for _, on := range []bool{false, true} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(old, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := l.Apply(on); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Errorf("on=%v: the old label's file is still there (%v)", on, err)
		}
		_, err := os.Stat(filepath.Join(dir, "dev.fevom.fevd.plist"))
		if on != (err == nil) {
			t.Errorf("on=%v: the current file: %v", on, err)
		}
	}
}

func legacyPlist(l LoginItem) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	str := func(s string) string {
		var e bytes.Buffer
		xml.EscapeText(&e, []byte(s))
		return "<string>" + e.String() + "</string>"
	}
	fmt.Fprintf(&b, "\t<key>Label</key>\n\t%s\n", str(LoginLabel))
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range append([]string{l.Program, "--foreground", "--no-autostart", "--login"}, l.Args...) {
		fmt.Fprintf(&b, "\t\t%s\n", str(arg))
	}
	b.WriteString("\t</array>\n\t<key>RunAtLoad</key>\n\t<true/>\n</dict>\n</plist>\n")
	return b.Bytes()
}

func TestTheLoginTemplateWritesWhatTheStringDid(t *testing.T) {
	for _, l := range []LoginItem{
		{Program: "/Users/someone/.fev/bin/fevd"},
		{Program: "/Applications/Fevom.app/Contents/Helpers/fevd", Args: []string{"--home", "/Users/someone/fev home"}},
		{Program: "/Applications/Fevom & Co/fevd", Args: []string{"--home", "/tmp/<odd> \"quoted\" 'home'", "--fake-vm"}},
	} {
		if got, want := l.Plist(), legacyPlist(l); !bytes.Equal(got, want) {
			t.Errorf("%+v:\n--- template\n%s\n--- string\n%s", l, got, want)
		}
	}
}
