package dockerctx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSelectThenRemoveLeavesTheConfigurationAsItWas(t *testing.T) {
	for _, c := range []struct{ name, config string }{
		{"two spaces, a login", "{\n  \"auths\": {\n    \"registry.example\": {}\n  }\n}\n"},
		{"one line, no newline", `{"auths":{},"credsStore":"osxkeychain"}`},
		{"empty", ""},
		{"absent", "-"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := scratch(t)
			if c.config != "-" {
				s.write(t, c.config)
			} else {
				os.MkdirAll(s.Dir, 0o700)
			}
			if err := s.Ensure(); err != nil {
				t.Fatal(err)
			}
			if selected, _, err := s.Select(); err != nil || !selected {
				t.Fatalf("Select: %v %v", selected, err)
			}
			if err := s.Remove(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(s.configFile())
			if c.config == "-" {
				if err == nil {
					t.Errorf("config.json made and left: %q", data)
				}
			} else if string(data) != c.config {
				t.Errorf("config.json is %q, was %q", data, c.config)
			}
			if _, err := os.Stat(filepath.Join(s.Dir, "contexts")); err == nil {
				t.Error("contexts/ left behind")
			}
		})
	}
}

func TestRemoveKeepsAChangeMadeSinceTheSelection(t *testing.T) {
	s := scratch(t)
	s.write(t, "{\n  \"auths\": {}\n}\n")
	s.Select()
	s.write(t, "{\n\t\"auths\": {\"new.example\": {}},\n\t\"currentContext\": \"fev\"\n}")
	if err := s.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := s.read(t); got != "{\n\t\"auths\": {\"new.example\": {}}\n}" {
		t.Errorf("config.json is %q", got)
	}
}
