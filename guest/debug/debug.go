package debug

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

//go:embed defaultset.lock
var DefaultSetLock string

//go:embed etc/.zshrc etc/.zshenv etc/bashrc etc/inputrc etc/dircolors
var etcFiles embed.FS

func EtcFS() (fs.FS, error) { return fs.Sub(etcFiles, "etc") }

func WriteEtc(dir string) error {
	sub, err := EtcFS()
	if err != nil {
		return err
	}
	return fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o444)
	})
}

const StampFile = "profiles/default/stamp"

func Stamp(lock []byte) string {
	h := sha256.New()
	h.Write(lock)
	sub, err := EtcFS()
	if err == nil {
		var names []string
		fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				names = append(names, p)
			}
			return nil
		})
		sort.Strings(names)
		for _, n := range names {
			b, _ := fs.ReadFile(sub, n)
			h.Write([]byte("\x00" + n + "\x00"))
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func StoreReady(root, stamp string) bool {
	b, err := os.ReadFile(filepath.Join(root, StampFile))
	if err != nil || string(b) != stamp+"\n" {
		return false
	}
	_, err = os.Lstat(filepath.Join(root, "profiles", "default", "bin", "zsh"))
	return err == nil
}
