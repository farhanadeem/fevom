package privhelper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	api "fev/proto/privhelper"
)

type Store interface {
	Entries() (map[string]StoreEntry, error)
	Set(name string, port int, written time.Time) error
	Remove(name string) error
}

type StoreEntry struct {
	Port    int
	Written time.Time
}

const StoreStandIn = "/var/db/dev.fevom.store-standin"

type DirStore struct{ Dir string }

type dirStoreFile struct {
	Key                     string   `json:"key"`
	SupplementalMatchDomain []string `json:"SupplementalMatchDomains"`
	NoSearch                int      `json:"SupplementalMatchDomainsNoSearch,omitempty"`
	ServerAddresses         []string `json:"ServerAddresses"`
	ServerPort              int      `json:"ServerPort"`
	Written                 string   `json:"written"`
}

func (d DirStore) Entries() (map[string]StoreEntry, error) {
	des, err := os.ReadDir(d.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]StoreEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]StoreEntry{}
	for _, de := range des {
		n, ok := strings.CutSuffix(de.Name(), ".json")
		if !ok || !entryName(n) {
			continue
		}
		b, err := readSmall(filepath.Join(d.Dir, de.Name()))
		if err != nil {
			return nil, err
		}
		var f dirStoreFile
		if err := json.Unmarshal(b, &f); err != nil || f.Key != api.StoreKey(n) {
			return nil, fmt.Errorf("%s is not a stand-in entry", filepath.Join(d.Dir, de.Name()))
		}
		t, _ := time.Parse(time.RFC3339Nano, f.Written)
		out[n] = StoreEntry{Port: f.ServerPort, Written: t}
	}
	return out, nil
}

func (d DirStore) Set(name string, port int, written time.Time) error {
	if err := os.MkdirAll(d.Dir, 0o755); err != nil {
		return err
	}
	noSearch := 1
	if api.StoreSearches(name) {
		noSearch = 0
	}
	b, err := json.MarshalIndent(dirStoreFile{
		Key: api.StoreKey(name), SupplementalMatchDomain: []string{name}, NoSearch: noSearch,
		ServerAddresses: []string{"127.0.0.1"}, ServerPort: port, Written: written.UTC().Format(time.RFC3339Nano),
	}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(d.Dir, name+".json"), append(b, '\n'), 0o644)
}

func (d DirStore) Remove(name string) error {
	err := os.Remove(filepath.Join(d.Dir, name+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func storeFor(root string) Store {
	if root == "" {
		return DarwinStore{}
	}
	return DirStore{Dir: filepath.Join(root, StoreStandIn)}
}

func entryName(n string) bool {
	if n == ownName {
		return true
	}
	c, err := api.CanonicalLocalName(n, "")
	return err == nil && c == n
}

func ownStoreKey(source string) bool {
	return strings.HasPrefix(source, "State:/Network/Service/"+api.StoreServicePrefix)
}
