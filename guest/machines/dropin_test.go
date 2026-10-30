package machines

import (
	"os"
	"path/filepath"
	"testing"
)

const netplanUnit = `[Unit]
Description=Netplan Backend Configuration
DefaultDependencies=no
Wants=systemd-udevd.service

[Service]
Type=oneshot
RemainAfterExit=true
Environment=NETPLAN_PARSER_IGNORE_ERRORS=1
ExecStart=/usr/libexec/netplan/configure
# 'udevadm trigger' is needed to rename interfaces at boot time:
ExecStartPost=udevadm control --reload
ExecStartPost=udevadm trigger --action=add --subsystem-match=net

[Install]
WantedBy=sysinit.target
`

func TestNetplansUdevadmCallsAreMadeNonFatal(t *testing.T) {
	d := dropIns[0]
	want := `# Written by fev at the machine's start, in its own /run.
# /sys is read-only in a machine and udevd does not run, so udevadm's
# calls cannot succeed; they are made non-fatal. Nothing else changes.
[Service]
ExecStartPost=
ExecStartPost=-udevadm control --reload
ExecStartPost=-udevadm trigger --action=add --subsystem-match=net
`
	if got := d.render(netplanUnit); got != want {
		t.Errorf("drop-in:\n%s\nwant:\n%s", got, want)
	}
	for name, unit := range map[string]string{
		"already non-fatal":  "[Service]\nExecStartPost=-udevadm control --reload\n",
		"no udevadm":         "[Service]\nExecStart=/bin/true\nExecStartPost=/bin/echo udevadm\n",
		"in another section": "[Unit]\nExecStartPost=udevadm control --reload\n",
		"another program":    "[Service]\nExecStartPost=/usr/bin/udevadmin\n",
	} {
		if got := d.render(unit); got != "" {
			t.Errorf("%s: a drop-in:\n%s", name, got)
		}
	}
	mixed := "[Service]\nExecStartPost=/bin/echo one\nExecStartPost=@/usr/bin/udevadm udevadm settle\nExecStartPost=\nExecStartPost=/usr/bin/udevadm trigger\n"
	wantMixed := "[Service]\nExecStartPost=\nExecStartPost=-/usr/bin/udevadm trigger\n"
	if got := d.render(mixed); len(got) < len(wantMixed) || got[len(got)-len(wantMixed):] != wantMixed {
		t.Errorf("a list reset in the unit itself:\n%s", got)
	}
}

func TestTheDropInIsWrittenOnlyForAUnitTheMachineHas(t *testing.T) {
	root, run := t.TempDir(), t.TempDir()
	if err := writeDropIns(root, run); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(run); len(entries) != 0 {
		t.Errorf("a drop-in for a unit the machine does not have: %v", entries)
	}
	os.MkdirAll(filepath.Join(root, "usr/lib/systemd/system"), 0o755)
	os.Symlink("usr/lib", filepath.Join(root, "lib"))
	os.WriteFile(filepath.Join(root, "usr/lib/systemd/system/netplan-configure.service"), []byte(netplanUnit), 0o644)
	if err := writeDropIns(root, run); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(run, "systemd/system/netplan-configure.service.d", DropInName))
	if err != nil || len(b) == 0 {
		t.Fatalf("no drop-in: %v", err)
	}
	os.MkdirAll(filepath.Join(root, "etc/systemd/system"), 0o755)
	os.WriteFile(filepath.Join(root, "etc/systemd/system/netplan-configure.service"), []byte("[Service]\nExecStart=/bin/true\n"), 0o644)
	run2 := t.TempDir()
	if err := writeDropIns(root, run2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(run2, "systemd/system/netplan-configure.service.d", DropInName)); err == nil {
		t.Error("a drop-in against the administrator's own unit")
	}
}
