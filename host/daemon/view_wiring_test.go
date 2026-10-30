package daemon

import (
	"log/slog"
	"path/filepath"
	"slices"
	"testing"

	"fev/host/nfsview"
	"fev/proto/bridge"
)

func TestAMachinesPlaceInTheViewIsGivenOnlyWhileMountedAndNotIsolated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Fevom")
	v := &View{Dir: dir, Isolated: func() []string { return []string{"iso"} }}
	if got := v.MachinePath("u"); got != "" {
		t.Fatalf("unmounted: %q", got)
	}
	v.mounted = true
	if got := v.MachinePath("u"); got != filepath.Join(dir, "u") {
		t.Fatalf("mounted: %q", got)
	}
	for _, name := range []string{"iso", "", "a/b"} {
		if got := v.MachinePath(name); got != "" {
			t.Fatalf("%q: %q", name, got)
		}
	}
	var none *View
	if none.MachinePath("u") != "" {
		t.Fatal("no view gave a place")
	}
}

func TestTheMacCommandsBridgeIsGivenTheMachinesPlaceInTheView(t *testing.T) {
	m := &Machines{View: func(machine string) string { return "/Users/u/Fevom/" + machine }}
	b := newBridgeServer(m, slog.Default())
	if got := b.viewOf("u"); got != "/Users/u/Fevom/u" {
		t.Fatalf("the bridge's view of u is %q", got)
	}
	if dir, ok := bridge.MacDirIn("/home/u", "/", true, b.viewOf("u")); !ok || dir != "/Users/u/Fevom/u/home/u" {
		t.Fatalf("MacDirIn: %q %v", dir, ok)
	}
}

func TestIsolatedMachinesAreNamedToTheView(t *testing.T) {
	m := &Machines{entries: map[string]*machineEntry{
		"trusted": {}, "was": {Isolated: true},
	}}
	if got := m.IsolatedNames(); !slices.Equal(got, []string{"was"}) {
		t.Fatalf("isolated %v", got)
	}
}

func TestThePlacesTheGuestSaysWentAreLetGo(t *testing.T) {
	k, err := nfsview.Keep(filepath.Join(t.TempDir(), "Fevom"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	var forgot []string
	old := viewForget
	viewForget = func(_ *nfsview.Keeper, rel string) (string, bool) { forgot = append(forgot, rel); return "", false }
	defer func() { viewForget = old }()
	v := &View{Dir: "/x", keeper: k, log: slog.Default()}
	v.removed([]string{"docker/containers/web", "u"})
	if !slices.Equal(forgot, []string{"docker/containers/web", "u"}) {
		t.Fatalf("forgot %v", forgot)
	}
}
