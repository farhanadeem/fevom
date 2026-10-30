package machines

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	api "fev/proto/agent"
)

const CloudSeedDir = "/var/lib/cloud/seed/nocloud-net"

const CloudConfigFile = "/etc/cloud/cloud.cfg.d/90-fev.cfg"

const cloudDisabled = "/etc/cloud/cloud-init.disabled"

const CloudOutputLog = "/var/log/cloud-init-output.log"

const cloudResult = "/run/cloud-init/result.json"

var cloudPrograms = []string{"/usr/bin/cloud-init", "/bin/cloud-init", "/usr/local/bin/cloud-init"}

const emptyCloudConfig = "#cloud-config\n{}\n"

const cloudNetworkConfig = "version: 1\nconfig:\n  - type: physical\n    name: eth0\n    subnets:\n      - type: dhcp\n        control: auto\n"

func NoCloudVariant(distro string) error {
	return fmt.Errorf("cloud-init not supported for '%s'", distro)
}

func CheckUserData(ref api.ImageRef, userData string) error {
	if userData == "" {
		return nil
	}
	if ref.Variant != api.VariantCloud {
		return NoCloudVariant(ref.Distro)
	}
	if len(userData) > api.MaxUserDataBytes {
		return fmt.Errorf("the user data is %d bytes; at most %d are taken", len(userData), api.MaxUserDataBytes)
	}
	return nil
}

type CloudSeed struct {
	Name, UUID string
	User       string
	UserData   string
	Password   bool
}

var doasPrograms = []string{"/usr/bin/doas", "/bin/doas", "/usr/local/bin/doas"}

var sshdPrograms = []string{"/usr/sbin/sshd", "/usr/bin/sshd", "/sbin/sshd"}

type cloudImage struct{ doas, sshd bool }

func cloudImageOf(has func(string) bool) cloudImage {
	anyOf := func(paths []string) bool {
		for _, p := range paths {
			if has(p) {
				return true
			}
		}
		return false
	}
	return cloudImage{doas: anyOf(doasPrograms), sshd: anyOf(sshdPrograms)}
}

func cloudFiles(s CloudSeed, has func(string) bool) ([]setupFile, error) {
	if err := checkUserName(s.User); err != nil {
		return nil, err
	}
	if s.Name == "" || s.UUID == "" {
		return nil, fmt.Errorf("a seed needs the machine's name and identity")
	}
	user := s.UserData
	if user == "" {
		user = emptyCloudConfig
	}
	meta := "instance-id: " + s.UUID + "\nlocal-hostname: " + s.Name + "\n"
	return []setupFile{
		{path: CloudSeedDir + "/meta-data", content: meta, mode: 0o644},
		{path: CloudSeedDir + "/user-data", content: user, mode: 0o600},
		{path: CloudSeedDir + "/vendor-data", content: emptyCloudConfig, mode: 0o644},
		{path: CloudSeedDir + "/network-config", content: cloudNetworkConfig, mode: 0o644},
		{path: CloudConfigFile, content: cloudConfig(s.User, s.Password, cloudImageOf(has)), mode: 0o644},
	}, nil
}

func cloudKeep(path string) bool {
	return path == CloudSeedDir+"/vendor-data" || path == CloudSeedDir+"/network-config"
}

func cloudConfig(user string, password bool, img cloudImage) string {
	var b strings.Builder
	b.WriteString("# fev: the machine's seed is " + CloudSeedDir + ", and its default user\n" +
		"# is fev's, made at its first boot before cloud-init ran.\n" +
		"datasource_list: [NoCloud, None]\n" +
		"# The host name is the machine's whole name, dots and all.\n" +
		"prefer_fqdn_over_hostname: true\n")
	if !img.sshd {
		b.WriteString("# No sshd in this image: fev serves the machine's SSH itself.\n" +
			"ssh_genkeytypes: []\n" +
			"ssh_pwauth: null\n" +
			"ssh:\n  emit_keys_to_console: false\n")
	}
	b.WriteString("system_info:\n  default_user:\n    name: " + strconv.Quote(user) + "\n    lock_passwd: " + strconv.FormatBool(!password) + "\n")
	if img.doas {
		b.WriteString("    doas:\n      - " + strconv.Quote("permit nopass "+user) + "\n")
	}
	return b.String()
}

