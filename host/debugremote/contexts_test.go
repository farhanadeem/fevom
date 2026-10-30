package debugremote_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"fev/host/debugremote"
	"fev/host/debugremote/fakeengine"
)

func writeContext(t *testing.T, config, name, host, desc string) {
	t.Helper()
	digest := hex.EncodeToString(sum(name))
	dir := filepath.Join(config, "contexts", "meta", digest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"Name":     name,
		"Metadata": map[string]any{"Description": desc},
		"Endpoints": map[string]any{
			"docker": map[string]any{"Host": host},
		},
	}
	b, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func sum(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

func TestContextsListsTheConfigsContexts(t *testing.T) {
	config := t.TempDir()
	writeContext(t, config, "work", "ssh://deploy@build.example", "staging builder")
	writeContext(t, config, "colima", "unix:///Users/x/.colima/docker.sock", "")
	os.WriteFile(filepath.Join(config, "config.json"), []byte(`{"currentContext":"colima"}`), 0o644)

	list, err := debugremote.Contexts(config, "unix:///var/run/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 contexts (default + 2), got %d: %+v", len(list), list)
	}
	if list[0].Name != "default" {
		t.Errorf("default is not first: %+v", list)
	}
	byName := map[string]debugremote.ContextInfo{}
	for _, c := range list {
		byName[c.Name] = c
	}
	if c := byName["colima"]; !c.Current {
		t.Errorf("colima should be current: %+v", c)
	}
	if byName["default"].Current {
		t.Errorf("default should not be current when colima is")
	}
	if c := byName["work"]; c.Host != "ssh://deploy@build.example" || c.Description != "staging builder" {
		t.Errorf("work context wrong: %+v", c)
	}
}

func TestContextsWithNoStoreIsJustDefault(t *testing.T) {
	config := t.TempDir()
	list, err := debugremote.Contexts(config, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "default" || !list[0].Current {
		t.Fatalf("want just the current default, got %+v", list)
	}
	if list[0].Host != debugremote.DefaultSocket {
		t.Errorf("default host = %q, want %q", list[0].Host, debugremote.DefaultSocket)
	}
}

func TestLookupResolvesAContext(t *testing.T) {
	config := t.TempDir()
	writeContext(t, config, "work", "ssh://x@h", "")
	if c, ok := debugremote.Lookup(config, "", "work"); !ok || c.Host != "ssh://x@h" {
		t.Errorf("Lookup(work) = %+v, %v", c, ok)
	}
	if _, ok := debugremote.Lookup(config, "", "nope"); ok {
		t.Error("Lookup(nope) should be false")
	}
	if c, ok := debugremote.Lookup(config, "unix:///d.sock", ""); !ok || c.Name != "default" {
		t.Errorf("Lookup(\"\") should be default, got %+v %v", c, ok)
	}
}

func TestEngineListsOverDirectUnixDial(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "e.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fe := fakeengine.New("aarch64", ln)
	defer fe.Close()
	fe.AddContainer("api", "ghcr.io/x/api:latest", true)
	fe.AddContainer("db", "postgres:17", false)
	fe.AddImage("ghcr.io/x/api:latest")

	tr := &debugremote.Transport{Ctx: debugremote.Context{Host: "unix://" + sock}}
	eng := debugremote.NewEngine(tr)
	ctx := context.Background()

	cs, err := eng.ContainerList(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("want 2 containers (all), got %d", len(cs))
	}
	running, err := eng.ContainerList(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || running[0].Image != "ghcr.io/x/api:latest" {
		t.Fatalf("want 1 running, got %+v", running)
	}
	im, err := eng.ImageList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(im) != 1 || len(im[0].RepoTags) != 1 || im[0].RepoTags[0] != "ghcr.io/x/api:latest" {
		t.Fatalf("want 1 image tagged, got %+v", im)
	}
}
