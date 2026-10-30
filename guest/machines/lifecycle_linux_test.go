package machines

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTemplatesAreWrittenInsideTheRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	outside := filepath.Join(dir, "outside")
	writeFiles(t, root, map[string]string{"etc/hosts": "old\n", "etc/motd": "mine\n"})
	os.WriteFile(outside, []byte("init's\n"), 0o644)
	os.Chmod(filepath.Join(root, "etc/hosts"), 0o604)
	os.Chown(filepath.Join(root, "etc/hosts"), 0, 4)
	os.Symlink(outside, filepath.Join(root, "etc/hostname"))
	os.MkdirAll(filepath.Join(root, dir), 0o755)
	os.Symlink("../../../outside", filepath.Join(root, "etc/escape"))
	unix.Umask(0o077)
	defer unix.Umask(0o022)
	ts := []api.ImageTemplate{
		{Path: "/etc/hostname", When: []string{"create"}, Template: "{{ container.name }}\n"},
		{Path: "/etc/hosts", When: []string{"create"}, Template: "127.0.1.1\t{{ container.name }}\n"},
		{Path: "/etc/motd", When: []string{"create"}, CreateOnly: true, Template: "theirs\n"},
		{Path: "/etc/escape", When: []string{"create"}, Template: "x\n"},
		{Path: "/var/lib/cloud/seed/nocloud-net/meta-data", When: []string{"create", "copy"}, Template: "instance-id: {{ container.name }}\n"},
		{Path: "/etc/only-copy", When: []string{"copy"}, Template: "copy\n"},
	}
	written, err := applyTemplates(root, ts, TriggerCreate, TemplateContext{Name: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, outside); got != "init's\n" {
		t.Fatalf("a template wrote outside the root: %q", got)
	}
	if got := strings.Join(written, " "); got != "/etc/escape /etc/hostname /etc/hosts /var/lib/cloud/seed/nocloud-net/meta-data" {
		t.Errorf("written: %s", got)
	}
	if got := readFile(t, filepath.Join(root, "outside")); got != "x\n" {
		t.Errorf("the climbing link's target in the root: %q", got)
	}
	if got := readFile(t, filepath.Join(root, outside)); got != "dev\n" {
		t.Errorf("the link's target in the root: %q", got)
	}
	var st unix.Stat_t
	unix.Stat(filepath.Join(root, "etc/hosts"), &st)
	if got := readFile(t, filepath.Join(root, "etc/hosts")); got != "127.0.1.1\tdev\n" || st.Mode&0o7777 != 0o604 || st.Gid != 4 {
		t.Errorf("hosts %q mode %o gid %d", got, st.Mode&0o7777, st.Gid)
	}
	if got := readFile(t, filepath.Join(root, "etc/motd")); got != "mine\n" {
		t.Errorf("a create_only template replaced %q", got)
	}
	seed := filepath.Join(root, "var/lib/cloud/seed/nocloud-net")
	unix.Stat(filepath.Join(seed, "meta-data"), &st)
	if st.Mode&0o7777 != 0o644 {
		t.Errorf("a new file's mode %o", st.Mode&0o7777)
	}
	unix.Stat(seed, &st)
	if st.Mode&0o7777 != 0o755 {
		t.Errorf("a new directory's mode %o", st.Mode&0o7777)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/only-copy")); err == nil {
		t.Error("a copy template was rendered at a create")
	}
}

