package dockerctx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	api "fev/proto/daemon"
)

const Name = "fev"

const Default = "default"

func Dir() (string, error) {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".docker"), nil
}

type Store struct {
	Dir       string
	Socket    string
	StateFile string
}

type state struct {
	Dir      string `json:"docker_config"`
	Previous string `json:"previous"`
	Kept     bool   `json:"kept,omitempty"`
	Original string `json:"original,omitempty"`
	Absent   bool   `json:"absent,omitempty"`
	Written  string `json:"written,omitempty"`
}

func metaDir(dir, name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]))
}

func (s Store) configFile() string { return filepath.Join(s.Dir, "config.json") }

type meta struct {
	Name      string            `json:"Name"`
	Metadata  map[string]string `json:"Metadata"`
	Endpoints map[string]struct {
		Host          string `json:"Host"`
		SkipTLSVerify bool   `json:"SkipTLSVerify"`
	} `json:"Endpoints"`
}

func (s Store) metaJSON() []byte {
	data, _ := json.Marshal(map[string]any{
		"Name":      Name,
		"Metadata":  map[string]string{"Description": "Fevom"},
		"Endpoints": map[string]any{"docker": map[string]any{"Host": "unix://" + s.Socket, "SkipTLSVerify": false}},
	})
	return data
}

func exists(dir, name string) bool {
	if name == Default || name == "" {
		return true
	}
	data, err := os.ReadFile(filepath.Join(metaDir(dir, name), "meta.json"))
	if err != nil {
		return false
	}
	var m meta
	return json.Unmarshal(data, &m) == nil && m.Name == name
}

func replace(path string, data []byte, mode fs.FileMode) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fev-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s Store) Ensure() error {
	dir := metaDir(s.Dir, Name)
	want := s.metaJSON()
	path := filepath.Join(dir, "meta.json")
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, want) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return replace(path, want, 0o644)
}

type field struct {
	key   string
	value json.RawMessage
}

func parseObject(data []byte) ([]field, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil {
		return nil, err
	} else if tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var fields []field
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("not a JSON object")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		fields = append(fields, field{key, value})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("something follows the JSON object")
	}
	return fields, nil
}

func formatObject(fields []field) []byte {
	var out bytes.Buffer
	out.WriteString("{")
	for i, f := range fields {
		if i > 0 {
			out.WriteString(",")
		}
		out.WriteString("\n\t")
		key, _ := json.Marshal(f.key)
		out.Write(key)
		out.WriteString(": ")
		out.Write(bytes.TrimSpace(f.value))
	}
	if len(fields) > 0 {
		out.WriteString("\n")
	}
	out.WriteString("}")
	return out.Bytes()
}

const currentKey = "currentContext"

func (s Store) readConfig() (fields []field, current string, mode fs.FileMode, err error) {
	mode = 0o600
	data, err := os.ReadFile(s.configFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, Default, mode, nil
	}
	if err != nil {
		return nil, "", mode, err
	}
	if info, err := os.Stat(s.configFile()); err == nil {
		mode = info.Mode().Perm()
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, Default, mode, nil
	}
	fields, err = parseObject(data)
	if err != nil {
		return nil, "", mode, fmt.Errorf("%s: %w", s.configFile(), err)
	}
	current = Default
	for _, f := range fields {
		if f.key == currentKey {
			var name string
			if json.Unmarshal(f.value, &name) != nil {
				return nil, "", mode, fmt.Errorf("%s: %s is not a string", s.configFile(), currentKey)
			}
			if name != "" {
				current = name
			}
		}
	}
	return fields, current, mode, nil
}

func (s Store) setCurrent(fields []field, name string, mode fs.FileMode) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	return replace(s.configFile(), formatObject(withCurrent(fields, name)), mode)
}

func withCurrent(fields []field, name string) []field {
	var out []field
	done := false
	for _, f := range fields {
		if f.key != currentKey {
			out = append(out, f)
			continue
		}
		if name != Default && !done {
			value, _ := json.Marshal(name)
			out = append(out, field{currentKey, value})
		}
		done = true
	}
	if !done && name != Default {
		value, _ := json.Marshal(name)
		out = append(out, field{currentKey, value})
	}
	return out
}

