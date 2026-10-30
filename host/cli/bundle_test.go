package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fev/host/daemon/home"
	"fev/host/docker/dockerctx"
)

func fakeBundle(t *testing.T, files ...string) string {
	t.Helper()
	c := filepath.Join(t.TempDir(), "Fevom.app", "Contents")
	for _, f := range append([]string{"Info.plist"}, files...) {
		p := filepath.Join(c, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestTheCLIOfABundleStartsTheBundlesDaemon(t *testing.T) {
	c := fakeBundle(t, "MacOS/bin/fev", "Helpers/fevd")
	if got, want := daemonNear(filepath.Join(c, "MacOS/bin/fev")), filepath.Join(c, "Helpers/fevd"); got != want {
		t.Errorf("the bundle's fev starts %q, want %q", got, want)
	}
	out := filepath.Join(t.TempDir(), "out")
	os.MkdirAll(out, 0o755)
	os.WriteFile(filepath.Join(out, "fevd"), nil, 0o755)
	if got := daemonNear(filepath.Join(out, "fev")); got != filepath.Join(out, "fevd") {
		t.Errorf("the tree's fev starts %q", got)
	}
}

func TestTheCLIOfABundleInstallsTheBundlesHelper(t *testing.T) {
	c := fakeBundle(t, "MacOS/bin/fev", "Helpers/fev-privhelper")
	got, err := helperNear(filepath.Join(c, "MacOS/bin/fev"))
	if want := filepath.Join(c, "Helpers/fev-privhelper"); got != want {
		t.Errorf("the bundle's fev installs %q (%v), want %q", got, err, want)
	}
}

func TestTheCLIOfABundleUsesTheBundlesTools(t *testing.T) {
	c := fakeBundle(t, "MacOS/bin/fev", "MacOS/xbin/docker", "MacOS/xbin/docker-compose")
	l := home.Layout{Home: filepath.Join(t.TempDir(), ".fev")}
	if got, want := toolsFor(l, filepath.Join(c, "MacOS/bin/fev")), filepath.Join(c, "MacOS/xbin"); got != want {
		t.Errorf("the bundle's fev uses the tools in %q, want %q", got, want)
	}
	if got, want := toolsFor(l, filepath.Join(t.TempDir(), "out", "fev")), filepath.Join(l.Home, "tools"); got != want {
		t.Errorf("the tree's fev uses the tools in %q, want %q", got, want)
	}
}

func TestUninstallingPutsBackTheDockerContext(t *testing.T) {
	h := newSetupHome(t, map[string]string{
		".docker/config.json": "{\n  \"auths\": {}\n}\n",
		".docker/contexts/meta/3fd30542fe3f61b14bd4a4b2dc0b6fb30fa6f63ebce52dd1778aaa8c4dc02cff/meta.json": `{"Name":"mine","Metadata":{},"Endpoints":{"docker":{"Host":"unix:///tmp/mine.sock","SkipTLSVerify":false}}}`,
	})
	dc := filepath.Join(h.user, ".docker")
	t.Setenv("DOCKER_CONFIG", dc)
	fevCtx := dockerctx.Store{Dir: dc, Socket: filepath.Join(h.fev, "run", "docker.sock"), StateFile: filepath.Join(h.fev, "config", "docker-context.json")}
	if err := fevCtx.Ensure(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fevCtx.Select(); err != nil {
		t.Fatal(err)
	}
	if st, _ := fevCtx.Status(); !st.Selected {
		t.Fatalf("fev's context not selected: %+v", st)
	}
	r := h.run("--undo", "--remove-context").want(t, 0)
	data, _ := os.ReadFile(filepath.Join(dc, "config.json"))
	if st, _ := fevCtx.Status(); st.Current != "default" || !strings.Contains(string(data), `"auths"`) {
		t.Errorf("config.json after: %s\n%s", data, r.out)
	}
	if st, _ := fevCtx.Status(); st.Exists {
		t.Errorf("fev's context is still defined: %+v\n%s", st, r.out)
	}
	if _, err := os.Stat(filepath.Join(dc, "contexts", "meta")); err != nil {
		t.Errorf("the user's own context went too: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(dc, "contexts", "meta"))
	if len(entries) != 1 {
		t.Errorf("contexts left: %v", entries)
	}
}
