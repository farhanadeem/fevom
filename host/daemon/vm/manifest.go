package vm

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

const ManifestSuffix = ".manifest"

type KernelManifest struct {
	Release, Version, SHA256 string
}

func ReadKernelManifest(image string) (KernelManifest, error) {
	var m KernelManifest
	data, err := os.ReadFile(image + ManifestSuffix)
	if err != nil {
		return m, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		value = strings.TrimSpace(value)
		switch key {
		case "release":
			m.Release = value
		case "version":
			m.Version = value
		case "sha256":
			m.SHA256 = value
		}
	}
	if m.Release == "" {
		return m, fmt.Errorf("%s%s: no release line", image, ManifestSuffix)
	}
	return m, nil
}

func NoManifest(err error) bool { return errors.Is(err, fs.ErrNotExist) }
