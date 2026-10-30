package binfmt

import (
	"os"
	"path/filepath"
	"testing"

	api "fev/proto/agent"
)

const kernelEntry = "enabled\ninterpreter /system/emu/FEX\nflags: POCF\noffset 0\nmagic 7f454c4602010100000000000000000002003e00\nmask fffffffffffefe00000000fffffffffffeffffff\n"

func TestAnEntrysFileIsReadAsTheKernelWritesIt(t *testing.T) {
	e, err := ParseEntry("FEX-x86_64", []byte(kernelEntry))
	if err != nil {
		t.Fatal(err)
	}
	want := api.EmulationEntry{Name: "FEX-x86_64", Enabled: true, Interpreter: "/system/emu/FEX", Flags: "POCF",
		Magic: "7f454c4602010100000000000000000002003e00", Mask: "fffffffffffefe00000000fffffffffffeffffff"}
	if e != want {
		t.Fatalf("got %+v", e)
	}
	ext, err := ParseEntry("DOSWin", []byte("disabled\ninterpreter /usr/bin/wine\nflags: \nextension .exe\n"))
	if err != nil || ext.Enabled || ext.Extension != "exe" || ext.Interpreter != "/usr/bin/wine" || ext.Magic != "" {
		t.Fatalf("by extension: %+v %v", ext, err)
	}
	for _, bad := range []string{"", "1\n", "enabled\nflags: P\n", "enabled\ninterpreter /x\noffset x\n"} {
		if _, err := ParseEntry("bad", []byte(bad)); err == nil {
			t.Errorf("%q read as an entry", bad)
		}
	}
	for _, s := range Fevom(api.EmulationPath) {
		if len(s.Magic) != len(s.Mask) || len(s.Magic) != 20 || s.Flags != "POCF" {
			t.Errorf("%s: magic %d bytes, mask %d, flags %s", s.Name, len(s.Magic), len(s.Mask), s.Flags)
		}
		if !s.Is(api.EmulationEntry{Name: s.Name, Interpreter: api.EmulationPath}) || s.Is(api.EmulationEntry{Name: s.Name, Interpreter: "/usr/bin/FEX"}) {
			t.Errorf("%s: Is", s.Name)
		}
	}
	if got := Fevom("/system/emu/FEX")[0].Line(); got != `:FEX-x86_64:M:0:\x7f\x45\x4c\x46\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:\xff\xff\xff\xff\xff\xfe\xfe\x00\x00\x00\x00\xff\xff\xff\xff\xff\xfe\xff\xff\xff:/system/emu/FEX:POCF` {
		t.Errorf("line %s", got)
	}
}

func TestTheTableIsItsDirectory(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("status", "enabled\n")
	write("register", "")
	write("qemu-x86_64", "enabled\ninterpreter /usr/bin/qemu-x86_64\nflags: POCF\noffset 0\nmagic 7f45\nmask ffff\n")
	write("FEX-x86_64", kernelEntry)
	on, entries, err := Table{Dir: dir}.Entries()
	if err != nil || !on || len(entries) != 2 || entries[0].Name != "FEX-x86_64" || entries[1].Name != "qemu-x86_64" {
		t.Fatalf("%v %+v %v", on, entries, err)
	}
	write("status", "disabled\n")
	if on, _, _ := (Table{Dir: dir}).Entries(); on {
		t.Error("disabled read as enabled")
	}
	if !(Table{Dir: dir}).Has("FEX-x86_64") || (Table{Dir: dir}).Has("FEX-x86") {
		t.Error("Has")
	}
}

func TestTheTranslatorsVersionIsItsOwnWord(t *testing.T) {
	for out, want := range map[string]string{
		"FEX-Emu (FEX-2610) \n":    "FEX-2610",
		"FEX-2610\n":               "FEX-2610",
		"#FEX-2610 SMP Oct 17":     "FEX-2610",
		"FEX-Emu\n":                "",
		"something else entirely ": "",
	} {
		if got := Version([]byte(out)); got != want {
			t.Errorf("%q: %q", out, got)
		}
	}
}

func TestWhyTheEntriesAreNotRegisteredIsSaid(t *testing.T) {
	specs := Fevom(api.EmulationPath)
	ours := func(name string, on bool) api.EmulationEntry {
		return api.EmulationEntry{Name: name, Enabled: on, Interpreter: api.EmulationPath}
	}
	for _, c := range []struct {
		enabled bool
		entries []api.EmulationEntry
		want    string
	}{
		{true, []api.EmulationEntry{ours("FEX-x86", true), ours("FEX-x86_64", true)}, ""},
		{false, []api.EmulationEntry{ours("FEX-x86", true), ours("FEX-x86_64", true)}, "binfmt_misc is turned off (0 was written to its status file)"},
		{true, []api.EmulationEntry{ours("FEX-x86", true)}, "FEX-x86_64 is not in the table"},
		{true, []api.EmulationEntry{ours("FEX-x86", false), {Name: "FEX-x86_64", Enabled: true, Interpreter: "/usr/bin/FEXInterpreter"}},
			"FEX-x86_64 is another tool's entry (interpreter /usr/bin/FEXInterpreter); FEX-x86 is disabled (0 was written to it)"},
	} {
		if got := WhyNot(c.enabled, c.entries, specs); got != c.want {
			t.Errorf("%+v: %q", c.entries, got)
		}
	}
}

