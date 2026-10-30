package dockerctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheKeychainIsTheStoreOnlyWhereNoneIsNamed(t *testing.T) {
	cases := []struct {
		name, before string
		set          bool
		after        string
	}{
		{"none", "{\n\t\"auths\": {},\n\t\"currentContext\": \"fev\"\n}", true,
			"{\n\t\"auths\": {},\n\t\"currentContext\": \"fev\",\n\t\"credsStore\": \"osxkeychain\"\n}"},
		{"empty", "{\"credsStore\": \"\", \"experimental\":   \"enabled\"}", true,
			"{\n\t\"experimental\": \"enabled\",\n\t\"credsStore\": \"osxkeychain\"\n}"},
		{"another", "{\"credsStore\": \"desktop\"}", false, "{\"credsStore\": \"desktop\"}"},
		{"ours already", "{\"credsStore\": \"osxkeychain\"}", false, "{\"credsStore\": \"osxkeychain\"}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Store{Dir: t.TempDir()}
			os.WriteFile(s.configFile(), []byte(c.before), 0o600)
			set, made, err := s.SetCredsStore()
			if err != nil || set != c.set || made {
				t.Fatalf("set %v made %v err %v", set, made, err)
			}
			if got, _ := os.ReadFile(s.configFile()); string(got) != c.after {
				t.Fatalf("config.json:\n%s\nwant\n%s", got, c.after)
			}
			if !set {
				return
			}
			if removed, err := s.UnsetCredsStore(made); !removed || err != nil {
				t.Fatalf("unset: %v %v", removed, err)
			}
			got, _ := os.ReadFile(s.configFile())
			var keys []string
			fields, _ := parseObject(got)
			for _, f := range fields {
				keys = append(keys, f.key)
				if f.key == credsKey {
					t.Errorf("the key is still there: %s", got)
				}
			}
			if len(keys) == 0 {
				t.Errorf("the other keys went: %s", got)
			}
		})
	}
	t.Run("made", func(t *testing.T) {
		s := Store{Dir: filepath.Join(t.TempDir(), ".docker")}
		set, made, err := s.SetCredsStore()
		if !set || !made || err != nil {
			t.Fatalf("set %v made %v err %v", set, made, err)
		}
		if info, err := os.Stat(s.configFile()); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("config.json: %v %v", info, err)
		}
		if removed, err := s.UnsetCredsStore(made); !removed || err != nil {
			t.Fatalf("unset: %v %v", removed, err)
		}
		if _, err := os.Stat(s.configFile()); !os.IsNotExist(err) {
			t.Errorf("config.json made for the key is still there: %v", err)
		}
	})
	t.Run("changed since", func(t *testing.T) {
		s := Store{Dir: t.TempDir()}
		if _, _, err := s.SetCredsStore(); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(s.configFile(), []byte(`{"credsStore": "pass"}`), 0o600)
		if removed, err := s.UnsetCredsStore(false); removed || err != nil {
			t.Fatalf("a store the user chose since was taken: %v %v", removed, err)
		}
		if got, _ := os.ReadFile(s.configFile()); string(got) != `{"credsStore": "pass"}` {
			t.Errorf("config.json: %s", got)
		}
	})
	t.Run("not an object", func(t *testing.T) {
		s := Store{Dir: t.TempDir()}
		os.WriteFile(s.configFile(), []byte(`[1]`), 0o600)
		if set, _, err := s.SetCredsStore(); set || err == nil {
			t.Fatalf("a file that is not an object was written over: %v %v", set, err)
		}
	})
}

func TestTheCredsStoreTakenBackLeavesTheFileAsItWas(t *testing.T) {
	dir := t.TempDir()
	s := Store{Dir: dir}
	original := "{\n    \"auths\": {},\n    \"detachKeys\": \"ctrl-e,e\"\n}\n"
	if err := os.WriteFile(s.configFile(), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	set, note, err := s.SetCredsStoreNoted()
	if err != nil || !set {
		t.Fatalf("set %v, %v", set, err)
	}
	if removed, err := s.UnsetCredsStoreNoted(note); err != nil || !removed {
		t.Fatalf("removed %v, %v", removed, err)
	}
	if got, _ := os.ReadFile(s.configFile()); string(got) != original {
		t.Errorf("config.json after the undo:\n%s\nwant, byte for byte:\n%s", got, original)
	}

	_, note, _ = s.SetCredsStoreNoted()
	data, _ := os.ReadFile(s.configFile())
	changed := strings.Replace(string(data), "ctrl-e,e", "ctrl-x,x", 1)
	os.WriteFile(s.configFile(), []byte(changed), 0o600)
	if removed, err := s.UnsetCredsStoreNoted(note); err != nil || !removed {
		t.Fatalf("removed %v, %v", removed, err)
	}
	got, _ := os.ReadFile(s.configFile())
	if !strings.Contains(string(got), "ctrl-x,x") || strings.Contains(string(got), Keychain) {
		t.Errorf("a file changed since: %s", got)
	}

	other := Store{Dir: filepath.Join(dir, "fresh")}
	_, note, _ = other.SetCredsStoreNoted()
	if _, err := other.UnsetCredsStoreNoted(note); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other.configFile()); !os.IsNotExist(err) {
		t.Errorf("a config.json made for the key is left: %v", err)
	}
}
