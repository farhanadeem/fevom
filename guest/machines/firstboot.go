package machines

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	api "fev/proto/agent"
)

const SudoersFile = "/etc/sudoers.d/fev"

const lingerDir = "/var/lib/systemd/linger"

type setupPlan struct {
	cmds   []setupCmd
	remove []string
	write  []setupFile
}

type setupCmd struct {
	argv      []string
	stdin     string
	okIfGone  string
	okIfGroup string
}

type setupFile struct {
	path    string
	content string
	mode    uint32
}

type passwdEntry struct {
	name        string
	uid, gid    int
	home, shell string
}

func parsePasswd(text string) []passwdEntry {
	var out []passwdEntry
	for _, line := range strings.Split(text, "\n") {
		f := strings.Split(line, ":")
		if len(f) < 7 || strings.HasPrefix(line, "#") {
			continue
		}
		uid, err1 := strconv.Atoi(f[2])
		gid, err2 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, passwdEntry{name: f[0], uid: uid, gid: gid, home: f[5], shell: f[6]})
	}
	return out
}

type groupEntry struct {
	name    string
	gid     int
	members []string
}

func parseGroup(text string) []groupEntry {
	var out []groupEntry
	for _, line := range strings.Split(text, "\n") {
		f := strings.Split(line, ":")
		if len(f) < 4 || strings.HasPrefix(line, "#") {
			continue
		}
		gid, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		var members []string
		for _, m := range strings.Split(f[3], ",") {
			if m != "" {
				members = append(members, m)
			}
		}
		out = append(out, groupEntry{name: f[0], gid: gid, members: members})
	}
	return out
}

func imageUser(e passwdEntry) bool { return e.uid >= 1000 && e.uid < 60000 }

func planSetup(u api.MachineUser, passwd, group string, sudoers map[string]string, has func(string) bool) (setupPlan, error) {
	var p setupPlan
	if err := checkUserName(u.Name); err != nil {
		return p, err
	}
	if u.UID <= 0 || u.UID >= 60000 {
		return p, fmt.Errorf("uid %d is not one a machine's default user can have", u.UID)
	}
	shadow := has("/usr/sbin/useradd") || has("/usr/bin/useradd") || has("/sbin/useradd")
	if !shadow && !has("/usr/sbin/adduser") && !has("/bin/adduser") && !has("/sbin/adduser") {
		return p, fmt.Errorf("the machine has neither useradd nor adduser")
	}
	users := parsePasswd(passwd)
	groups := parseGroup(group)
	groupNamed := func(name string) *groupEntry {
		for i := range groups {
			if groups[i].name == name {
				return &groups[i]
			}
		}
		return nil
	}

	var removed []string
	for _, e := range users {
		if e.name == u.Name || e.uid == u.UID || imageUser(e) {
			removed = append(removed, e.name)
			if shadow {
				p.cmds = append(p.cmds, setupCmd{argv: []string{"userdel", "-r", e.name}, okIfGone: e.name})
			} else {
				p.cmds = append(p.cmds, setupCmd{argv: []string{"deluser", "--remove-home", e.name}, okIfGone: e.name})
			}
		}
	}
	names := make([]string, 0, len(sudoers))
	for name := range sudoers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if rulesOnlyFor(sudoers[name], removed) {
			p.remove = append(p.remove, "/etc/sudoers.d/"+name)
		}
	}

	kept := groups[:0:0]
	for _, g := range groups {
		gone := false
		for _, r := range removed {
			gone = gone || g.name == r && len(g.members) == 0
		}
		if !gone {
			kept = append(kept, g)
		}
	}
	groups = kept

	var extra []string
	switch {
	case groupNamed("sudo") != nil:
		extra = append(extra, "sudo")
	case groupNamed("wheel") != nil:
		extra = append(extra, "wheel")
	}
	for _, g := range []string{"adm", "video", "staff"} {
		if groupNamed(g) != nil {
			extra = append(extra, g)
		}
	}
	sort.Strings(extra)
	shell := "/bin/sh"
	if has("/bin/bash") || has("/usr/bin/bash") {
		shell = "/bin/bash"
	}
	uid := strconv.Itoa(u.UID)
	own := groupNamed(u.Name)
	gidTaken := false
	for _, g := range groups {
		gidTaken = gidTaken || g.gid == u.UID && g.name != u.Name
	}
	home := "/home/" + u.Name
	password := "!"
	if u.PasswordHash != "" {
		password = u.PasswordHash
	}
	if shadow {
		if own == nil {
			args := []string{"groupadd"}
			if !gidTaken {
				args = append(args, "-g", uid)
			}
			p.cmds = append(p.cmds, setupCmd{argv: append(args, u.Name), okIfGroup: u.Name})
		}
		args := []string{"useradd", "-m", "-d", home, "-u", uid, "-g", u.Name, "-s", shell, "-p", password}
		if len(extra) > 0 {
			args = append(args, "-G", strings.Join(extra, ","))
		}
		p.cmds = append(p.cmds, setupCmd{argv: append(args, u.Name)})
	} else {
		if own == nil {
			args := []string{"addgroup"}
			if !gidTaken {
				args = append(args, "-g", uid)
			}
			p.cmds = append(p.cmds, setupCmd{argv: append(args, u.Name), okIfGroup: u.Name})
		}
		p.cmds = append(p.cmds, setupCmd{argv: []string{"adduser", "-D", "-u", uid, "-G", u.Name, "-s", shell, "-h", home, u.Name}})
		for _, g := range extra {
			p.cmds = append(p.cmds, setupCmd{argv: []string{"addgroup", u.Name, g}})
		}
		if u.PasswordHash != "" {
			p.cmds = append(p.cmds, setupCmd{argv: []string{"chpasswd", "-e"}, stdin: u.Name + ":" + u.PasswordHash + "\n"})
		}
	}
	p.write = append(p.write, setupFile{path: SudoersFile, content: u.Name + " ALL=(ALL) NOPASSWD:ALL\n", mode: 0o440})
	if has("/usr/lib/systemd/systemd") || has("/lib/systemd/systemd") {
		p.write = append(p.write, setupFile{path: lingerDir + "/" + u.Name, mode: 0o644})
	}
	return p, nil
}

