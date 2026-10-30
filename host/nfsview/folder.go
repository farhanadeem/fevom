package nfsview

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const StoppedREADME = `fev

This folder shows the files of fev's Linux machines and Docker objects
while fev is running: each machine at ~/Fevom/<machine>, Docker's
containers, images and volumes under ~/Fevom/docker. The files live in
fev's Linux VM; start fev to see them here.

[fev is STOPPED. Files are NOT available.]
`

const (
	folderMode = 0o555
	readmeMode = 0o444
	readmeName = "README.txt"
)

type NotEmptyError struct {
	Dir   string
	Names []string
}

func (e *NotEmptyError) Error() string {
	return fmt.Sprintf("%s holds files of its own (%s): fev shows its view there; move them, and fev mounts it at the next start",
		e.Dir, strings.Join(e.Names, ", "))
}

var ignored = []string{readmeName, ".DS_Store", ".localized"}

func Prepare(dir string) error {
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("%s is not a folder (%s): fev shows its view there", dir, fi.Mode().Type())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var own []string
	for _, e := range entries {
		if !slices.Contains(ignored, e.Name()) {
			own = append(own, e.Name())
		}
	}
	if len(own) > 0 {
		if len(own) > 5 {
			own = append(own[:5], "...")
		}
		return &NotEmptyError{Dir: dir, Names: own}
	}
	readme := filepath.Join(dir, readmeName)
	have, err := os.ReadFile(readme)
	fi, serr := os.Lstat(readme)
	if err != nil || !bytes.Equal(have, []byte(StoppedREADME)) || serr != nil || !fi.Mode().IsRegular() {
		if err := os.Chmod(dir, 0o755); err != nil {
			return err
		}
		os.Remove(readme)
		if err := os.WriteFile(readme, []byte(StoppedREADME), readmeMode); err != nil {
			os.Chmod(dir, folderMode)
			return err
		}
	}
	if err := os.Chmod(readme, readmeMode); err != nil {
		return err
	}
	return os.Chmod(dir, folderMode)
}
