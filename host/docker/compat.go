package docker

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	api "fev/proto/daemon"
)

const CompatLink = "/var/run/docker.sock"

func compatStatus(link, socket string) (state, target string) {
	target, err := os.Readlink(link)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return api.CompatAbsent, ""
	case err != nil:
		return api.CompatElsewhere, ""
	}
	resolved := target
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(link), resolved)
	}
	if filepath.Clean(resolved) == filepath.Clean(socket) {
		return api.CompatLinked, ""
	}
	return api.CompatElsewhere, target
}
