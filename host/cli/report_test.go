package cli

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fev/host/daemon/report"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func TestReportWritesTheBundleAndSaysWhere(t *testing.T) {
	doctorWorld(t)
	f := newFake(t)
	f.then(api.MethodStatsSubscribe, func(c *jsonrpc.Conn, _ json.RawMessage) {
		c.Notify(api.NotifyStatsSample, topSample(10))
	})
	out := t.TempDir()
	r := f.run("report", "-o", out).want(t, 0)
	if !strings.Contains(r.out, "Wrote "+out+"/fev-report-") || !strings.Contains(r.out, "sent nowhere") {
		t.Errorf("report:\n%s", r.out)
	}
	found, _ := filepath.Glob(filepath.Join(out, "fev-report-*.zip"))
	if len(found) != 1 {
		t.Fatalf("%v", found)
	}
	z, err := zip.OpenReader(found[0])
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	var sample api.StatsSample
	for _, f := range z.File {
		names[f.Name] = true
		if f.Name == "daemon/stats.sample.json" {
			rc, _ := f.Open()
			json.NewDecoder(rc).Decode(&sample)
			rc.Close()
		}
	}
	z.Close()
	for _, want := range []string{"daemon/status.json", "daemon/docker.status.json", "daemon/machines.list.json", "daemon/config.list.json", "daemon/stats.sample.json", "doctor.json", "summary.txt"} {
		if !names[want] {
			t.Errorf("%s not in the bundle: %v", want, names)
		}
	}
	if sample.VMM == nil || sample.VMM.FootprintBytes != topSample(10).VMM.FootprintBytes || sample.Guest == nil || len(sample.Guest.Groups) == 0 {
		t.Errorf("stats.sample.json: %+v", sample)
	}
	path := filepath.Join(out, "mine.zip")
	if r := f.run("-q", "report", "-o", path).want(t, 0); strings.TrimSpace(r.out) != path {
		t.Errorf("quiet: %q", r.out)
	}
	if r := f.run("report", "-o", path); r.code != ExitFailed || !strings.Contains(r.err, "exists") {
		t.Errorf("over an existing file: %+v", r)
	}
	r = f.run("--format", "json", "report", "-o", filepath.Join(out, "j.zip")).want(t, 0)
	var w report.Written
	if err := json.Unmarshal([]byte(r.out), &w); err != nil || w.Path != filepath.Join(out, "j.zip") || len(w.Files) == 0 {
		t.Errorf("json %s: %v", r.out, err)
	}

	started := noDaemon(t)
	h := scratchHome(t)
	r = fev(t, h, "", "report", "-o", filepath.Join(out, "none.zip")).want(t, 0)
	z, err = zip.OpenReader(filepath.Join(out, "none.zip"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range z.File {
		if strings.HasPrefix(f.Name, "daemon/") {
			t.Errorf("%s with no daemon", f.Name)
		}
	}
	z.Close()
	if _, err := os.Stat(started); err == nil {
		t.Error("report started a daemon")
	}
}
