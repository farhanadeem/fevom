package machines

import (
	"path"
	"strings"

	api "fev/proto/agent"
)

func agentPathInMachine(p string) bool {
	if p == "" || !path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	for _, s := range api.MachineSharePaths {
		if strings.HasPrefix(p, s.Target+"/") {
			return true
		}
	}
	return false
}
