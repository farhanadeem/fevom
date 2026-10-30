package machines

import (
	"os"
	"path/filepath"
	"testing"

	api "fev/proto/agent"
)

func TestTheHostNameIsSwappedWhereTheTemplatesWroteIt(t *testing.T) {
	if got, ok := swapHostname("dev\n", "dev", "web"); !ok || got != "web\n" {
		t.Errorf("hostname %q %v", got, ok)
	}
	if got, ok := swapHostname("mybox\n", "dev", "web"); ok || got != "mybox\n" {
		t.Errorf("a name the user chose %q %v", got, ok)
	}
	in := "127.0.1.1\tdev\n127.0.0.1\tlocalhost dev.example devx\n# dev is mine\n::1   localhost ip6-localhost\n"
	want := "127.0.1.1\tweb\n127.0.0.1\tlocalhost web.example devx\n# dev is mine\n::1   localhost ip6-localhost\n"
	if got, ok := swapHosts(in, "dev", "web"); !ok || got != want {
		t.Errorf("hosts:\n%q\nwant\n%q", got, want)
	}
	if _, ok := swapHosts("127.0.0.1 localhost\n", "dev", "web"); ok {
		t.Error("hosts without the name changed")
	}
	if got, _ := swapHosts("dev other\n", "dev", "web"); got != "dev other\n" {
		t.Errorf("the address field: %q", got)
	}
	if got, ok := swapMetaHostname("instance-id: x\nlocal-hostname: dev\n", "dev", "web"); !ok || got != "instance-id: x\nlocal-hostname: web\n" {
		t.Errorf("meta-data %q", got)
	}
}

func TestTheCloudInstanceIsNew(t *testing.T) {
	in := "instance-id: dev\nlocal-hostname: dev\nextra:\n  instance-id: not-this\n"
	if got := resetMetaData(in, "u-1", "web"); got != "instance-id: u-1\nlocal-hostname: web\nextra:\n  instance-id: not-this\n" {
		t.Errorf("clone %q", got)
	}
	if got := resetMetaData(in, "u-2", ""); got != "instance-id: u-2\nlocal-hostname: dev\nextra:\n  instance-id: not-this\n" {
		t.Errorf("import %q", got)
	}
	if got := resetMetaData("", "u-3", ""); got != "instance-id: u-3\n" {
		t.Errorf("empty %q", got)
	}
	if got := machineID("8F14E45F-CEEA-467A-9575-0C7A3C5D2B11"); got != "8f14e45fceea467a95750c7a3c5d2b11" {
		t.Errorf("machine-id %q", got)
	}
	for name, want := range map[string]bool{
		"ssh_host_ed25519_key": true, "ssh_host_rsa_key.pub": true, "sshd_config": false, "ssh_host_ed25519_key.bak": false,
	} {
		if isSSHHostKey(name) != want {
			t.Errorf("%s: %v", name, !want)
		}
	}
}

func TestABuildsTemplatesAreKept(t *testing.T) {
	dir := t.TempDir()
	old := api.ImageRef{Distro: "alpine", Release: "3.24", Arch: "arm64", Variant: "default", Serial: "20261012_13:01"}
	newer := old
	newer.Serial = "20261013_13:01"
	ts := []api.ImageTemplate{{Path: "/etc/hostname", When: []string{"create"}, Template: "{{ container.name }}\n"}}
	if _, ok := ReadImageTemplates(dir, old); ok {
		t.Fatal("templates of a build never kept")
	}
	for _, ref := range []api.ImageRef{old, newer} {
		if err := WriteImageTemplates(dir, ref, ts); err != nil {
			t.Fatal(err)
		}
	}
	PruneImageTemplates(dir, newer)
	if _, ok := ReadImageTemplates(dir, old); ok {
		t.Error("the older build's templates were kept")
	}
	got, ok := ReadImageTemplates(dir, newer)
	if !ok || len(got) != 1 || got[0].Template != ts[0].Template {
		t.Errorf("read %+v %v", got, ok)
	}
	none := newer
	none.Variant = "cloud"
	WriteImageTemplates(dir, none, nil)
	if got, ok := ReadImageTemplates(dir, none); !ok || len(got) != 0 {
		t.Errorf("none: %+v %v", got, ok)
	}
	if err := WriteRecord(dir, &Record{Name: "dev"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, RecordsDir, "dev.console"), []byte("x"), 0o600)
	names, err := Records(dir)
	if err != nil || len(names) != 1 || names[0] != "dev" {
		t.Errorf("records %v %v", names, err)
	}
	if err := RemoveRecord(dir, "dev"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRecord(dir, "dev"); err != nil {
		t.Errorf("removing a record that is gone: %v", err)
	}
}

func TestZramsMajorIsReadFromTheBlockSection(t *testing.T) {
	text := "Character devices:\n  1 mem\n252 zram\n\nBlock devices:\n  7 loop\n253 virtblk\n254 zram\n259 blkext\n"
	if m, ok := parseBlockMajor(text, "zram"); !ok || m != 254 {
		t.Errorf("zram: %d %v", m, ok)
	}
	if _, ok := parseBlockMajor("Block devices:\n  7 loop\n", "zram"); ok {
		t.Error("a zram that is not there")
	}
}
