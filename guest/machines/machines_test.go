package machines

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "fev/proto/agent"
)

func TestAMachineIsItsFilesAndItsRecord(t *testing.T) {
	data := t.TempDir()
	for _, d := range []string{"ubuntu", "orphan", ".images", "Bad_Name"} {
		if err := os.Mkdir(filepath.Join(data, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	uuid, err := NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	rec := &Record{Name: "ubuntu", UUID: uuid, Image: api.ImageRef{Distro: "ubuntu", Release: "noble", Arch: "arm64", Variant: "default", Serial: "20261014_07:42"}}
	if err := WriteRecord(data, rec); err != nil {
		t.Fatal(err)
	}
	if err := WriteRecord(data, &Record{Name: "ghost", UUID: uuid}); err != nil {
		t.Fatal(err)
	}
	names, err := Names(data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "ubuntu" {
		t.Errorf("names %v", names)
	}
	got, err := ReadRecord(data, "ubuntu")
	if err != nil || *got != *rec {
		t.Errorf("read %+v, %v", got, err)
	}
	for _, name := range []string{"orphan", "ghost", "nothing"} {
		if _, err := ReadRecord(data, name); !errors.Is(err, ErrNoMachine) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ReadRecord(data, "../etc"); err == nil || errors.Is(err, ErrNoMachine) {
		t.Errorf("a name that is not a machine's: %v", err)
	}
	if err := WriteRecord(data, &Record{Name: "Bad_Name"}); err == nil {
		t.Error("a record for a name a machine cannot have")
	}
	entries, _ := os.ReadDir(filepath.Join(data, RecordsDir))
	if len(entries) != 2 {
		t.Errorf("records directory holds %d files", len(entries))
	}
	os.WriteFile(RecordPath(data, "orphan"), []byte(`{"name":"ubuntu"}`), 0o600)
	if _, err := ReadRecord(data, "orphan"); err == nil {
		t.Error("a record that names another machine was taken")
	}
	if u2, _ := NewUUID(); u2 == uuid || len(u2) != 36 || u2[14] != '4' {
		t.Errorf("uuid %q after %q", u2, uuid)
	}
}

func TestTheInitIsFoundInsideTheRoot(t *testing.T) {
	for name, c := range map[string]struct {
		links map[string]string
		files []string
		want  string
	}{
		"ubuntu": {map[string]string{"sbin": "usr/sbin", "lib": "usr/lib", "usr/sbin/init": "../lib/systemd/systemd"}, []string{"usr/lib/systemd/systemd"}, InitSystemd},
		"fedora": {map[string]string{"sbin": "usr/bin", "usr/sbin": "bin", "usr/bin/init": "/usr/lib/systemd/systemd"}, []string{"usr/lib/systemd/systemd"}, InitSystemd},
		"alpine": {map[string]string{"sbin/init": "/bin/busybox"}, []string{"bin/busybox"}, InitBusybox},
		"openrc": {map[string]string{"sbin/init": "openrc-init"}, []string{"sbin/openrc-init"}, InitOpenRC},
		"void":   {map[string]string{"sbin/init": "runit-init"}, []string{"sbin/runit-init"}, InitRunit},
		"sysv":   {nil, []string{"sbin/init"}, InitSysvinit},
	} {
		root := t.TempDir()
		for _, f := range c.files {
			os.MkdirAll(filepath.Join(root, filepath.Dir(f)), 0o755)
			os.WriteFile(filepath.Join(root, f), nil, 0o755)
		}
		for l, target := range c.links {
			os.MkdirAll(filepath.Join(root, filepath.Dir(l)), 0o755)
			if err := os.Symlink(target, filepath.Join(root, l)); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := InitKind(root); got != c.want || err != nil {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sbin"), 0o755)
	os.Symlink("../../../../bin/busybox", filepath.Join(root, "sbin/init"))
	if got, err := InitKind(root); err == nil {
		t.Errorf("a link out of the root was followed out of it: %q", got)
	}
	if _, err := InitKind(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no /sbin/init") {
		t.Errorf("no init: %v", err)
	}
	loop := t.TempDir()
	os.MkdirAll(filepath.Join(loop, "sbin"), 0o755)
	os.Symlink("init", filepath.Join(loop, "sbin/init"))
	if _, err := InitKind(loop); err == nil {
		t.Error("a link to itself")
	}
}

func TestARunEndsAsItsInitSays(t *testing.T) {
	for name, c := range map[string]struct {
		signaled     bool
		signal, code int
		asked        bool
		want         ending
	}{
		"reboot inside":       {true, 1, 0, false, ending{again: true, last: api.ExitReboot}},
		"poweroff inside":     {true, 2, 0, false, ending{state: api.MachineStopped, last: api.ExitHalt}},
		"halt by a stop":      {true, 2, 0, true, ending{state: api.MachineStopped, last: api.ExitStopped}},
		"reboot under a stop": {true, 1, 0, true, ending{state: api.MachineStopped, last: api.ExitStopped}},
		"killed by a stop":    {true, 9, 0, true, ending{state: api.MachineStopped, last: api.ExitStopped}},
		"crash":               {true, 11, 0, false, ending{state: api.MachineFailed, last: "signal 11"}},
		"init exits":          {false, 0, 1, false, ending{state: api.MachineFailed, last: "exit 1"}},
	} {
		if got := endOf(c.signaled, c.signal, c.code, c.asked); got != c.want {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}
}
