//go:build !linux

package machines

import (
	"os"
	"path/filepath"
)

func readIn(root, name string) ([]byte, error) {
	p, err := resolveIn(root, name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
}
