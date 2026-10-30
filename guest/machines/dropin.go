package machines

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

var unitDirs = []string{"/etc/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"}

type dropIn struct {
	unit     string
	settings []string
	programs []string
}

var dropIns = []dropIn{
	{unit: "netplan-configure.service", settings: []string{"ExecStartPost"}, programs: []string{"udevadm"}},
}

const DropInName = "fev.conf"

func writeDropIns(root, run string) error {
	for _, d := range dropIns {
		var text []byte
		for _, dir := range unitDirs {
			p, err := resolveIn(root, path.Join(dir, d.unit))
			if err != nil {
				continue
			}
			if text, err = os.ReadFile(filepath.Join(root, filepath.FromSlash(p))); err == nil {
				break
			}
		}
		if text == nil {
			continue
		}
		conf := d.render(string(text))
		if conf == "" {
			continue
		}
		dir := filepath.Join(run, "systemd/system", d.unit+".d")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, DropInName), []byte(conf), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (d dropIn) render(unit string) string {
	var out strings.Builder
	section := ""
	lines := map[string][]string{}
	for _, raw := range strings.Split(unit, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		if section != "[Service]" || line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		for _, s := range d.settings {
			if key == s {
				if value == "" {
					lines[s] = nil
				} else {
					lines[s] = append(lines[s], value)
				}
			}
		}
	}
	for _, s := range d.settings {
		changed := false
		restated := make([]string, 0, len(lines[s]))
		for _, v := range lines[s] {
			if d.fatal(v) {
				v, changed = "-"+v, true
			}
			restated = append(restated, v)
		}
		if !changed {
			continue
		}
		if out.Len() == 0 {
			out.WriteString("# Written by fev at the machine's start, in its own /run.\n")
			out.WriteString("# /sys is read-only in a machine and udevd does not run, so udevadm's\n")
			out.WriteString("# calls cannot succeed; they are made non-fatal. Nothing else changes.\n")
			out.WriteString("[Service]\n")
		}
		out.WriteString(s + "=\n")
		for _, v := range restated {
			out.WriteString(s + "=" + v + "\n")
		}
	}
	return out.String()
}

func (d dropIn) fatal(v string) bool {
	prefix := ""
	for len(v) > 0 && strings.ContainsRune("-@:+!", rune(v[0])) {
		prefix += v[:1]
		v = v[1:]
	}
	if strings.Contains(prefix, "-") {
		return false
	}
	program := v
	if i := strings.IndexAny(v, " \t"); i >= 0 {
		program = v[:i]
	}
	for _, p := range d.programs {
		if program == p || path.Base(program) == p && strings.HasPrefix(program, "/") {
			return true
		}
	}
	return false
}