func (s Store) Status() (api.DockerContext, error) {
	st := api.DockerContext{Name: Name, Exists: exists(s.Dir, Name)}
	_, current, _, err := s.readConfig()
	if err != nil {
		return st, err
	}
	st.Current = current
	st.Selected = current == Name && st.Exists
	return st, nil
}

func (s Store) Select() (selected bool, left string, err error) {
	if err := s.Ensure(); err != nil {
		return false, "", err
	}
	fields, current, mode, err := s.readConfig()
	if err != nil {
		return false, "", err
	}
	switch {
	case current == Name:
		return true, "", nil
	case current != Default && exists(s.Dir, current):
		return false, current, nil
	}
	note, err := s.noteOf(fields, current)
	if err != nil {
		return false, "", err
	}
	if err := s.remember(note); err != nil {
		return false, "", err
	}
	if err := s.setCurrent(fields, Name, mode); err != nil {
		return false, "", err
	}
	return true, "", nil
}

func (s Store) Use() error {
	if err := s.Ensure(); err != nil {
		return err
	}
	fields, current, mode, err := s.readConfig()
	if err != nil {
		return err
	}
	if current == Name {
		return nil
	}
	note, err := s.noteOf(fields, current)
	if err != nil {
		return err
	}
	if err := s.remember(note); err != nil {
		return err
	}
	return s.setCurrent(fields, Name, mode)
}

func (s Store) noteOf(fields []field, current string) (state, error) {
	note := state{Dir: s.Dir, Previous: current, Kept: true}
	if original, err := os.ReadFile(s.configFile()); err == nil {
		note.Original = string(original)
	} else if errors.Is(err, fs.ErrNotExist) {
		note.Absent = true
	} else {
		return state{}, err
	}
	note.Written = sum(formatObject(withCurrent(fields, Name)))
	return note, nil
}

func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func (s Store) remember(note state) error {
	if s.StateFile == "" {
		return nil
	}
	data, _ := json.Marshal(note)
	if err := os.MkdirAll(filepath.Dir(s.StateFile), 0o755); err != nil {
		return err
	}
	return replace(s.StateFile, append(data, '\n'), 0o600)
}

func (s Store) Restore() error {
	fields, current, mode, err := s.readConfig()
	if err != nil {
		return err
	}
	if current == Name {
		previous := Default
		if data, err := os.ReadFile(s.StateFile); err == nil && s.StateFile != "" {
			var st state
			if json.Unmarshal(data, &st) == nil && st.Dir == s.Dir && st.Previous != Name && exists(s.Dir, st.Previous) {
				previous = st.Previous
			}
		}
		if err := s.setCurrent(fields, previous, mode); err != nil {
			return err
		}
	}
	if s.StateFile != "" {
		if err := os.Remove(s.StateFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s Store) Unselect() error {
	if s.StateFile == "" {
		return nil
	}
	data, err := os.ReadFile(s.StateFile)
	if err != nil {
		return nil
	}
	var st state
	if json.Unmarshal(data, &st) != nil || st.Dir != s.Dir {
		return nil
	}
	return s.Restore()
}

func (s Store) Remove() error {
	var note state
	if data, err := os.ReadFile(s.StateFile); err == nil && s.StateFile != "" {
		json.Unmarshal(data, &note)
	}
	now, err := os.ReadFile(s.configFile())
	switch {
	case note.Dir == s.Dir && note.Kept && err == nil && note.Written == sum(now):
		if note.Absent {
			err = os.Remove(s.configFile())
		} else {
			mode := fs.FileMode(0o600)
			if info, err := os.Stat(s.configFile()); err == nil {
				mode = info.Mode().Perm()
			}
			err = replace(s.configFile(), []byte(note.Original), mode)
		}
		if err != nil {
			return err
		}
		if err := os.Remove(s.StateFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	default:
		if err := s.Restore(); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(metaDir(s.Dir, Name)); err != nil {
		return err
	}
	os.Remove(filepath.Join(s.Dir, "contexts", "meta"))
	os.Remove(filepath.Join(s.Dir, "contexts"))
	return nil
}
