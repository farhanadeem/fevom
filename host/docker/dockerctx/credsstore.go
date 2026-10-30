package dockerctx

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
)

const (
	credsKey = "credsStore"
	Keychain = "osxkeychain"
)

func (s Store) SetCredsStore() (set, made bool, err error) {
	_, statErr := os.Stat(s.configFile())
	fields, _, mode, err := s.readConfig()
	if err != nil {
		return false, false, err
	}
	var out []field
	for _, f := range fields {
		if f.key == credsKey {
			var name string
			if json.Unmarshal(f.value, &name) != nil || name != "" {
				return false, false, nil
			}
			continue
		}
		out = append(out, f)
	}
	value, _ := json.Marshal(Keychain)
	out = append(out, field{credsKey, value})
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return false, false, err
	}
	if err := replace(s.configFile(), formatObject(out), mode); err != nil {
		return false, false, err
	}
	return true, errors.Is(statErr, fs.ErrNotExist), nil
}

func (s Store) UnsetCredsStore(made bool) (bool, error) {
	fields, _, mode, err := s.readConfig()
	if err != nil {
		return false, err
	}
	var out []field
	removed := false
	for _, f := range fields {
		var name string
		if f.key == credsKey && json.Unmarshal(f.value, &name) == nil && name == Keychain {
			removed = true
			continue
		}
		out = append(out, f)
	}
	if !removed {
		return false, nil
	}
	if made && len(out) == 0 {
		return true, os.Remove(s.configFile())
	}
	return true, replace(s.configFile(), formatObject(out), mode)
}

type CredsNote struct {
	Set      bool   `json:"set,omitempty"`
	Original string `json:"original,omitempty"`
	Absent   bool   `json:"absent,omitempty"`
	Written  string `json:"written,omitempty"`
}

func (s Store) SetCredsStoreNoted() (bool, CredsNote, error) {
	var note CredsNote
	if original, err := os.ReadFile(s.configFile()); err == nil {
		note.Original = string(original)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, note, err
	}
	set, made, err := s.SetCredsStore()
	if err != nil || !set {
		return set, CredsNote{}, err
	}
	note.Set, note.Absent = true, made
	written, err := os.ReadFile(s.configFile())
	if err != nil {
		return true, note, err
	}
	note.Written = sum(written)
	return true, note, nil
}

func (s Store) UnsetCredsStoreNoted(note CredsNote) (bool, error) {
	if !note.Set {
		return false, nil
	}
	current, err := os.ReadFile(s.configFile())
	if err == nil && note.Written != "" && sum(current) == note.Written {
		if note.Absent {
			return true, os.Remove(s.configFile())
		}
		info, err := os.Stat(s.configFile())
		if err != nil {
			return false, err
		}
		return true, replace(s.configFile(), []byte(note.Original), info.Mode().Perm())
	}
	return s.UnsetCredsStore(note.Absent)
}

func (s Store) CredsStoreName() (string, error) {
	fields, _, _, err := s.readConfig()
	if err != nil {
		return "", err
	}
	for _, f := range fields {
		if f.key == credsKey {
			var name string
			if json.Unmarshal(f.value, &name) != nil {
				return "", nil
			}
			return name, nil
		}
	}
	return "", nil
}
