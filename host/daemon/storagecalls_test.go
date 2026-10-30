package daemon

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fev/host/daemon/vm"
	api "fev/proto/daemon"
)

func storageFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	share := t.TempDir()
	m := &Machines{MacUser: "ada", UID: 501}
	f.opts.Modules = append(f.opts.Modules, m, &StorageCalls{Machines: m, Share: &vm.Share{Root: share, Deny: []string{filepath.Join(share, "hidden")}}})
	return f.start(), share
}

func TestStorageUsageIsTheGuestsAccount(t *testing.T) {
	f, _ := storageFixture(t)
	c := f.client()
	if _, err := c.StorageUsage(bg, api.UsageParams{}); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("usage with the VM stopped: %v", err)
	}
	if st := f.d.hub.Status(); st.State != api.StateStopped {
		t.Fatalf("usage started the VM: %+v", st)
	}
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	f.running()
	u, err := c.StorageUsage(bg, api.UsageParams{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(u)
	if err := api.Conforms(b, api.StorageUsage{}); err != nil || u.EngineBytes == 0 || u.Engine != nil || u.ImageFileBytes != 0 {
		t.Errorf("usage %s: %v", b, err)
	}
}

func TestStorageSalvageIsAnOperationThroughTheShare(t *testing.T) {
	f, share := storageFixture(t)
	c := f.client()
	ended := make(chan api.Operation, 4)
	_, stop, err := c.Subscribe(bg, api.Events{Operation: func(op api.Operation) {
		if op.Method == api.MethodStorageSalvage && op.State != api.OperationRunning {
			ended <- op
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for _, bad := range []string{"relative", "/elsewhere/x", filepath.Join(share, "hidden", "x")} {
		if _, err := c.StorageSalvage(bg, api.SalvageParams{Dest: bad}); err == nil {
			t.Errorf("salvage to %s was taken", bad)
		}
	}
	dest := filepath.Join(share, "Salvage")
	op, err := c.StorageSalvage(bg, api.SalvageParams{Dest: dest, Volumes: []string{"pgdata"}, Machines: []string{"dev"}})
	if err != nil {
		t.Fatal(err)
	}
	if op.Method != api.MethodStorageSalvage || op.Target != dest || op.State != api.OperationRunning {
		t.Errorf("operation %+v", op)
	}
	select {
	case end := <-ended:
		var r api.Salvaged
		if end.ID != op.ID || end.State != api.OperationDone || json.Unmarshal(end.Result, &r) != nil || r.Files != 2 {
			t.Errorf("end %+v", end)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no end of the salvage")
	}
	if st := f.d.hub.Status(); st.State == api.StateStopped {
		t.Errorf("the VM after a salvage: %+v", st)
	}

	if _, err := c.ConfigSet(bg, map[string]any{api.KeyFilesShare: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.VMRestart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	f.running()
	if _, err := c.StorageSalvage(bg, api.SalvageParams{Dest: dest}); err == nil || !strings.Contains(err.Error(), "files.share") {
		t.Errorf("salvage with the share off: %v", err)
	}
}

func TestShareDestIsThePathUnderTheShare(t *testing.T) {
	f := started(t)
	s := &StorageCalls{host: moduleHost{f.d}, Share: &vm.Share{Root: "/", Deny: []string{"/Users/u/.fev"}}}
	for in, want := range map[string]string{"/Users/u/Salvage": "/Users/u/Salvage", "/Users/u/a/../b": "/Users/u/b"} {
		if got, err := s.shareDest(in); err != nil || got != want {
			t.Errorf("%s: %q, %v", in, got, err)
		}
	}
	if _, err := s.shareDest("/Users/u/.fev/x"); err == nil {
		t.Error("a hidden folder was taken")
	}
	s.Share = &vm.Share{Root: "/tmp/root"}
	if got, err := s.shareDest("/tmp/root/out"); err != nil || got != "/out" {
		t.Errorf("under a root: %q, %v", got, err)
	}
	for _, bad := range []string{"/tmp/rootx/out", "/tmp", "/tmp/root/../x"} {
		if _, err := s.shareDest(bad); err == nil {
			t.Errorf("%s was taken", bad)
		}
	}
	s.Share = nil
	if _, err := s.shareDest("/x"); err == nil {
		t.Error("no share, and a salvage was taken")
	}
}
