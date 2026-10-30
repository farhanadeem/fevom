package machines

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	api "fev/proto/agent"
)

func runSetup(u api.MachineUser) error {
	passwd, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return err
	}
	group, err := os.ReadFile("/etc/group")
	if err != nil {
		return err
	}
	sudoers := map[string]string{}
	if entries, err := os.ReadDir("/etc/sudoers.d"); err == nil {
		for _, e := range entries {
			if e.Type().IsRegular() {
				if b, err := os.ReadFile(filepath.Join("/etc/sudoers.d", e.Name())); err == nil {
					sudoers[e.Name()] = string(b)
				}
			}
		}
	}
	has := func(p string) bool { _, err := os.Stat(p); return err == nil }
	plan, err := planSetup(u, string(passwd), string(group), sudoers, has)
	if err != nil {
		return err
	}
	os.Setenv("PATH", machinePath)
	for _, c := range plan.cmds {
		cmd := exec.Command(c.argv[0], c.argv[1:]...)
		cmd.Env = []string{"PATH=" + machinePath, "LANG=C"}
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if c.stdin != "" {
			cmd.Stdin = strings.NewReader(c.stdin)
		}
		if err := cmd.Run(); err != nil {
			if c.okIfGone != "" && !userExists(c.okIfGone) || c.okIfGroup != "" && groupExists(c.okIfGroup) {
				continue
			}
			return fmt.Errorf("%s: %w", strings.Join(c.argv, " "), err)
		}
	}
	for _, p := range plan.remove {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	plan.write = append(plan.write, setupFile{path: ProfileFile, content: profileScript, mode: 0o644})
	if b, err := os.ReadFile(updatedbConf); err == nil {
		if conf, changed := pruneUpdatedb(string(b)); changed {
			st, _ := os.Stat(updatedbConf)
			mode := uint32(0o644)
			if st != nil {
				mode = uint32(st.Mode().Perm())
			}
			plan.write = append(plan.write, setupFile{path: updatedbConf, content: conf, mode: mode})
		}
	}
	if b, err := os.ReadFile(dhcpcdConf); err == nil {
		if conf, changed := dhcpcdNoARP(string(b)); changed {
			mode := uint32(0o644)
			if st, err := os.Stat(dhcpcdConf); err == nil {
				mode = uint32(st.Mode().Perm())
			}
			plan.write = append(plan.write, setupFile{path: dhcpcdConf, content: conf, mode: mode})
		}
	}
	if b, err := os.ReadFile(inittabFile); err == nil {
		if tab, changed := inittabWithoutAbsentTerminals(string(b), func(dev string) bool { return has("/dev/" + dev) }); changed {
			mode := uint32(0o644)
			if st, err := os.Stat(inittabFile); err == nil {
				mode = uint32(st.Mode().Perm())
			}
			plan.write = append(plan.write, setupFile{path: inittabFile, content: tab, mode: mode})
		}
	}
	for _, f := range plan.write {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o750); err != nil {
			return err
		}
		tmp := f.path + ".fev-new"
		if err := os.WriteFile(tmp, []byte(f.content), os.FileMode(f.mode)); err != nil {
			return err
		}
		if err := os.Chmod(tmp, os.FileMode(f.mode)); err != nil {
			return err
		}
		if err := os.Rename(tmp, f.path); err != nil {
			return err
		}
	}
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return err
	}
	for _, e := range parsePasswd(string(b)) {
		if e.name == u.Name {
			if e.uid != u.UID {
				return fmt.Errorf("%s was made with uid %d, not %d", u.Name, e.uid, u.UID)
			}
			return nil
		}
	}
	return fmt.Errorf("%s is not in /etc/passwd after its setup", u.Name)
}

func userExists(name string) bool {
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return true
	}
	for _, e := range parsePasswd(string(b)) {
		if e.name == name {
			return true
		}
	}
	return false
}

func groupExists(name string) bool {
	b, err := os.ReadFile("/etc/group")
	if err != nil {
		return false
	}
	for _, g := range parseGroup(string(b)) {
		if g.name == name {
			return true
		}
	}
	return false
}
