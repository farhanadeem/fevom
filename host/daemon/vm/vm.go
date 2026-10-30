package vm

import (
	"net"
	"strings"
	"time"

	"fev/proto/vmmctl"
)

type Spec struct {
	BootID    string
	CPUs      int
	MemMiB    int
	Share     bool
	Verbose   bool
	EngineOff bool
	NodeName  string
	K8s       bool
}

func Cmdline(base string, verbose bool) string {
	if !verbose {
		return base
	}
	words := strings.Fields(base)
	keep := words[:0]
	for _, w := range words {
		if w != "quiet" {
			keep = append(keep, w)
		}
	}
	return strings.Join(keep, " ")
}

type Mark struct {
	Name    string `json:"mark"`
	NS      uint64 `json:"ns"`
	Build   string `json:"build,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	Path    string `json:"path,omitempty"`
}

type Exit struct {
	Code   int
	Signal string
	At     time.Time
}

type Events interface {
	Mark(Mark)
	VMMLine(string)
	ConsoleLine(string)
	AgentConn(net.Conn)
	Exited(Exit)
}

type Instance interface {
	PID() int
	VsockPath(port uint32) string
	Stop()
	Kill()
	Control() *vmmctl.Client
}

type Launcher interface {
	Start(spec Spec, ev Events) (Instance, error)
	Fake() bool
}
