package binfmt

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	api "fev/proto/agent"
)

type dirTable struct {
	Table
	mu      sync.Mutex
	writes  []string
	refuse  string
	removes []string
}

func (d *dirTable) Register(s Spec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s.Name == d.refuse {
		return fmt.Errorf("refused")
	}
	d.writes = append(d.writes, s.Name)
	text := fmt.Sprintf("enabled\ninterpreter %s\nflags: %s\noffset %d\nmagic %x\nmask %x\n", s.Interpreter, s.Flags, s.Offset, s.Magic, s.Mask)
	return os.WriteFile(filepath.Join(d.Dir, s.Name), []byte(text), 0o644)
}

func (d *dirTable) Remove(name string) error {
	d.mu.Lock()
	d.removes = append(d.removes, name)
	d.mu.Unlock()
	return os.Remove(filepath.Join(d.Dir, name))
}

func (d *dirTable) registered() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.writes...)
}

func newKeeper(t *testing.T) (*Keeper, *dirTable) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "status"), []byte("enabled\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "register"), nil, 0o200)
	d := &dirTable{Table: Table{Dir: dir}}
	k := &Keeper{Table: d, Dir: dir, Specs: Fevom(api.EmulationPath), Logf: func(string, ...any) {}}
	t.Cleanup(func() { k.Set(false) })
	return k, d
}

func within(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("%s: not within 5 s", what)
}

func TestTheKeeperPutsBackOnlyItsOwnMissingEntryOnTheTablesEvent(t *testing.T) {
	k, d := newKeeper(t)
	other := filepath.Join(d.Dir, "qemu-riscv64")
	os.WriteFile(other, []byte("enabled\ninterpreter /usr/bin/qemu-riscv64\nflags: POCF\noffset 0\nmagic 7f454c460201010000000000000000000200f300\nmask ffffffffffffff00fffffffffffffffffeffffff\n"), 0o644)
	if err := k.Start(true); err != nil {
		t.Fatal(err)
	}
	if !k.Watched() {
		t.Fatal("the table is not watched")
	}
	if got := d.registered(); len(got) != 2 {
		t.Fatalf("registered %v", got)
	}
	on, entries, registered, restored, err := k.State()
	if !on || !registered || restored != 0 || err != nil || len(entries) != 3 || !entries[0].Fevom || !entries[1].Fevom || entries[2].Fevom {
		t.Fatalf("state %v %+v %v %d %v", on, entries, registered, restored, err)
	}
	os.Remove(filepath.Join(d.Dir, api.EmulationEntryX86_64))
	within(t, "put back", func() bool { return d.Has(api.EmulationEntryX86_64) })
	if got := d.registered(); len(got) != 3 || got[2] != api.EmulationEntryX86_64 {
		t.Fatalf("registered %v: only the missing one is written again", got)
	}
	os.Remove(other)
	os.WriteFile(filepath.Join(d.Dir, "qemu-riscv64-2"), []byte("enabled\ninterpreter /usr/bin/qemu-riscv64\nflags: F\noffset 0\nmagic 7f454c460201010000000000000000000200f300\nmask ffffffffffffff00fffffffffffffffffeffffff\n"), 0o644)
	time.Sleep(200 * time.Millisecond)
	if got := d.registered(); len(got) != 3 {
		t.Fatalf("registered %v after another tool's change", got)
	}
	os.Remove(filepath.Join(d.Dir, api.EmulationEntryX86_64))
	os.Remove(filepath.Join(d.Dir, api.EmulationEntryI386))
	within(t, "both put back", func() bool { return d.Has(api.EmulationEntryX86_64) && d.Has(api.EmulationEntryI386) })
	if _, _, _, restored, _ := k.State(); restored != 3 {
		t.Errorf("restored %d", restored)
	}
	if err := k.Set(false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if d.Has(api.EmulationEntryX86_64) || d.Has(api.EmulationEntryI386) || !d.Has("qemu-riscv64-2") {
		t.Fatal("off left fev's or removed another's")
	}
	if _, entries, registered, _, _ := k.State(); registered || len(entries) != 1 {
		t.Fatalf("off: registered %v, %+v", registered, entries)
	}
	if err := k.Set(true); err != nil || !d.Has(api.EmulationEntryX86_64) || !d.Has(api.EmulationEntryI386) {
		t.Fatalf("on again: %v", err)
	}
	for _, name := range d.removes {
		if name == "qemu-riscv64-2" || name == "qemu-riscv64" {
			t.Errorf("removed another tool's %s", name)
		}
	}
}

func TestAnEntryOfFevomsNameWithAnotherInterpreterIsLeftAlone(t *testing.T) {
	k, d := newKeeper(t)
	theirs := filepath.Join(d.Dir, api.EmulationEntryX86_64)
	os.WriteFile(theirs, []byte("enabled\ninterpreter /usr/bin/FEXInterpreter\nflags: POCF\noffset 0\nmagic 7f454c4602010100000000000000000002003e00\nmask fffffffffffefe00000000fffffffffffeffffff\n"), 0o644)
	d.refuse = api.EmulationEntryI386
	if err := k.Start(true); err == nil {
		t.Fatal("a refused registration was not said")
	}
	_, entries, registered, _, err := k.State()
	if registered || err == nil || len(entries) != 1 || entries[0].Fevom {
		t.Fatalf("state: %v %+v %v", registered, entries, err)
	}
	d.mu.Lock()
	d.refuse = ""
	d.mu.Unlock()
	if _, _, registered, restored, err := k.State(); registered || err != nil || restored != 0 || !d.Has(api.EmulationEntryI386) {
		t.Fatalf("after the refusal ended: %v %d %v", registered, restored, err)
	}
	if err := k.Set(false); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(theirs); len(data) == 0 {
		t.Fatal("off removed another tool's entry of fev's name")
	}
	if d.Has(api.EmulationEntryI386) {
		t.Fatal("off left fev's own")
	}
}

func TestStartedOffTheKeeperRegistersNothing(t *testing.T) {
	k, d := newKeeper(t)
	if err := k.Start(false); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d.Dir, "x"), []byte("enabled\ninterpreter /x\n"), 0o644)
	os.Remove(filepath.Join(d.Dir, "x"))
	time.Sleep(200 * time.Millisecond)
	if got := d.registered(); len(got) != 0 {
		t.Fatalf("registered %v while off", got)
	}
	if _, _, registered, _, _ := k.State(); registered {
		t.Fatal("registered while off")
	}
}
