package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func TestDockerVolumeCommandsAreTheirCalls(t *testing.T) {
	cloned := api.Volume{Name: "pgcopy", Labels: map[string]string{"com.docker.compose.project": "shop"}, Files: 1288}
	for _, argv := range [][]string{{"docker", "volume", "clone", "pgdata", "pgcopy"}, {"docker", "volume", "clone", "-n", "pgcopy", "pgdata"}, {"docker", "volume", "clone", "pgdata", "pgcopy", "--name", "pgcopy"}} {
		f := newFake(t)
		f.on(api.MethodVolumesClone, func(_ *jsonrpc.Conn, params json.RawMessage) (any, error) { return cloned, nil })
		r := f.run(argv...).want(t, 0)
		if got := f.params(api.MethodVolumesClone); got != `{"name":"pgdata","new_name":"pgcopy"}` {
			t.Errorf("%v: %s", argv, got)
		}
		if r.out != "volume: pgcopy\nlabel: com.docker.compose.project=shop\nentries: 1288\n" {
			t.Errorf("%v: %q", argv, r.out)
		}
	}
	f := newFake(t)
	f.on(api.MethodVolumesClone, func(_ *jsonrpc.Conn, params json.RawMessage) (any, error) { return cloned, nil })
	for _, argv := range [][]string{{"docker", "volume", "clone", "pgdata"}, {"docker", "volume", "clone", "pgdata", "a", "-n", "b"}, {"docker", "volume", "clone"}} {
		if r := f.run(argv...); r.code != ExitUsage {
			t.Errorf("%v: %+v", argv, r)
		}
	}
	if r := f.run("docker", "volume", "clone", "pgdata", "pgcopy", "--format", "json").want(t, 0); !strings.Contains(r.out, `"name": "pgcopy"`) {
		t.Errorf("json: %q", r.out)
	}
	f = newFake(t)

	exported, _ := json.Marshal(api.VolumeExported{Path: "/tmp/pgdata.tar.zst", Bytes: 9 << 20})
	operation(f, api.MethodVolumesExport, api.Operation{State: api.OperationDone, Result: exported})
	r := f.run("docker", "volume", "export", "pgdata", "rel.tar.zst").want(t, 0)
	wd, _ := os.Getwd()
	if got := f.params(api.MethodVolumesExport); got != `{"name":"pgdata","path":"`+filepath.Join(wd, "rel.tar.zst")+`"}` {
		t.Errorf("export %s", got)
	}
	if r.out != "archive: /tmp/pgdata.tar.zst\nsize: 9.0 MiB\n" {
		t.Errorf("export: %q", r.out)
	}
	archive := filepath.Join(t.TempDir(), "pgdata.tar.zst")
	os.WriteFile(archive, []byte("x"), 0o644)
	imported, _ := json.Marshal(api.Volume{Name: "restored", Files: 3})
	operation(f, api.MethodVolumesImport, api.Operation{State: api.OperationDone, Result: imported})
	if r := f.run("docker", "volume", "import", "-n", "restored", archive).want(t, 0); r.out != "volume: restored\nentries: 3\n" {
		t.Errorf("import: %q", r.out)
	}
	if got := f.params(api.MethodVolumesImport); got != `{"path":"`+archive+`","name":"restored"}` {
		t.Errorf("import %s", got)
	}
	if r := f.run("docker", "volume", "import", filepath.Join(t.TempDir(), "none.tar.zst")); r.code != ExitFailed {
		t.Errorf("an import of no file: %+v", r)
	}
	operation(f, api.MethodVolumesExport, api.Operation{State: api.OperationFailed, Error: "no such volume: nosuch"})
	if r := f.run("docker", "volume", "export", "nosuch", "/tmp/x.tar.zst"); r.code != ExitFailed || !strings.HasSuffix(r.err, "\nfev: export nosuch: no such volume: nosuch\n") {
		t.Errorf("a failed export: %+v", r)
	}
}