func rulesOnlyFor(text string, users []string) bool {
	rules := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rules++
		subject := strings.Fields(line)[0]
		found := false
		for _, u := range users {
			found = found || subject == u
		}
		if !found {
			return false
		}
	}
	return rules > 0
}

func checkUserName(name string) error {
	if name == "" || len(name) > 32 {
		return fmt.Errorf("user name %q: empty or longer than 32", name)
	}
	for i, c := range name {
		ok := c >= 'a' && c <= 'z' || c == '_' || i > 0 && (c >= '0' && c <= '9' || c == '-' || c == '.')
		if !ok {
			return fmt.Errorf("user name %q: a machine's user name is a lower-case letter or _, then letters, digits, _, - and .", name)
		}
	}
	if name == "root" {
		return fmt.Errorf("user name %q is the machine's own", name)
	}
	return nil
}

const ProfileFile = "/etc/profile.d/fev.sh"

const profileScript = "# fev: its helpers (open, code, mac and the rest), last on PATH, so that\n" +
	"# a program of the distribution's own of the same name wins.\n" +
	"case \":$PATH:\" in\n*:" + HelperDir + ":*) ;;\n*) PATH=\"$PATH:" + HelperDir + "\" ;;\nesac\n"

const updatedbConf = "/etc/updatedb.conf"

var prunedPaths = []string{"/mnt/mac", "/Users", "/Volumes", "/private", "/Applications", "/Library", "/mnt/machines"}

func pruneUpdatedb(conf string) (string, bool) {
	lines := strings.Split(conf, "\n")
	found := false
	changed := false
	for i, line := range lines {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "PRUNEPATHS")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		val, ok := strings.CutPrefix(rest, "=")
		if !ok {
			continue
		}
		found = true
		quote := ""
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			quote, val = val[:1], val[1:len(val)-1]
		}
		have := map[string]bool{}
		paths := strings.Fields(val)
		for _, p := range paths {
			have[p] = true
		}
		for _, p := range prunedPaths {
			if !have[p] {
				paths = append(paths, p)
				changed = true
			}
		}
		if quote == "" {
			quote = "\""
		}
		lines[i] = "PRUNEPATHS=" + quote + strings.Join(paths, " ") + quote
	}
	if !found {
		lines = append([]string{"PRUNEPATHS=\"" + strings.Join(prunedPaths, " ") + "\""}, lines...)
		changed = true
	}
	return strings.Join(lines, "\n"), changed
}

const dhcpcdConf = "/etc/dhcpcd.conf"

func dhcpcdNoARP(conf string) (string, bool) {
	for _, line := range strings.Split(conf, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == "noarp" {
			return conf, false
		}
	}
	if conf != "" && !strings.HasSuffix(conf, "\n") {
		conf += "\n"
	}
	return conf + "\n# fev: the machines' bridge gives this machine an address no other has,\n# so it is taken without the ARP probe's wait of about 5 s at each start.\nnoarp\n", true
}

const inittabFile = "/etc/inittab"

func inittabWithoutAbsentTerminals(tab string, has func(dev string) bool) (string, bool) {
	lines := strings.Split(tab, "\n")
	changed := false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		f := strings.SplitN(t, ":", 4)
		if len(f) != 4 || f[2] != "respawn" {
			continue
		}
		words := strings.Fields(f[3])
		if len(words) == 0 || !strings.HasSuffix(path.Base(words[0]), "getty") {
			continue
		}
		dev := ""
		for _, w := range words[1:] {
			if w == "console" || strings.HasPrefix(w, "tty") || strings.HasPrefix(w, "hvc") {
				dev = w
				break
			}
		}
		if dev == "" || dev == "console" || has(dev) {
			continue
		}
		lines[i] = "#fev: this machine has no /dev/" + dev + ": " + line
		changed = true
	}
	return strings.Join(lines, "\n"), changed
}

func fstabTmpfsTmp(fstab string) bool {
	for _, line := range strings.Split(fstab, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && !strings.HasPrefix(f[0], "#") && f[1] == "/tmp" && f[2] == "tmpfs" {
			return true
		}
	}
	return false
}
