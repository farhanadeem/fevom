package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"fev/host/daemon/home"
)

const (
	sshFenceOpen  = "# >>> fev ssh >>>"
	sshFenceClose = "# <<< fev ssh <<<"
)

type sshEdit struct {
	Path    string   `json:"path"`
	Created bool     `json:"created,omitempty"`
	Dirs    []string `json:"dirs,omitempty"`
}

func sshEntryPath(l home.Layout) string { return filepath.Join(l.Home, "ssh", "config") }

func sshBlock(entry string) string {
	return sshFenceOpen + "\n" +
		"# fev's Linux machines over SSH (\"ssh fev\"). Keep this before any Host\n" +
		"# block: an Include after one belongs to that host. \"fev setup --undo\" removes it.\n" +
		"Include \"" + entry + "\"\n" +
		sshFenceClose + "\n"
}

func addSSHInclude(path, entry string, done bool) (edit sshEdit, changed bool, err error) {
	edit = sshEdit{Path: path}
	old, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if done {
			return edit, false, nil
		}
		edit.Created = true
		for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
			if _, err := os.Stat(dir); err == nil || dir == filepath.Dir(dir) {
				break
			}
			edit.Dirs = append([]string{dir}, edit.Dirs...)
		}
		for _, dir := range edit.Dirs {
			if err := os.Mkdir(dir, 0o700); err != nil {
				return edit, false, err
			}
		}
	case err != nil:
		return edit, false, err
	}
	if bytes.Contains(old, []byte(sshFenceOpen)) || done {
		return edit, false, nil
	}
	return edit, true, replaceFile(path, append([]byte(sshBlock(entry)), old...), 0o600)
}

func removeSSHInclude(edit sshEdit) error {
	data, err := os.ReadFile(edit.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	start := bytes.Index(data, []byte(sshFenceOpen))
	if start < 0 {
		return nil
	}
	end := bytes.Index(data[start:], []byte(sshFenceClose+"\n"))
	if end < 0 {
		return fmt.Errorf("%s: the end of fev's block is missing; take it out by hand", edit.Path)
	}
	end += start + len(sshFenceClose) + 1
	rest := append(append([]byte{}, data[:start]...), data[end:]...)
	if edit.Created && len(rest) == 0 {
		if err := os.Remove(edit.Path); err != nil {
			return err
		}
		for i := len(edit.Dirs) - 1; i >= 0; i-- {
			if os.Remove(edit.Dirs[i]) != nil {
				break
			}
		}
		return nil
	}
	return replaceFile(edit.Path, rest, 0o600)
}
