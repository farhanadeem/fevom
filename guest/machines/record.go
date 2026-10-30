package machines

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	api "fev/proto/agent"
)

const RecordsDir = ".records"

type Record struct {
	Name   string          `json:"name"`
	UUID   string          `json:"uuid"`
	Image  api.ImageRef    `json:"image"`
	User   api.MachineUser `json:"user"`
	Setup  bool            `json:"setup,omitempty"`
	Online bool            `json:"online,omitempty"`
	Keygen bool            `json:"keygen,omitempty"`
}

var ErrNoMachine = errors.New("no such machine")

func RecordPath(dataDir, name string) string {
	return filepath.Join(dataDir, RecordsDir, name+".json")
}

func ReadRecord(dataDir, name string) (*Record, error) {
	if err := api.CheckMachineName(name); err != nil {
		return nil, err
	}
	if st, err := os.Stat(filepath.Join(dataDir, name)); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrNoMachine, name)
	}
	b, err := os.ReadFile(RecordPath(dataDir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s (its files are there, its record is not)", ErrNoMachine, name)
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("the record of %s: %w", name, err)
	}
	if r.Name != name {
		return nil, fmt.Errorf("the record of %s names %q", name, r.Name)
	}
	return &r, nil
}

func WriteRecord(dataDir string, r *Record) error {
	if err := api.CheckMachineName(r.Name); err != nil {
		return err
	}
	dir := filepath.Join(dataDir, RecordsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+r.Name+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), RecordPath(dataDir, r.Name))
}

func Names(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || api.CheckMachineName(e.Name()) != nil {
			continue
		}
		if _, err := os.Stat(RecordPath(dataDir, e.Name())); err == nil {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

func RemoveRecord(dataDir, name string) error {
	if err := api.CheckMachineName(name); err != nil {
		return err
	}
	if err := os.Remove(RecordPath(dataDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func Records(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, RecordsDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if ok && e.Type().IsRegular() && api.CheckMachineName(name) == nil {
			names = append(names, name)
		}
	}
	return names, nil
}

const imageTemplatesDir = ".images"

func imageTemplatesPath(dataDir string, ref api.ImageRef) string {
	return filepath.Join(dataDir, RecordsDir, imageTemplatesDir, ref.Distro, ref.Release, ref.Arch, ref.Variant, ref.Serial+".json")
}

func WriteImageTemplates(dataDir string, ref api.ImageRef, ts []api.ImageTemplate) error {
	p := imageTemplatesPath(dataDir, ref)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if ts == nil {
		ts = []api.ImageTemplate{}
	}
	b, err := json.Marshal(ts)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".templates.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func ReadImageTemplates(dataDir string, ref api.ImageRef) ([]api.ImageTemplate, bool) {
	b, err := os.ReadFile(imageTemplatesPath(dataDir, ref))
	if err != nil {
		return nil, false
	}
	var ts []api.ImageTemplate
	if json.Unmarshal(b, &ts) != nil {
		return nil, false
	}
	return ts, true
}

func PruneImageTemplates(dataDir string, ref api.ImageRef) {
	dir := filepath.Dir(imageTemplatesPath(dataDir, ref))
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if serial, ok := strings.CutSuffix(e.Name(), ".json"); ok && serial < ref.Serial {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func NewUUID() (string, error) {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", err
	}
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:]), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
