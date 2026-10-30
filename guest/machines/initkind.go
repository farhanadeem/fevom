package machines

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	api "fev/proto/agent"
)

const (
	InitSystemd  = "systemd"
	InitBusybox  = "busybox"
	InitOpenRC   = "openrc"
	InitRunit    = "runit"
	InitSysvinit = "sysvinit"
)

const MachineInit = "/sbin/init"

func resolveIn(root, name string) (string, error) {
	cur := "/"
	rest := strings.Split(strings.Trim(path.Clean("/"+name), "/"), "/")
	for hops := 0; len(rest) > 0; {
		part := rest[0]
		rest = rest[1:]
		if part == "" || part == "." {
			continue
		}
		next := path.Join(cur, part)
		st, err := os.Lstat(filepath.Join(root, filepath.FromSlash(next)))
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if hops++; hops > 40 {
			return "", fmt.Errorf("%s: too many levels of symbolic links", name)
		}
		target, err := os.Readlink(filepath.Join(root, filepath.FromSlash(next)))
		if err != nil {
			return "", err
		}
		if !strings.HasPrefix(target, "/") {
			target = path.Join(cur, target)
		}
		rest = append(strings.Split(strings.Trim(path.Clean(target), "/"), "/"), rest...)
		cur = "/"
	}
	return cur, nil
}

func InitKind(root string) (string, error) {
	p, err := resolveIn(root, MachineInit)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("the machine has no %s", MachineInit)
	}
	if err != nil {
		return "", err
	}
	switch base := path.Base(p); {
	case base == "systemd":
		return InitSystemd, nil
	case base == "busybox":
		return InitBusybox, nil
	case base == "openrc-init":
		return InitOpenRC, nil
	case strings.HasPrefix(base, "runit"):
		return InitRunit, nil
	default:
		return InitSysvinit, nil
	}
}

const (
	sigHUP = 1
	sigINT = 2
)

type ending struct {
	again bool
	state string
	last  string
}

func endOf(signaled bool, signal, code int, asked bool) ending {
	how := fmt.Sprintf("exit %d", code)
	if signaled {
		how = fmt.Sprintf("signal %d", signal)
	}
	switch {
	case asked:
		return ending{state: api.MachineStopped, last: api.ExitStopped}
	case signaled && signal == sigHUP:
		return ending{again: true, last: api.ExitReboot}
	case signaled && signal == sigINT:
		return ending{state: api.MachineStopped, last: api.ExitHalt}
	default:
		return ending{state: api.MachineFailed, last: how}
	}
}
