package agent

import (
	"encoding/json"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
)

type Debugger interface {
	DebugStatus() (api.DebugStatus, error)
	DebugReset(force bool) (api.DebugReset, error)
}

func (s *Server) handleDebug(method string, params json.RawMessage) (any, bool, error) {
	switch method {
	case api.MethodDebugStatus, api.MethodDebugReset:
	default:
		return nil, false, nil
	}
	d, ok := s.Sys.(Debugger)
	if !ok {
		return nil, true, failed(ErrNotImplemented)
	}
	if method == api.MethodDebugStatus {
		st, err := d.DebugStatus()
		if err != nil {
			return nil, true, failed(err)
		}
		return st, true, nil
	}
	p, err := jsonrpc.Bind[api.DebugResetParams](params)
	if err != nil {
		return nil, true, err
	}
	r, err := d.DebugReset(p.Force)
	if err != nil {
		return nil, true, err
	}
	return r, true, nil
}
