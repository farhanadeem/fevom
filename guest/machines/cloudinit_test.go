package machines

import (
	"strings"
	"testing"

	api "fev/proto/agent"
)

func TestUserDataNeedsTheCloudVariant(t *testing.T) {
	arch := api.ImageRef{Distro: "archlinux", Release: "current", Arch: "arm64", Variant: api.VariantDefault}
	err := CheckUserData(arch, "#cloud-config\n{}\n")
	if err == nil || err.Error() != "cloud-init not supported for 'archlinux'" {
		t.Fatalf("arch with user data: %v", err)
	}
	if err := CheckUserData(arch, ""); err != nil {
		t.Fatalf("arch without: %v", err)
	}
	ubuntu := api.ImageRef{Distro: "ubuntu", Release: "resolute", Arch: "arm64", Variant: api.VariantCloud}
	if err := CheckUserData(ubuntu, "#cloud-config\nruncmd: [true]\n"); err != nil {
		t.Fatalf("ubuntu cloud: %v", err)
	}
	if err := CheckUserData(ubuntu, strings.Repeat("x", api.MaxUserDataBytes+1)); err == nil {
		t.Fatal("more than MaxUserDataBytes was taken")
	}
}

func TestTheSeedAndFevomsCloudConfig(t *testing.T) {
	ubuntu := func(p string) bool { return p == "/usr/sbin/sshd" }
	files, err := cloudFiles(CloudSeed{Name: "dev", UUID: "8f14e45f-ceea-467a-9575-0c7a3c5d2b11", User: "ada", UserData: "#cloud-config\nruncmd: [[sh, -c, 'exit 3']]\n"}, ubuntu)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]setupFile{}
	for _, f := range files {
		got[f.path] = f
	}
	want := map[string]string{
		"/var/lib/cloud/seed/nocloud-net/meta-data":      "instance-id: 8f14e45f-ceea-467a-9575-0c7a3c5d2b11\nlocal-hostname: dev\n",
		"/var/lib/cloud/seed/nocloud-net/user-data":      "#cloud-config\nruncmd: [[sh, -c, 'exit 3']]\n",
		"/var/lib/cloud/seed/nocloud-net/vendor-data":    "#cloud-config\n{}\n",
		"/var/lib/cloud/seed/nocloud-net/network-config": "version: 1\nconfig:\n  - type: physical\n    name: eth0\n    subnets:\n      - type: dhcp\n        control: auto\n",
	}
	for p, content := range want {
		if got[p].content != content {
			t.Errorf("%s:\n%q\nwant\n%q", p, got[p].content, content)
		}
	}
	if got["/var/lib/cloud/seed/nocloud-net/user-data"].mode != 0o600 {
		t.Errorf("user-data's mode %o", got["/var/lib/cloud/seed/nocloud-net/user-data"].mode)
	}
	cfg := got[CloudConfigFile].content
	for _, line := range []string{"datasource_list: [NoCloud, None]\n", "  default_user:\n    name: \"ada\"\n    lock_passwd: true\n"} {
		if !strings.Contains(cfg, line) {
			t.Errorf("90-fev.cfg lacks %q:\n%s", line, cfg)
		}
	}
	files, _ = cloudFiles(CloudSeed{Name: "dev", UUID: "u", User: "ada", Password: true}, ubuntu)
	for _, f := range files {
		if f.path == CloudConfigFile && !strings.Contains(f.content, "    lock_passwd: false\n") {
			t.Errorf("with a password, 90-fev.cfg locks it:\n%s", f.content)
		}
	}
	if strings.Contains(cfg, "network") {
		t.Errorf("90-fev.cfg says something of the network, which the cloud images leave to cloud-init:\n%s", cfg)
	}
	files, _ = cloudFiles(CloudSeed{Name: "dev", UUID: "u", User: "ada"}, ubuntu)
	for _, f := range files {
		if strings.HasSuffix(f.path, "/user-data") && f.content != "#cloud-config\n{}\n" {
			t.Errorf("no user data: %q", f.content)
		}
	}
	if _, err := cloudFiles(CloudSeed{Name: "dev", UUID: "u", User: "Bad User"}, ubuntu); err == nil {
		t.Error("a user name the tools refuse was taken")
	}
}