func TestAnIdentityIsMadeAfresh(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	writeFiles(t, root, map[string]string{
		"etc/machine-id": "0123456789abcdef0123456789abcdef\n",
		"etc/hostname":   "dev\n",
		"etc/hosts":      "127.0.1.1\tdev\n127.0.0.1\tlocalhost\n",
		"var/lib/cloud/seed/nocloud-net/meta-data": "instance-id: 1111\nlocal-hostname: dev\n",
		"etc/ssh/ssh_host_ed25519_key":             "k",
		"etc/ssh/ssh_host_ed25519_key.pub":         "k",
		"etc/ssh/sshd_config":                      "c",
	})
	os.MkdirAll(filepath.Join(root, "var/lib/dbus"), 0o755)
	os.Symlink("/etc/machine-id", filepath.Join(root, "var/lib/dbus/machine-id"))
	uuid := "45c48cce-2e2d-4fbd-a1b2-c3d4e5f60718"
	said, keys, err := resetIdentity(root, identity{UUID: uuid, OldName: "dev", NewName: "dev2"})
	if err != nil {
		t.Fatal(err)
	}
	if keys != 2 {
		t.Errorf("host keys removed: %d", keys)
	}
	for p, want := range map[string]string{
		"etc/machine-id": "45c48cce2e2d4fbda1b2c3d4e5f60718\n",
		"etc/hostname":   "dev2\n",
		"etc/hosts":      "127.0.1.1\tdev2\n127.0.0.1\tlocalhost\n",
		"var/lib/cloud/seed/nocloud-net/meta-data": "instance-id: " + uuid + "\nlocal-hostname: dev2\n",
		"etc/ssh/sshd_config":                      "c",
	} {
		if got := readFile(t, filepath.Join(root, p)); got != want {
			t.Errorf("%s: %q, want %q", p, got, want)
		}
	}
	if fi, err := os.Lstat(filepath.Join(root, "var/lib/dbus/machine-id")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("dbus's link to the machine-id was replaced")
	}
	if _, err := os.Stat(filepath.Join(root, "etc/ssh/ssh_host_ed25519_key")); err == nil {
		t.Error("the source's host key is left")
	}
	t.Logf("said: %v", said)

	root2 := filepath.Join(dir, "root2")
	writeFiles(t, root2, map[string]string{"etc/machine-id": "", "etc/hostname": "dev\n", "var/lib/cloud/seed/nocloud-net/meta-data": "instance-id: 1\nlocal-hostname: dev\n"})
	if _, _, err := resetIdentity(root2, identity{UUID: uuid}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(root2, "etc/hostname")); got != "dev\n" {
		t.Errorf("an import's host name: %q", got)
	}
	if got := readFile(t, filepath.Join(root2, "etc/machine-id")); got != "" {
		t.Errorf("an empty machine-id became %q", got)
	}
	if got := readFile(t, filepath.Join(root2, "var/lib/cloud/seed/nocloud-net/meta-data")); got != "instance-id: "+uuid+"\nlocal-hostname: dev\n" {
		t.Errorf("an import's meta-data: %q", got)
	}

	if _, err := renameHost(root, "dev2", "web"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(root, "etc/hostname")); got != "web\n" {
		t.Errorf("renamed hostname %q", got)
	}
	if got := readFile(t, filepath.Join(root, "var/lib/cloud/seed/nocloud-net/meta-data")); got != "instance-id: "+uuid+"\nlocal-hostname: web\n" {
		t.Errorf("renamed meta-data %q", got)
	}
}

func TestTheViewHoldsMachinesRootsOnly(t *testing.T) {
	dir := t.TempDir()
	view := filepath.Join(dir, "view")
	if err := EnsureView(view, ""); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(view, unix.MNT_DETACH)
	if err := EnsureView(view, ""); err != nil {
		t.Fatal(err)
	}
	mi := readFile(t, "/proc/self/mountinfo")
	n := 0
	for _, line := range strings.Split(mi, "\n") {
		f := strings.Fields(line)
		if len(f) > 6 && f[4] == view {
			n++
			if !strings.Contains(line, "shared:") {
				t.Errorf("the view is not shared: %s", line)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d mounts at the view", n)
	}
	data := filepath.Join(dir, "data")
	writeFiles(t, data, map[string]string{"dev/etc/hostname": "dev\n", ".records/dev.json": "{}"})
	rt := New(Options{DataDir: data, View: view})
	if err := rt.viewAdd("dev"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(view, "dev/etc/hostname")); got != "dev\n" {
		t.Errorf("through the view: %q", got)
	}
	if _, err := os.Stat(filepath.Join(view, ".records")); err == nil {
		t.Error("the records are in the view")
	}
	if err := rt.viewRemove("dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(view, "dev")); err == nil {
		t.Error("a removed machine is left in the view")
	}
	if err := rt.viewRemove("dev"); err != nil {
		t.Errorf("removing what is gone: %v", err)
	}
}

func TestAnImageIsArm64OrAmd64(t *testing.T) {
	ref := api.ImageRef{Distro: "ubuntu", Release: "noble", Arch: "arm64", Variant: api.VariantDefault, Serial: "20261018_07:42"}
	for _, a := range []string{"arm64", "amd64"} {
		ref.Arch = a
		if err := checkImageRef(ref); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	for _, a := range []string{"riscv64", "x86_64", "i386", "", "../amd64"} {
		ref.Arch = a
		if err := checkImageRef(ref); err == nil {
			t.Errorf("%q taken", a)
		}
	}
}
