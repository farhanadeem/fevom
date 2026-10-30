package agent

import (
	"encoding/json"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
)

type Emulation interface {
	EmulationStatus() api.EmulationStatus
	EmulationSet(x86 bool) (api.EmulationStatus, error)
}

func (s *Server) handleEmulation(method string, params json.RawMessage) (any, bool, error) {
	switch method {
	case api.MethodEmulationStatus, api.MethodEmulationSet:
	default:
		return nil, false, nil
	}
	e, ok := s.Sys.(Emulation)
	if !ok {
		return nil, true, failed(ErrNotImplemented)
	}
	if method == api.MethodEmulationStatus {
		return e.EmulationStatus(), true, nil
	}
	p, err := jsonrpc.Bind[api.EmulationSetParams](params)
	if err != nil {
		return nil, true, err
	}
	st, err := e.EmulationSet(p.X86)
	if err != nil {
		return nil, true, failed(err)
	}
	return st, true, nil
}
