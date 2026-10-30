package bundle

import (
	"os"
	"path/filepath"
)

func Contents(exe string) string {
	dir := filepath.Dir(exe)
	for i := 0; i < 3; i++ {
		if filepath.Base(dir) == "Contents" {
			if _, err := os.Stat(filepath.Join(dir, "Info.plist")); err == nil {
				return dir
			}
			return ""
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

func Peer(exe, name string) string {
	candidates := []string{filepath.Join(filepath.Dir(exe), name)}
	if c := Contents(exe); c != "" {
		switch name {
		case "fev":
			candidates = append(candidates, filepath.Join(c, "MacOS", "bin", name))
		default:
			candidates = append(candidates, filepath.Join(c, "Helpers", name))
		}
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

func Tools(exe string) string {
	c := Contents(exe)
	if c == "" {
		return ""
	}
	dir := filepath.Join(c, "MacOS", "xbin")
	if info, err := os.Stat(filepath.Join(dir, "docker")); err == nil && info.Mode().IsRegular() {
		return dir
	}
	return ""
}

func DebugAgent(exe string) string {
	if c := Contents(exe); c != "" {
		d := filepath.Join(c, "Resources", "guest", "debug-agent")
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			return d
		}
	}
	dir := filepath.Dir(exe)
	for i := 0; i < 4; i++ {
		d := filepath.Join(dir, "guest", "out", "debug-agent")
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			return d
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

func Self() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return exe
}