func hasCloudInit(has func(string) bool) bool {
	for _, p := range cloudPrograms {
		if has(p) {
			return true
		}
	}
	return false
}

var sudoPrograms = []string{"/usr/bin/sudo", "/bin/sudo", "/usr/sbin/sudo", "/usr/local/bin/sudo"}

var apkPrograms = []string{"/sbin/apk", "/usr/sbin/apk", "/bin/apk", "/usr/bin/apk"}

type onlinePlan struct {
	install  []string
	packages []string
	cloud    bool
}

func (p onlinePlan) empty() bool { return len(p.install) == 0 && !p.cloud }

func planOnline(has func(string) bool) (onlinePlan, error) {
	var p onlinePlan
	p.cloud = hasCloudInit(has)
	anyOf := func(paths []string) bool {
		for _, s := range paths {
			if has(s) {
				return true
			}
		}
		return false
	}
	sudo := anyOf(sudoPrograms)
	if !anyOf(apkPrograms) {
		if sudo {
			return p, nil
		}
		return p, fmt.Errorf("the machine has no sudo, and fev knows no way to add it there")
	}
	var pkgs []string
	if !sudo {
		pkgs = append(pkgs, "sudo")
	}
	if !anyOf(updateCACertificates) && !anyOf(updateCATrust) {
		pkgs = append(pkgs, "ca-certificates")
	}
	if len(pkgs) > 0 {
		p.install = append([]string{"apk", "add", "--no-progress", "--quiet", "--update-cache"}, pkgs...)
		p.packages = pkgs
	}
	return p, nil
}

func hasNameserver(conf string) bool {
	for _, line := range strings.Split(conf, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			return true
		}
	}
	return false
}

func cloudStatus(out string) (status string, errs, recoverable []string) {
	section, level := "", ""
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "status:"); ok {
			status = strings.TrimSpace(v)
			section = ""
			continue
		}
		if line != "" && line[0] != ' ' && line[0] != '\t' && line[0] != '-' {
			if section == "recoverable" && isLevel(line) {
				level = strings.TrimSuffix(line, ":")
				continue
			}
			section, level = "", ""
			switch {
			case strings.HasPrefix(line, "errors:"):
				section = "errors"
			case strings.HasPrefix(line, "recoverable_errors:"):
				section = "recoverable"
			}
			continue
		}
		e, ok := strings.CutPrefix(trimmed, "- ")
		if !ok || e == "" {
			continue
		}
		switch {
		case section == "errors":
			errs = append(errs, e)
		case section == "recoverable" && level != "":
			recoverable = append(recoverable, level+": "+e)
		}
	}
	return status, errs, recoverable
}

func isLevel(line string) bool {
	word, ok := strings.CutSuffix(line, ":")
	if !ok || word == "" {
		return false
	}
	for _, r := range word {
		if (r < 'A' || r > 'Z') && r != '_' {
			return false
		}
	}
	return true
}

func cloudOff(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "boot_status_code:"); ok {
			return strings.HasPrefix(strings.TrimSpace(v), "disabled")
		}
	}
	return false
}

func resultErrors(b []byte) ([]string, error) {
	var r struct {
		V1 struct {
			Errors []string `json:"errors"`
		} `json:"v1"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return r.V1.Errors, nil
}

const cloudOutputLines = 20

func tailLines(s string, n int) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func cloudAccount(statusOut string, result, output []byte) api.CloudInit {
	status, errs, recoverable := cloudStatus(statusOut)
	if result != nil {
		if r, err := resultErrors(result); err == nil && len(r) > 0 {
			errs = r
		}
	}
	c := api.CloudInit{Status: status, Errors: errs, RecoverableErrors: recoverable}
	switch {
	case strings.HasPrefix(status, "error"):
		c.Status = "error"
	case strings.HasSuffix(status, "done"):
		c.Status = "done"
	case strings.HasPrefix(status, "disabled"):
		c.Status = "disabled"
	}
	if c.Status == "error" {
		c.OutputTail = tailLines(string(output), cloudOutputLines)
	}
	return c
}