func TestFevomsCloudConfigFollowsTheImage(t *testing.T) {
	cfg := func(has func(string) bool) string {
		files, err := cloudFiles(CloudSeed{Name: "a.b", UUID: "u", User: "ada"}, has)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if f.path == CloudConfigFile {
				return f.content
			}
		}
		t.Fatal("no 90-fev.cfg")
		return ""
	}
	alpine := cfg(func(p string) bool { return p == "/usr/bin/doas" })
	for _, line := range []string{
		"prefer_fqdn_over_hostname: true\n",
		"ssh_genkeytypes: []\n", "ssh_pwauth: null\n", "ssh:\n  emit_keys_to_console: false\n",
		"    name: \"ada\"\n    lock_passwd: true\n    doas:\n      - \"permit nopass ada\"\n",
	} {
		if !strings.Contains(alpine, line) {
			t.Errorf("Alpine's 90-fev.cfg lacks %q:\n%s", line, alpine)
		}
	}
	if strings.Contains(alpine, "alpine") {
		t.Errorf("Alpine's 90-fev.cfg names the image's user:\n%s", alpine)
	}
	ubuntu := cfg(func(p string) bool { return p == "/usr/sbin/sshd" })
	if !strings.Contains(ubuntu, "prefer_fqdn_over_hostname: true\n") {
		t.Errorf("Ubuntu's 90-fev.cfg keeps a dotted name's first label:\n%s", ubuntu)
	}
	for _, not := range []string{"ssh_genkeytypes", "ssh_pwauth", "emit_keys_to_console", "doas"} {
		if strings.Contains(ubuntu, not) {
			t.Errorf("an image with sshd and no doas is given %s:\n%s", not, ubuntu)
		}
	}
}

func TestTheOnlineStepAddsSudoAndWaitsForCloudInit(t *testing.T) {
	alpine := map[string]bool{"/sbin/apk": true, "/usr/bin/doas": true}
	p, err := planOnline(func(s string) bool { return alpine[s] })
	if err != nil || strings.Join(p.install, " ") != "apk add --no-progress --quiet --update-cache sudo ca-certificates" || p.cloud {
		t.Fatalf("alpine: %+v %v", p, err)
	}
	alpine["/usr/sbin/update-ca-certificates"] = true
	if p, _ := planOnline(func(s string) bool { return alpine[s] }); strings.Join(p.install, " ") != "apk add --no-progress --quiet --update-cache sudo" {
		t.Fatalf("alpine with the tool: %+v", p)
	}
	alpine["/usr/bin/sudo"] = true
	if p, _ := planOnline(func(s string) bool { return alpine[s] }); len(p.install) != 0 {
		t.Fatalf("alpine with both: %+v", p)
	}
	delete(alpine, "/usr/bin/sudo")
	alpine["/usr/bin/cloud-init"] = true
	if p, _ := planOnline(func(s string) bool { return alpine[s] }); !p.cloud {
		t.Fatalf("alpine cloud: %+v", p)
	}
	ubuntu := map[string]bool{"/usr/bin/sudo": true, "/usr/bin/cloud-init": true}
	p, err = planOnline(func(s string) bool { return ubuntu[s] })
	if err != nil || len(p.install) > 0 || !p.cloud {
		t.Fatalf("ubuntu cloud: %+v %v", p, err)
	}
	delete(ubuntu, "/usr/bin/cloud-init")
	if p, _ := planOnline(func(s string) bool { return ubuntu[s] }); !p.empty() {
		t.Fatalf("ubuntu: %+v", p)
	}
	if _, err := planOnline(func(string) bool { return false }); err == nil {
		t.Fatal("no sudo and no way to add it was not said")
	}
}

