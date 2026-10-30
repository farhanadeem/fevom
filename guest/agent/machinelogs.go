package agent

import (
	"errors"
	"strings"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"

	"fev/guest/internal/logbuf"
)

type MachineLogs interface {
	MachineLogSubscribe(name string, runtime, backlog bool, deliver func(logbuf.Entry), backlogDone func()) (stop func(), err error)
}

type MachineShare interface {
	MachineShareBind(names []string) ([]api.MachineShareBound, error)
}

var ErrNoShare = errors.New("this VM has no file share")

func machineSource(src string) (name string, runtime, ok bool) {
	rest, found := strings.CutPrefix(src, api.LogMachinePrefix)
	if !found {
		return "", false, false
	}
	if n, r := strings.CutSuffix(rest, ":runtime"); r {
		return n, true, true
	}
	return rest, false, true
}

func (s *Server) bindShare(p api.ShareBindParams) (any, error) {
	engine := len(p.Targets) == 0
	var machines []string
	for _, t := range p.Targets {
		switch t.Context {
		case api.ExecEngine:
			engine = true
		case api.ExecMachine:
			if err := api.CheckMachineName(t.Machine); err != nil {
				return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
			}
			machines = append(machines, t.Machine)
		default:
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "a bind's target is the %s or a %s, not %q", api.ExecEngine, api.ExecMachine, t.Context)
		}
	}
	ms, hasMachines := s.Sys.(MachineShare)
	if len(machines) > 0 && !hasMachines {
		return nil, failed(ErrNotImplemented)
	}
	r := api.ShareBound{Bound: []string{}, Unbound: []string{}}
	if engine {
		bound, unbound, err := s.Sys.FileShareBind()
		if err != nil {
			st, serr := s.Sys.EngineStatus()
			if len(p.Targets) > 0 || errors.Is(err, ErrNoShare) || serr != nil || st.State == api.EngineReady || !hasMachines {
				return nil, failed(err)
			}
		}
		r.Bound, r.Unbound = append(r.Bound, bound...), append(r.Unbound, unbound...)
		if len(r.Bound) > 0 {
			s.Log.Logf(logbuf.Info, "agent: bound the Mac's %s in the running engine", strings.Join(r.Bound, ", "))
		}
	}
	if hasMachines && (len(p.Targets) == 0 || len(machines) > 0) {
		m, err := ms.MachineShareBind(machines)
		if err != nil {
			return nil, failed(err)
		}
		r.Machines = m
	}
	return r, nil
}