func TestAnotherToolsEntryForTheSameProgramsIsNamed(t *testing.T) {
	specs := Fevom(api.EmulationPath)
	ours := func(s Spec) api.EmulationEntry {
		return api.EmulationEntry{Name: s.Name, Interpreter: s.Interpreter, Enabled: true, Flags: "POCF"}
	}
	qemu := api.EmulationEntry{Name: "qemu-x86_64", Interpreter: "/usr/bin/qemu-x86_64", Enabled: true, Flags: "POCF",
		Magic: "7f454c4602010100000000000000000002003e00", Mask: "fffffffffffefe00000000fffffffffffeffffff"}
	riscv := api.EmulationEntry{Name: "qemu-riscv64", Interpreter: "/usr/bin/qemu-riscv64", Enabled: true,
		Magic: "7f454c460201010000000000000000000200f300", Mask: "ffffffffffffff00fffffffffffffffffeffffff"}
	off := qemu
	off.Name, off.Enabled = "qemu-x86_64-off", false
	ext := api.EmulationEntry{Name: "wine", Interpreter: "/usr/bin/wine", Enabled: true, Extension: "exe"}
	why := WhyNot(true, []api.EmulationEntry{ours(specs[0]), ours(specs[1]), qemu, riscv, off, ext}, specs)
	if want := "another tool's entry qemu-x86_64 (interpreter /usr/bin/qemu-x86_64) is matched before FEX-x86_64 for the programs both take"; why != want {
		t.Fatalf("with qemu-x86_64 there: %q", why)
	}
	why = WhyNot(true, []api.EmulationEntry{ours(specs[1]), qemu}, specs)
	if want := "FEX-x86_64 is left out while another tool's entry qemu-x86_64 (interpreter /usr/bin/qemu-x86_64) takes the same programs (it is written again once that entry goes)"; why != want {
		t.Fatalf("fev's left out: %q", why)
	}
	if why := WhyNot(true, []api.EmulationEntry{ours(specs[0]), ours(specs[1]), riscv, off, ext}, specs); why != "" {
		t.Fatalf("no entry for the same programs: %q", why)
	}
}

func TestTwoEntriesOverlapWhenTheirMagicsAgreeUnderBothMasks(t *testing.T) {
	x64 := Fevom(api.EmulationPath)[0]
	i386 := Fevom(api.EmulationPath)[1]
	entry := func(off int, magic, mask string) api.EmulationEntry {
		return api.EmulationEntry{Name: "e", Enabled: true, Offset: off, Magic: magic, Mask: mask}
	}
	for _, c := range []struct {
		e    api.EmulationEntry
		s    Spec
		want bool
	}{
		{entry(0, "7f454c46", ""), x64, true},
		{entry(0, "7f454c46", ""), i386, true},
		{entry(0, "7f454c4601", ""), x64, false},
		{entry(18, "3e00", ""), x64, true},
		{entry(18, "b700", ""), x64, false},
		{entry(16, "0300", "ffff"), x64, true},
		{entry(16, "0400", "ffff"), x64, false},
		{entry(0, "cffaedfe", ""), x64, false},
		{entry(0, "00000000", "00000000"), x64, true},
		{entry(40, "ffff", ""), x64, true},
		{api.EmulationEntry{Name: "e", Enabled: true, Extension: "exe"}, x64, false},
	} {
		if got := Overlaps(c.e, c.s.Offset, c.s.Magic, c.s.Mask); got != c.want {
			t.Errorf("%+v with %s: %v", c.e, c.s.Name, got)
		}
	}
}

func TestRosettaReplacesOnlyAmd64AndPreservesArgvZero(t *testing.T) {
	specs := Rosetta(api.EmulationRosettaPath)
	if specs[0].Name != api.EmulationRosettaEntry || specs[0].Flags != "POCFJ" || specs[0].Interpreter != api.EmulationRosettaPath {
		t.Fatal(specs[0])
	}
	fex := Fevom(api.EmulationPath)
	if specs[1].Line() != fex[1].Line() {
		t.Fatal("i386 lost FEX")
	}
	if string(specs[0].Magic) != string(fex[0].Magic) || string(specs[0].Mask) != string(fex[0].Mask) {
		t.Fatal("changed amd64 matching")
	}
}

func TestInterpreterGroupsDoNotShadowEachOtherButRespectOperatorEntries(t *testing.T) {
	specs := Mixed()
	if len(specs) != 3 || specs[0].Flags != "POCF2" || specs[2].Flags != "POCFJ1" {
		t.Fatal(specs)
	}
	peer := api.EmulationEntry{Name: specs[0].Name, Interpreter: specs[0].Interpreter, Enabled: true, Flags: specs[0].Flags, Magic: "7f454c4602010100000000000000000002003e00", Mask: "fffffffffffefe00000000fffffffffffeffffff"}
	if len(Shadowing([]api.EmulationEntry{peer}, specs[2])) != 0 {
		t.Fatal("FEX shadows the explicit Rosetta group")
	}
	peer.Name, peer.Interpreter, peer.Flags = "operator-x86", "/usr/bin/qemu-x86_64", "POCF"
	if len(Shadowing([]api.EmulationEntry{peer}, specs[2])) != 1 {
		t.Fatal("operator precedence lost")
	}
}
