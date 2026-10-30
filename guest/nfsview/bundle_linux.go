package nfsview

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func ReadBundle(dir string) (int, []string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "init.pid"))
	if err != nil {
		return 0, nil, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, nil, fmt.Errorf("nfsview: %s/init.pid: %q", dir, b)
	}
	spec, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return 0, nil, err
	}
	dests, err := BindDests(spec)
	return pid, dests, err
}