func TestCloudInitsAccountOfARun(t *testing.T) {
	long := "status: error\nextended_status: error - done\nboot_status_code: enabled-by-generator\n" +
		"last_update: Wed, 14 Oct 2026 13:12:01 +0000\ndetail: DataSourceNoCloud [seed=/var/lib/cloud/seed/nocloud-net]\n" +
		"errors:\n\t- ('scripts_user', RuntimeError('Runparts: 1 failures (part-001) in 1 attempted commands'))\n" +
		"recoverable_errors:\nWARNING:\n\t- Failed to run module scripts_user (scripts in /var/lib/cloud/instance/scripts)\n"
	var log strings.Builder
	for i := range 30 {
		log.WriteString("line " + string(rune('a'+i%26)) + "\n")
	}
	log.WriteString("Cloud-init v. 25.2 finished at Wed, 14 Oct 2026 13:12:01 +0000.\n")
	c := cloudAccount(long, nil, []byte(log.String()))
	if c.Status != "error" || len(c.Errors) != 1 || !strings.Contains(c.Errors[0], "Runparts: 1 failures") {
		t.Fatalf("%+v", c)
	}
	if len(c.OutputTail) != cloudOutputLines || !strings.HasPrefix(c.OutputTail[cloudOutputLines-1], "Cloud-init v. 25.2 finished") {
		t.Fatalf("tail %q", c.OutputTail)
	}
	result := []byte(`{"v1": {"datasource": "DataSourceNoCloud", "errors": ["('scripts_user', RuntimeError('x'))"], "recoverable_errors": {}}}`)
	if c := cloudAccount(long, result, nil); len(c.Errors) != 1 || c.Errors[0] != "('scripts_user', RuntimeError('x'))" {
		t.Fatalf("from result.json: %+v", c)
	}
	done := cloudAccount("status: done\nextended_status: done\nerrors: []\nrecoverable_errors: {}\n", nil, []byte("x\n"))
	if done.Status != "done" || len(done.Errors) != 0 || done.OutputTail != nil || done.RecoverableErrors != nil {
		t.Fatalf("done: %+v", done)
	}
	degraded := "status: done\nextended_status: degraded done\nboot_status_code: enabled-by-sysvinit\n" +
		"last_update: Thu, 01 Jan 1970 00:00:04 +0000\ndetail: DataSourceNoCloud [seed=/var/lib/cloud/seed/nocloud-net]\n" +
		"errors: []\nrecoverable_errors:\nERROR:\n\t- Invalid doas rule 'permit nopass alpine' for user 'ada', not writing any doas rules for user!\n" +
		"WARNING:\n\t- Failed generating key type rsa to file /etc/ssh/ssh_host_rsa_key\n" +
		"\t- Unable to activate module keys_to_console, helper tool not found at /usr/lib/cloud-init/write-ssh-key-fingerprints\n"
	d := cloudAccount(degraded, []byte(`{"v1": {"datasource": "DataSourceNoCloud", "errors": []}}`), nil)
	want := []string{
		"ERROR: Invalid doas rule 'permit nopass alpine' for user 'ada', not writing any doas rules for user!",
		"WARNING: Failed generating key type rsa to file /etc/ssh/ssh_host_rsa_key",
		"WARNING: Unable to activate module keys_to_console, helper tool not found at /usr/lib/cloud-init/write-ssh-key-fingerprints",
	}
	if d.Status != "done" || len(d.Errors) != 0 || strings.Join(d.RecoverableErrors, "\n") != strings.Join(want, "\n") {
		t.Fatalf("degraded done: %+v", d)
	}
	if c := cloudAccount(long, nil, nil); len(c.RecoverableErrors) != 1 || !strings.HasPrefix(c.RecoverableErrors[0], "WARNING: Failed to run module scripts_user") {
		t.Fatalf("error's recoverable: %+v", c)
	}
	if c := cloudAccount("status: disabled\n", nil, nil); c.Status != "disabled" {
		t.Fatalf("disabled: %+v", c)
	}
}

func TestATurnedOffCloudInitIsNotWaitedFor(t *testing.T) {
	alpine := "status: not started\nextended_status: not started\nboot_status_code: disabled-by-marker-file\ndetail: Cloud-init disabled by /etc/cloud/cloud-init.disabled\n"
	if !cloudOff(alpine) {
		t.Fatal("a marker file's off")
	}
	if cloudOff("status: running\nboot_status_code: enabled-by-sysvinit\n") || cloudOff("status: running\n") {
		t.Fatal("an enabled one is off")
	}
}

func TestAResolverNamesAServer(t *testing.T) {
	for conf, want := range map[string]bool{
		"":                        false,
		"# empty\nsearch .\n":     false,
		"nameserver 198.19.0.1\n": true,
		"  nameserver\t10.0.0.1":  true,
		"nameservers 1.1.1.1\n":   false,
	} {
		if hasNameserver(conf) != want {
			t.Errorf("%q: %v", conf, !want)
		}
	}
}
