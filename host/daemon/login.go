package daemon

import (
	"bytes"
	_ "embed"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"text/template"
)

const LoginLabel = "dev.fevom.fevd"

const oldLoginLabel = "local.fev.fevd"

type LoginItem struct {
	Dir     string
	Program string
	Args    []string
}

func (l LoginItem) Path() string { return filepath.Join(l.Dir, LoginLabel+".plist") }

//go:embed login.plist.tmpl
var loginPlist string

var loginTemplate = template.Must(template.New("login.plist").Funcs(template.FuncMap{
	"xml": func(s string) (string, error) {
		var e bytes.Buffer
		err := xml.EscapeText(&e, []byte(s))
		return e.String(), err
	},
}).Parse(loginPlist))

func (l LoginItem) Plist() []byte {
	var b bytes.Buffer
	err := loginTemplate.Execute(&b, struct {
		Label     string
		Arguments []string
	}{LoginLabel, append([]string{l.Program, "--foreground", "--no-autostart", "--login"}, l.Args...)})
	if err != nil {
		panic("login.plist.tmpl: " + err.Error())
	}
	return b.Bytes()
}

func (l LoginItem) Apply(on bool) error {
	if err := os.Remove(filepath.Join(l.Dir, oldLoginLabel+".plist")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	path := l.Path()
	if !on {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	want := l.Plist()
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, want) {
		return nil
	}
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, want, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
