package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	policy "fev/guest/internal/emulation"
	"fev/guest/internal/logbuf"
	"fev/guest/internal/stage"
	api "fev/proto/agent"
)

func TestUnusableRosettaLeavesEngineDiscoveryOnFEX(t *testing.T) {
	for _, cfg := range []policy.Config{{Mode: policy.Auto}, {Mode: policy.Auto, Rosetta: true}, {Mode: policy.Rosetta, Rosetta: true}, {Mode: policy.FEX}} {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "fev"), 0755); err != nil {
			t.Fatal(err)
		}
		group := filepath.Join(dir, "cgroup.binfmt_group")
		if err := os.WriteFile(group, []byte("0"), 0600); err != nil {
			t.Fatal(err)
		}
		e := &emulation{profile: cfg}
		if err := e.writeConfig(dir, dir); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(group)
		want := map[bool]string{false: "2", true: "1"}[cfg.Rosetta]
		if cfg.Mode == policy.FEX {
			want = "0"
		}
		if err != nil || string(b) != want {
			t.Fatalf("%+v: group %q, %v", cfg, b, err)
		}
	}
}

func TestTheTranslatorIsRegisteredBeforeTheEngineAndMachinesAndNeitherNeedsIt(t *testing.T) {
	for _, fail := range []bool{false, true} {
		s := &system{}
		stages := s.stages(logbuf.New(1, func() float64 { return 0 }, nil))
		var mu sync.Mutex
		var order []string
		note := func(name string) {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
		}
		for i := range stages {
			name := stages[i].Name
			switch name {
			case "emulation":
				stages[i].Run = func() error {
					note(name)
					if fail {
						return errors.New("binfmt: mount /proc/sys/fs/binfmt_misc: no such device")
					}
					return nil
				}
			case "engine", "machines":
				stages[i].Run = func() error { note(name); return nil }
			default:
				stages[i].Run = func() error { return nil }
			}
		}
		results, err := stage.Run(stages, func() float64 { return 0 })
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range results {
			if (r.Name == "engine" || r.Name == "machines") && r.NotRun {
				t.Errorf("failed=%v: %s not run", fail, r.Name)
			}
		}
		if len(order) != 3 || order[0] != "emulation" {
			t.Errorf("failed=%v: order %v", fail, order)
		}
	}
}

func TestTheTranslatorsVersionIsFoundInItsBinary(t *testing.T) {
	for _, at := range []int{0, 100, 1<<16 - 7, 1<<16 - 2, 3 << 16} {
		data := bytes.Repeat([]byte{0xa5}, at)
		data = append(data, []byte("#FEX-1999 a decoy\x00\x00Linux version %d.%d.%d (FEX@FEX) (clang) #FEX-2610 SMP Oct 17 2026 15:14:22\x00")...)
		data = append(data, bytes.Repeat([]byte{0x5a}, 1000)...)
		if got := unameVersion(bytes.NewReader(data)); got != "FEX-2610" {
			t.Errorf("at %d: %q", at, got)
		}
	}
	if got := unameVersion(strings.NewReader(strings.Repeat("no version here ", 10000))); got != "" {
		t.Errorf("none: %q", got)
	}
}

func TestTheBootSelectsRosettaForAmd64AndKeepsFEXForI386(t *testing.T) {
	e := newEmulation(nil, "fev.emulator=rosetta fev.x86=0")
	if e.want || e.path != api.EmulationRosettaPath || e.engine != api.EmulationEngineRosetta {
		t.Fatalf("%+v", e)
	}
	if len(e.specs) != 3 || e.specs[2].Name != api.EmulationRosettaEntry || e.specs[2].Flags != "POCFJ1" || e.specs[0].Flags != "POCF2" || e.specs[1].Interpreter != api.EmulationPath {
		t.Fatal(e.specs)
	}
	fex := newEmulation(nil, "")
	if !fex.want || fex.engine != api.EmulationEngineFEX || fex.path != api.EmulationPath {
		t.Fatalf("%+v", fex)
	}
	unknown := newEmulation(nil, "fev.emulator=unknown")
	if err := unknown.prepareTranslator(); err == nil {
		t.Fatal("unknown backend silently accepted")
	}
}

func TestTheRosettaCacheNameIsHeldAndRefused(t *testing.T) {
	name := fmt.Sprintf("fev.test.cache.%d", os.Getpid())
	fd, err := holdCacheName(name)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if _, err := holdCacheName(name); !errors.Is(err, unix.EADDRINUSE) {
		t.Fatalf("a second bind of the name: %v, want EADDRINUSE", err)
	}
	c, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(c)
	if err := unix.Connect(c, &unix.SockaddrUnix{Name: "@" + name}); !errors.Is(err, unix.ECONNREFUSED) {
		t.Fatalf("a connection to the name: %v, want ECONNREFUSED", err)
	}
}
