package machines

import (
	"fmt"
	"path"
	"strings"

	api "fev/proto/agent"
	"fev/proto/bridge"
)

const HelperDir = "/opt/fev/bin"

const (
	SSHAuthSock = "/run/fev/ssh-auth.sock"
	DockerSock  = "/run/fev/docker.sock"
)

const defaultSessionPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

type User struct {
	Name        string
	UID, GID    uint32
	Groups      []uint32
	Home, Shell string
}

func lookupUser(root, name string) (*User, error) {
	if name == "" {
		return nil, fmt.Errorf("no user named, and the machine has no default user")
	}
	passwd, err := readIn(root, "/etc/passwd")
	if err != nil {
		return nil, fmt.Errorf("the machine's /etc/passwd: %w", err)
	}
	var u *User
	for _, e := range parsePasswd(string(passwd)) {
		if e.name == name {
			u = &User{Name: e.name, UID: uint32(e.uid), GID: uint32(e.gid), Home: e.home, Shell: e.shell}
			break
		}
	}
	if u == nil {
		return nil, fmt.Errorf("the machine has no user %q", name)
	}
	if u.Home == "" {
		u.Home = "/"
	}
	if u.Shell == "" {
		u.Shell = "/bin/sh"
	}
	u.Groups = []uint32{u.GID}
	if group, err := readIn(root, "/etc/group"); err == nil {
		for _, g := range parseGroup(string(group)) {
			if uint32(g.gid) == u.GID {
				continue
			}
			for _, m := range g.members {
				if m == name {
					u.Groups = append(u.Groups, uint32(g.gid))
					break
				}
			}
		}
	}
	return u, nil
}

func sessionPath(root string) string {
	p := defaultSessionPath
	if b, err := readIn(root, "/etc/environment"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
			if v, ok := strings.CutPrefix(line, "PATH="); ok {
				v = strings.Trim(v, `"'`)
				if v != "" && !strings.Contains(v, "$") {
					p = v
				}
			}
		}
	}
	return p + ":" + HelperDir
}

func sessionEnv(root string, u *User) []string {
	return []string{
		"HOME=" + u.Home,
		"USER=" + u.Name,
		"LOGNAME=" + u.Name,
		"SHELL=" + u.Shell,
		"PATH=" + sessionPath(root),
		"LANG=C.UTF-8",
		"BROWSER=" + HelperDir + "/open",
		"SSH_AUTH_SOCK=" + SSHAuthSock,
		"DOCKER_HOST=unix://" + DockerSock,
	}
}

func sessionArgv(u *User, p api.ExecStartParams) (argv []string, file string) {
	switch {
	case p.Copy != "":
		return p.Argv, ""
	case p.Subsystem == api.SubsystemSFTP:
		return []string{path.Base(bridge.HelperPath), "sftp"}, bridge.HelperPath
	case p.Login && p.Shell:
		return []string{"-" + path.Base(u.Shell), "-c", strings.Join(p.Argv, " ")}, u.Shell
	case p.Login:
		return []string{"-" + path.Base(u.Shell)}, u.Shell
	case p.Shell:
		return []string{u.Shell, "-c", strings.Join(p.Argv, " ")}, u.Shell
	default:
		return p.Argv, ""
	}
}

func UserIDs(root, name string) (uid, gid uint32, err error) {
	u, err := lookupUser(root, name)
	if err != nil {
		return 0, 0, err
	}
	return u.UID, u.GID, nil
}
