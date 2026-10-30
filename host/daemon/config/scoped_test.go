package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "fev/proto/daemon"
)

func TestMachineEmulationOverrideIsValidatedAndSurvivesRename(t *testing.T) {
	s, _ := fresh(t)
	if err := s.AddScope(MachinePrefix, "dev", MachineKeys("ada"), nil); err != nil {
		t.Fatal(err)
	}
	key := "machine.dev.emulation"
	if s.String(key) != "auto" {
		t.Fatal(s.String(key))
	}
	if _, err := s.Set(map[string]any{key: "typo"}); err == nil {
		t.Fatal("invalid engine accepted")
	}
	s.MachineStarted("dev", s.MachineValues("dev"))
	change, err := s.Set(map[string]any{key: "fex"})
	if err != nil || len(change) != 1 || !change[0].Pending || change[0].Apply != api.ApplyMachineRestart {
		t.Fatalf("%+v %v", change, err)
	}
	if err := s.RenameScope(MachinePrefix, "dev", "renamed"); err != nil {
		t.Fatal(err)
	}
	if s.String("machine.renamed.emulation") != "fex" {
		t.Fatal("override lost on rename")
	}
}

func TestAMachinesKeysLiveAndDieWithIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"version":1,"values":{"machine.old.username":"x","machine.dev.username":"alice","machine.gone.username":"y"}}`), 0o644)
	var logged []string
	s, err := Load(path, Keys(Host{CPUs: 4, MemMiB: 8192}), func(f string, args ...any) { logged = append(logged, f) })
	if err != nil {
		t.Fatal(err)
	}
	if len(logged) != 0 {
		t.Errorf("a machine's key was reported as unknown: %v", logged)
	}
	fixed, per := len(s.Describe()), len(MachineKeys("ada"))
	if err := s.AddScope(MachinePrefix, "dev", MachineKeys("ada"), nil); err != nil {
		t.Fatal(err)
	}
	if got := s.String("machine.dev.username"); got != "alice" {
		t.Errorf("the file's value was not taken up: %q", got)
	}
	if err := s.AddScope(MachinePrefix, "f44", MachineKeys("ada"), map[string]any{KeyUsername: "bob"}); err != nil {
		t.Fatal(err)
	}
	if len(s.Describe()) != fixed+2*per || s.String("machine.f44.username") != "bob" {
		t.Errorf("after two machines: %d keys, f44's user %q", len(s.Describe()), s.String("machine.f44.username"))
	}
	if _, err := s.Set(map[string]any{"machine.f44.username": "Bob!"}); err == nil {
		t.Error("a user name no distribution takes was set")
	}
	if _, err := s.Set(map[string]any{"machine.f44.username": "carol"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameScope(MachinePrefix, "f44", "web"); err != nil {
		t.Fatal(err)
	}
	if s.String("machine.web.username") != "carol" || s.String("machine.f44.username") != "" {
		t.Errorf("after a rename: %q and %q", s.String("machine.web.username"), s.String("machine.f44.username"))
	}
	if err := s.PruneScope(MachinePrefix, func(name string) bool { return name == "dev" || name == "web" }); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveScope(MachinePrefix, "web"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "old") || strings.Contains(string(data), "gone") || strings.Contains(string(data), "web") || !strings.Contains(string(data), `"machine.dev.username": "alice"`) {
		t.Errorf("the file after prune and remove: %s", data)
	}
	if len(s.Describe()) != fixed+per {
		t.Errorf("%d keys described", len(s.Describe()))
	}
}

func TestADottedMachinesKeysAreItsOwn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"version":1,"values":{"machine.a.b.username":"ab","machine.a.username":"a","machine.x.y.z.username":"xyz"}}`), 0o644)
	var logged []string
	s, err := Load(path, Keys(Host{CPUs: 4, MemMiB: 8192}), func(f string, args ...any) { logged = append(logged, f) })
	if err != nil {
		t.Fatal(err)
	}
	if len(logged) != 0 {
		t.Errorf("a dotted machine's key was reported as unknown: %v", logged)
	}
	for _, name := range []string{"a", "a.b"} {
		if err := s.AddScope(MachinePrefix, name, MachineKeys("ada"), nil); err != nil {
			t.Fatal(err)
		}
	}
	if s.String("machine.a.username") != "a" || s.String("machine.a.b.username") != "ab" {
		t.Fatalf("taken up: %q %q", s.String("machine.a.username"), s.String("machine.a.b.username"))
	}
	if err := s.PruneScope(MachinePrefix, func(name string) bool { return name == "a.b" || name == "x.y.z" }); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"machine.a.b.username": "ab"`) || !strings.Contains(string(data), `"machine.x.y.z.username": "xyz"`) || strings.Contains(string(data), `"machine.a.username"`) {
		t.Errorf("the file after the prune: %s", data)
	}
	if err := s.AddScope(MachinePrefix, "a", MachineKeys("ada"), map[string]any{KeyUsername: "anew"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveScope(MachinePrefix, "a"); err != nil {
		t.Fatal(err)
	}
	if s.String("machine.a.b.username") != "ab" {
		t.Errorf("removing a took a.b's key: %q", s.String("machine.a.b.username"))
	}
	if err := s.AddScope(MachinePrefix, "a", MachineKeys("ada"), map[string]any{KeyUsername: "anew"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameScope(MachinePrefix, "a", "c"); err != nil {
		t.Fatal(err)
	}
	if s.String("machine.c.username") != "anew" || s.String("machine.a.b.username") != "ab" || s.String("machine.c.b.username") != "" {
		t.Errorf("after a's rename: c %q, a.b %q, c.b %q", s.String("machine.c.username"), s.String("machine.a.b.username"), s.String("machine.c.b.username"))
	}
	if err := s.RenameScope(MachinePrefix, "a.b", "A.B"); err != nil {
		t.Fatal(err)
	}
	if s.String("machine.A.B.username") != "ab" || s.String("machine.a.b.username") != "" {
		t.Errorf("after a.b's rename: %q %q", s.String("machine.A.B.username"), s.String("machine.a.b.username"))
	}
}

func TestAMachinesKeyIsPendingAgainstWhatItRunsWith(t *testing.T) {
	s, _ := fresh(t)
	if err := s.AddScope(MachinePrefix, "m", MachineKeys("ada"), nil); err != nil {
		t.Fatal(err)
	}
	pending := func(key string) bool {
		got, err := s.Get(key)
		if err != nil {
			t.Fatal(err)
		}
		return got[key].Pending
	}
	key := "machine.m." + api.MachineKeyHTTPPort
	if ch, _ := s.Set(map[string]any{key: float64(8080)}); len(ch) != 1 || ch[0].Pending {
		t.Errorf("a stopped machine's change is pending: %+v", ch)
	}
	s.MachineStarted("m", s.MachineValues("m"))
	if ch, _ := s.Set(map[string]any{key: float64(9090)}); len(ch) != 1 || !ch[0].Pending || !pending(key) {
		t.Errorf("a running machine's change is not pending: %+v", ch)
	}
	if got := s.PendingRestart(); len(got) != 0 {
		t.Errorf("a machine's key waits for a VM restart: %v", got)
	}
	if err := s.RenameScope(MachinePrefix, "m", "n"); err != nil {
		t.Fatal(err)
	}
	if key = "machine.n." + api.MachineKeyHTTPPort; !pending(key) {
		t.Errorf("%s is not pending after the rename", key)
	}
	s.MachineStopped("n")
	if pending(key) {
		t.Errorf("%s is pending with its machine stopped", key)
	}
	s.MachineStarted("n", map[string]any{api.MachineKeyHTTPPort: int64(8080)})
	s.VMStopped()
	if pending(key) {
		t.Errorf("%s is pending with the VM stopped", key)
	}
	s.MachineStarted("n", map[string]any{api.MachineKeyHTTPPort: int64(8080)})
	if err := s.RemoveScope(MachinePrefix, "n"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddScope(MachinePrefix, "n", MachineKeys("ada"), map[string]any{api.MachineKeyHTTPPort: float64(9090)}); err != nil {
		t.Fatal(err)
	}
	if pending(key) {
		t.Errorf("a new machine of a deleted one's name has its change pending")
	}
}
