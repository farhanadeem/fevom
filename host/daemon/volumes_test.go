package daemon

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"fev/host/docker/fakeguest"
	api "fev/proto/daemon"
)

func (f *machinesFixture) fakeEngine(method, path string, body any, into any) {
	f.t.Helper()
	sock := filepath.Join(f.layout.VMDir(), fmt.Sprintf("vsock_%d.sock", fakeguest.PortEngine))
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	defer c.CloseIdleConnections()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://docker"+path, rd)
	resp, err := c.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		f.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	if into != nil {
		json.Unmarshal(b, into)
	}
}

func TestVolumesAreClonedExportedAndImported(t *testing.T) {
	f := machinesDaemon(t)
	if _, err := f.c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	f.running()
	files := map[string]string{"PG_VERSION": "17\n", "base/16384/1259": strings.Repeat("x", 100000)}
	labels := map[string]string{"com.docker.compose.project": "shop"}
	f.fakeEngine(http.MethodPost, "/_fake/volume-data", fakeguest.VolumeData{Name: "pgdata", Labels: labels, Files: files}, nil)

	v, err := f.c.CloneVolume(bg, "pgdata", "pgcopy")
	if err != nil || v.Name != "pgcopy" || v.Labels["com.docker.compose.project"] != "shop" || v.Files != 3 {
		t.Fatalf("clone: %+v %v", v, err)
	}
	var got fakeguest.VolumeData
	f.fakeEngine(http.MethodGet, "/_fake/volume-data/pgcopy", nil, &got)
	if got.Files["base/16384/1259"] != files["base/16384/1259"] || got.Labels["com.docker.compose.project"] != "shop" {
		t.Fatalf("the clone holds %d files, labels %v", len(got.Files), got.Labels)
	}
	for _, bad := range [][2]string{{"pgdata", "a/b"}, {"..", "x1"}, {"pgdata", "x"}} {
		if _, err := f.c.CloneVolume(bg, bad[0], bad[1]); err == nil {
			t.Errorf("clone %q to %q was taken", bad[0], bad[1])
		}
	}
	if _, err := f.c.CloneVolume(bg, "nosuch", "x1"); err == nil || !strings.Contains(err.Error(), "no such volume") {
		t.Errorf("clone of no volume: %v", err)
	}
	if _, err := f.c.CloneVolume(bg, "pgdata", "pgcopy"); err == nil || !strings.Contains(err.Error(), "has a volume named pgcopy") {
		t.Errorf("clone over a volume: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "pgdata.tar.zst")
	exp, err := f.c.ExportVolume(bg, api.VolumesExportParams{Name: "pgdata", Path: path})
	if err != nil || exp.Method != api.MethodVolumesExport || exp.Target != "pgdata" {
		t.Fatalf("export: %+v %v", exp, err)
	}
	end := f.ended(exp.ID)
	var r api.VolumeExported
	json.Unmarshal(end.Result, &r)
	st, err := os.Stat(path)
	if end.State != api.OperationDone || err != nil || r.Path != path || r.Bytes != st.Size() {
		t.Fatalf("the export ended %+v: %+v %v", end, r, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the export left %d files", len(entries))
	}
	zf, _ := os.Open(path)
	zr, _ := zstd.NewReader(zf)
	tr := tar.NewReader(zr)
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	zr.Close()
	zf.Close()
	if strings.Join(names, " ") != "_fev/ _fev/v1/ _fev/v1/config.json data/ data/PG_VERSION data/base/16384/1259" {
		t.Fatalf("the archive holds %q", names)
	}
	if _, err := f.c.ExportVolume(bg, api.VolumesExportParams{Name: "pgdata", Path: path}); err == nil {
		t.Error("an export over an existing file was begun without replace")
	}
	if _, err := f.c.ExportVolume(bg, api.VolumesExportParams{Name: "pgdata", Path: "rel.tar.zst"}); err == nil {
		t.Error("a relative path was taken")
	}
	exp, err = f.c.ExportVolume(bg, api.VolumesExportParams{Name: "nosuch", Path: filepath.Join(dir, "x.tar.zst")})
	if err == nil {
		if end := f.ended(exp.ID); end.State != api.OperationFailed || !strings.Contains(end.Error, "no such volume") {
			t.Errorf("the export of no volume ended %+v", end)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("a failed export left %d files", len(entries))
	}

	imp, err := f.c.ImportVolume(bg, api.VolumesImportParams{Path: path, Name: "restored"})
	if err != nil || imp.Target != "restored" {
		t.Fatalf("import: %+v %v", imp, err)
	}
	end = f.ended(imp.ID)
	var made api.Volume
	json.Unmarshal(end.Result, &made)
	if end.State != api.OperationDone || made.Name != "restored" || made.Labels["com.docker.compose.project"] != "shop" || end.TotalBytes != st.Size() {
		t.Fatalf("the import ended %+v: %+v", end, made)
	}
	got = fakeguest.VolumeData{}
	f.fakeEngine(http.MethodGet, "/_fake/volume-data/restored", nil, &got)
	if len(got.Files) != 2 || got.Files["PG_VERSION"] != "17\n" || got.Files["base/16384/1259"] != files["base/16384/1259"] {
		t.Fatalf("the import holds %v", got.Files)
	}
	imp, err = f.c.ImportVolume(bg, api.VolumesImportParams{Path: path})
	if err == nil {
		if end := f.ended(imp.ID); end.State != api.OperationFailed || !strings.Contains(end.Error, "has a volume named pgdata") {
			t.Errorf("an import under a name in use ended %+v", end)
		}
	}
	f.create("dev", "ubuntu")
	mpath := filepath.Join(dir, "dev.tar.zst")
	mexp, err := f.c.ExportMachine(bg, api.MachinesExportParams{Name: "dev", Path: mpath})
	if err != nil || f.ended(mexp.ID).State != api.OperationDone {
		t.Fatalf("the machine's export: %v", err)
	}
	if _, err := f.c.ImportVolume(bg, api.VolumesImportParams{Path: mpath, Name: "v2"}); err == nil || !strings.Contains(err.Error(), "not a Docker volume's archive") {
		t.Errorf("a machine's archive as a volume's: %v", err)
	}
	if _, err := f.c.ImportMachine(bg, api.MachinesImportParams{Path: path, Name: "m2"}); err == nil || !strings.Contains(err.Error(), "Docker volume's") {
		t.Errorf("a volume's archive as a machine's: %v", err)
	}
}
