package binfmt

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	api "fev/proto/agent"
)

const Dir = "/proc/sys/fs/binfmt_misc"

type Spec struct {
	Name        string
	Offset      int
	Magic, Mask []byte
	Interpreter string
	Flags       string
}

var x86Mask = []byte("\xff\xff\xff\xff\xff\xfe\xfe\x00\x00\x00\x00\xff\xff\xff\xff\xff\xfe\xff\xff\xff")

func Fevom(interpreter string) []Spec {
	return []Spec{
		{Name: api.EmulationEntryX86_64, Interpreter: interpreter, Flags: "POCF", Mask: x86Mask,
			Magic: []byte("\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00")},
		{Name: api.EmulationEntryI386, Interpreter: interpreter, Flags: "POCF", Mask: x86Mask,
			Magic: []byte("\x7fELF\x01\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x03\x00")},
	}
}

func Rosetta(interpreter string) []Spec {
	specs := Fevom(api.EmulationPath)
	specs[0].Name = api.EmulationRosettaEntry
	specs[0].Interpreter = interpreter
	specs[0].Flags = "POCFJ"
	return specs
}

func Mixed() []Spec {
	fex := Fevom(api.EmulationPath)
	fex[0].Flags += "2"
	ros := Rosetta(api.EmulationRosettaPath)[0]
	ros.Flags += "1"
	return append(fex, ros)
}

func interpreterGroup(flags string) byte {
	if strings.Contains(flags, "1") {
		return '1'
	}
	if strings.Contains(flags, "2") {
		return '2'
	}
	return 0
}

func (s Spec) Line() string {
	esc := func(b []byte) string {
		var sb strings.Builder
		for _, c := range b {
			fmt.Fprintf(&sb, "\\x%02x", c)
		}
		return sb.String()
	}
	return fmt.Sprintf(":%s:M:%d:%s:%s:%s:%s", s.Name, s.Offset, esc(s.Magic), esc(s.Mask), s.Interpreter, s.Flags)
}

func (s Spec) Is(e api.EmulationEntry) bool {
	return e.Name == s.Name && e.Interpreter == s.Interpreter
}

func ParseEntry(name string, data []byte) (api.EmulationEntry, error) {
	e := api.EmulationEntry{Name: name}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 {
		return e, fmt.Errorf("binfmt: %s: empty", name)
	}
	switch lines[0] {
	case "enabled":
		e.Enabled = true
	case "disabled":
	default:
		return e, fmt.Errorf("binfmt: %s: first line %q", name, lines[0])
	}
	seen := false
	for _, l := range lines[1:] {
		key, value, _ := strings.Cut(l, " ")
		switch key {
		case "interpreter":
			e.Interpreter, seen = value, true
		case "flags:":
			e.Flags = value
		case "offset":
			n, err := strconv.Atoi(value)
			if err != nil {
				return e, fmt.Errorf("binfmt: %s: offset %q", name, value)
			}
			e.Offset = n
		case "magic":
			e.Magic = value
		case "mask":
			e.Mask = value
		case "extension":
			e.Extension = strings.TrimPrefix(value, ".")
		}
	}
	if !seen {
		return e, fmt.Errorf("binfmt: %s: no interpreter", name)
	}
	return e, nil
}

func WhyNot(enabled bool, entries []api.EmulationEntry, specs []Spec) string {
	if !enabled {
		return "binfmt_misc is turned off (0 was written to its status file)"
	}
	byName := map[string]api.EmulationEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	var why []string
	for _, s := range specs {
		e, ok := byName[s.Name]
		other := Shadowing(entries, s)
		switch {
		case !ok && len(other) > 0:
			why = append(why, fmt.Sprintf("%s is left out while another tool's entry %s takes the same programs (it is written again once that entry goes)", s.Name, named(other)))
		case !ok:
			why = append(why, s.Name+" is not in the table")
		case !s.Is(e):
			why = append(why, fmt.Sprintf("%s is another tool's entry (interpreter %s)", s.Name, e.Interpreter))
		case !e.Enabled:
			why = append(why, s.Name+" is disabled (0 was written to it)")
		case len(other) > 0:
			why = append(why, fmt.Sprintf("another tool's entry %s is matched before %s for the programs both take", named(other), s.Name))
		}
	}
	return strings.Join(why, "; ")
}

func named(entries []api.EmulationEntry) string {
	var n []string
	for _, e := range entries {
		n = append(n, fmt.Sprintf("%s (interpreter %s)", e.Name, e.Interpreter))
	}
	return strings.Join(n, " and ")
}

func Shadowing(entries []api.EmulationEntry, s Spec) []api.EmulationEntry {
	var out []api.EmulationEntry
	for _, e := range entries {
		if a, b := interpreterGroup(s.Flags), interpreterGroup(e.Flags); a != 0 && b != 0 && a != b {
			continue
		}
		if e.Name != s.Name && e.Enabled && Overlaps(e, s.Offset, s.Magic, s.Mask) {
			out = append(out, e)
		}
	}
	return out
}

func Overlaps(e api.EmulationEntry, offset int, magic, mask []byte) bool {
	if e.Extension != "" || e.Magic == "" {
		return false
	}
	em, err := hex.DecodeString(e.Magic)
	if err != nil {
		return false
	}
	ek := bytes.Repeat([]byte{0xff}, len(em))
	if e.Mask != "" {
		if ek, err = hex.DecodeString(e.Mask); err != nil || len(ek) != len(em) {
			return false
		}
	}
	for i := range em {
		at := e.Offset + i - offset
		if at < 0 || at >= len(magic) {
			continue
		}
		k := byte(0xff)
		if mask != nil {
			k = mask[at]
		}
		if (em[i]^magic[at])&ek[i]&k != 0 {
			return false
		}
	}
	return true
}

type Table struct{ Dir string }

func (t Table) Entries() (bool, []api.EmulationEntry, error) {
	status, err := os.ReadFile(filepath.Join(t.Dir, "status"))
	if err != nil {
		return false, nil, err
	}
	names, err := os.ReadDir(t.Dir)
	if err != nil {
		return false, nil, err
	}
	entries := []api.EmulationEntry{}
	for _, d := range names {
		if d.Name() == "status" || d.Name() == "register" || d.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(t.Dir, d.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, nil, err
		}
		e, err := ParseEntry(d.Name(), data)
		if err != nil {
			return false, nil, err
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return string(bytes.TrimSpace(status)) == "enabled", entries, nil
}

func (t Table) Has(name string) bool {
	_, err := os.Stat(filepath.Join(t.Dir, name))
	return err == nil
}

func (t Table) Register(s Spec) error {
	return writeTo(filepath.Join(t.Dir, "register"), s.Line())
}

func (t Table) Remove(name string) error {
	return writeTo(filepath.Join(t.Dir, name), "-1")
}

func writeTo(path, text string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.Write([]byte(text))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func Version(out []byte) string {
	for _, w := range strings.FieldsFunc(string(out), func(r rune) bool {
		return r == ' ' || r == '\n' || r == '(' || r == ')' || r == '\t' || r == '#'
	}) {
		if strings.HasPrefix(w, "FEX-") && w != "FEX-Emu" && len(w) > len("FEX-") {
			return w
		}
	}
	return ""
}
