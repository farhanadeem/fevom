package responsible

import (
	"path/filepath"
	"strings"
)

type Who struct {
	PID       int
	Path      string
	App, Name string
	Self      bool
}

func (w Who) Known() bool { return w.Path != "" }

func Describe(pid int, path, self string) Who {
	w := Who{PID: pid, Path: path}
	if path == "" {
		return w
	}
	if app := appOf(path); app != "" {
		w.App = app
		w.Name = strings.TrimSuffix(filepath.Base(app), ".app")
	}
	w.Self = w.App == "" && self != "" && filepath.Clean(path) == filepath.Clean(self)
	return w
}

func appOf(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) != "MacOS" {
		return ""
	}
	contents := filepath.Dir(dir)
	if filepath.Base(contents) != "Contents" {
		return ""
	}
	app := filepath.Dir(contents)
	if !strings.HasSuffix(app, ".app") || filepath.Base(app) == ".app" {
		return ""
	}
	return app
}

func (w Who) Grant() string {
	const where = "System Settings > Privacy & Security"
	switch {
	case w.Name != "":
		return "Allow it for " + w.Name + ", the app fev was started from, in " + where +
			" > Files and Folders, or give " + w.Name + " Full Disk Access, then restart fev"
	case w.Self:
		return "fev was not started from an app, so macOS asks fevd itself: give fevd (" + w.Path +
			") Full Disk Access in " + where + ", or start fev from the Fevom app or a terminal that has access, then restart fev"
	case w.Path != "":
		return "Allow it for " + w.Path + ", which started fev, in " + where +
			" > Files and Folders, or give it Full Disk Access, then restart fev"
	default:
		return "Allow it for the app fev was started from (usually your terminal) in " + where +
			" > Files and Folders, or give that app Full Disk Access, then restart fev"
	}
}
