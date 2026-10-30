package machines

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "fev/proto/agent"
)

func loadTemplates(t *testing.T, build string) []api.ImageTemplate {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata/templates", build+".json"))
	if os.IsNotExist(err) {
		t.Skip("no testdata here: the test binary runs away from its source (it runs on the Mac with the rest)")
	}
	if err != nil {
		t.Fatal(err)
	}
	var ts []api.ImageTemplate
	if err := json.Unmarshal(b, &ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

func render(t *testing.T, ts []api.ImageTemplate, path string, ctx TemplateContext) string {
	t.Helper()
	for _, tpl := range TemplatesFor(ts, TriggerCreate) {
		if tpl.Path == path {
			out, err := RenderTemplate(tpl, TriggerCreate, ctx)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			return out
		}
	}
	t.Fatalf("no create template for %s", path)
	return ""
}

func TestTheContainersArchitectureIsTheImages(t *testing.T) {
	tpl := api.ImageTemplate{Path: "/etc/arch", Template: "{{ container.architecture }} {{ instance.architecture }}"}
	for arch, want := range map[string]string{"amd64": "x86_64 x86_64", "arm64": "aarch64 aarch64", "": "aarch64 aarch64"} {
		if got, err := RenderTemplate(tpl, TriggerCreate, TemplateContext{Name: "m", Arch: arch}); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", arch, got, err, want)
		}
	}
}

func TestTheFiveBuildsTemplatesRenderAsIncusRendersThem(t *testing.T) {
	ctx := TemplateContext{Name: "dev"}
	for _, build := range []string{
		"alpine_3.24_arm64_default_20261013_13_01", "archlinux_current_arm64_default_20261014_04_18",
		"debian_bookworm_arm64_default_20261014_05_24", "ubuntu_resolute_arm64_default_20261013_07_42",
	} {
		ts := loadTemplates(t, build)
		if got := render(t, ts, "/etc/hostname", ctx); got != "dev\n" {
			t.Errorf("%s: hostname %q", build, got)
		}
		hosts := render(t, ts, "/etc/hosts", ctx)
		if !strings.HasPrefix(hosts, "127.0.1.1\tdev\n") || strings.Contains(hosts, "{") {
			t.Errorf("%s: hosts %q", build, hosts)
		}
	}
	alp := loadTemplates(t, "alpine_3.24_arm64_default_20261013_13_01")
	inittab := render(t, alp, "/etc/inittab", ctx)
	if !strings.Contains(inittab, "::respawn:/sbin/getty 38400 console\n") || strings.Contains(inittab, "tty1") {
		t.Errorf("Alpine's inittab: %q", inittab)
	}
	for _, tpl := range alp {
		if tpl.Path == "/etc/inittab" && inittab != tpl.Template {
			t.Errorf("Alpine's inittab is not its template's text as it is")
		}
	}

	fed := loadTemplates(t, "fedora_44_arm64_cloud_20261013_20_33")
	if got := render(t, fed, "/var/lib/cloud/seed/nocloud-net/meta-data", ctx); got != "instance-id: dev\nlocal-hostname: dev\n\n" {
		t.Errorf("meta-data %q", got)
	}
	if got := render(t, fed, "/var/lib/cloud/seed/nocloud-net/user-data", ctx); got != "#cloud-config\n{}\n" {
		t.Errorf("user-data with none given %q", got)
	}
	given := TemplateContext{Name: "dev", Config: map[string]string{"cloud-init.user-data": "#cloud-config\nruncmd:\n  - [touch, /x]\n\r\n"}}
	if got := render(t, fed, "/var/lib/cloud/seed/nocloud-net/user-data", given); got != "#cloud-config\nruncmd:\n  - [touch, /x]\n" {
		t.Errorf("user-data given %q", got)
	}
	if got := render(t, fed, "/var/lib/cloud/seed/nocloud-net/vendor-data", given); got != "#cloud-config\n{}\n" {
		t.Errorf("vendor-data %q", got)
	}
	nc := render(t, fed, "/var/lib/cloud/seed/nocloud-net/network-config", ctx)
	for _, want := range []string{"version: 1\n", "    name: eth0\n", "      - type: dhcp\n", "      - type: dhcp6\n"} {
		if !strings.Contains(nc, want) {
			t.Errorf("network-config has no %q:\n%s", want, nc)
		}
	}
	if strings.Contains(nc, "enp5s0") || strings.Contains(nc, "{%") {
		t.Errorf("network-config %q", nc)
	}
}

func TestATemplateReachesNoFile(t *testing.T) {
	for _, text := range []string{`{% include "/etc/shadow" %}`, `{% extends "x" %}`, `{% import "x" y %}`, `{% ssi "/etc/shadow" %}`} {
		if out, err := RenderTemplate(api.ImageTemplate{Path: "/x", Template: text}, TriggerCreate, TemplateContext{Name: "m"}); err == nil {
			t.Errorf("%s rendered: %q", text, out)
		}
	}
	ts := []api.ImageTemplate{
		{Path: "/b", When: []string{"copy"}}, {Path: "/a", When: []string{"create", "copy"}}, {Path: "/c", When: []string{"start"}},
	}
	got := TemplatesFor(ts, TriggerCreate)
	if len(got) != 1 || got[0].Path != "/a" {
		t.Errorf("create's: %+v", got)
	}
	got = TemplatesFor(ts, TriggerCopy)
	if len(got) != 2 || got[0].Path != "/a" || got[1].Path != "/b" {
		t.Errorf("copy's, in their order: %+v", got)
	}
	out, err := RenderTemplate(api.ImageTemplate{Path: "/x", Template: `{{ config_get("k", "") }}`}, TriggerCreate, TemplateContext{Config: map[string]string{"k": "<a & b>"}})
	if err != nil || out != "<a & b>" {
		t.Errorf("escaped: %q %v", out, err)
	}
}
